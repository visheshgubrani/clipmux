// Package server wires `clipmux serve`: the integration API, the worker API,
// the dashboard and built-in delivery on one HTTP listener.
package server

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/assets"
	"github.com/visheshgubrani/clipmux/internal/config"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/sweeper"
	"github.com/visheshgubrani/clipmux/internal/uploads"
	"github.com/visheshgubrani/clipmux/internal/web"
	"github.com/visheshgubrani/clipmux/internal/workerapi"
)

// LeaseSweepInterval is how often lost leases are swept. Leases last 90 s and
// workers heartbeat every 15 s.
const LeaseSweepInterval = 15 * time.Second

// UploadSweepInterval is how often abandoned uploads are checked.
const UploadSweepInterval = 5 * time.Minute

// Registrar adds an API's routes to the root mux.
type Registrar interface {
	Register(mux *http.ServeMux)
}

// Pinger reports whether the database is reachable.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Deps are the parts the root handler routes to.
type Deps struct {
	DB Pinger
	// Worker serves /worker/v1; nil leaves those routes unmounted.
	Worker http.Handler
	// Storage serves signed filesystem-storage URLs under /_storage/.
	Storage http.Handler
	// APIs register the integration API routes (/v1/...).
	APIs []Registrar
}

// Handler returns the root HTTP handler. /healthz reports the process is up;
// /readyz also requires the database.
func Handler(d Deps) http.Handler {
	mux := http.NewServeMux()
	if d.Worker != nil {
		// Every worker call is a POST; the worker API routes within it.
		mux.Handle("POST /worker/v1/", d.Worker)
	}
	if d.Storage != nil {
		for _, method := range []string{"GET", "PUT", "OPTIONS"} {
			mux.Handle(method+" /_storage/", d.Storage)
		}
	}
	for _, api := range d.APIs {
		api.Register(mux)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		text(w, http.StatusOK, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := d.DB.Ping(ctx); err != nil {
			text(w, http.StatusServiceUnavailable, "database unavailable")
			return
		}
		text(w, http.StatusOK, "ok")
	})
	mux.Handle("GET /", http.FileServerFS(web.Dist()))
	return mux
}

func text(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body + "\n"))
}

// Run connects to Postgres, applies pending migrations, opens storage and
// serves until ctx is cancelled.
func Run(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	logger.Info("database ready")

	// v1 starts on filesystem storage; per-asset storage configs and the
	// R2/S3 adapters arrive in Phase 2.
	store, err := storage.OpenFilesystem(filepath.Join(cfg.DataDir, "storage"), storage.FilesystemOptions{BaseURL: cfg.PublicURL})
	if err != nil {
		return err
	}
	defer store.Close()
	logger.Info("storage ready", "kind", "fs", "public_url", cfg.PublicURL)

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	return serve(ctx, cfg, ln, logger, pool, store)
}

// serve runs the HTTP server and background loops. It returns only after
// every goroutine it started has stopped, so the caller can close the pool
// and storage.
func serve(ctx context.Context, cfg config.Config, ln net.Listener, logger *slog.Logger, pool *pgxpool.Pool, store *storage.Filesystem) error {
	// Until the owner table exists (P1-T10) every boot is a first boot.
	logger.Info("first boot: create the owner account with this setup token", "setup_token", rand.Text())

	q := queue.New(pool)
	// The built-in worker's key lives only in memory and is handed to the
	// child worker (P1-T06); it is never logged or stored.
	worker := workerapi.New(q, workerapi.BuiltinKey(rand.Text()))
	worker.Logger = logger
	upl := uploads.New(pool, store)
	upl.Logger = logger
	if cfg.MaxUploadBytes > 0 {
		upl.MaxSize = cfg.MaxUploadBytes
	}

	ctx, stop := context.WithCancel(ctx)
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() {
		sweeper.Every(ctx, LeaseSweepInterval, "lost_leases", func(ctx context.Context) (int, error) {
			return q.SweepLostLeases(ctx, 100)
		}, logger)
	})
	wg.Go(func() {
		sweeper.Every(ctx, UploadSweepInterval, "uploads", func(ctx context.Context) (int, error) {
			return upl.Sweep(ctx, 500)
		}, logger)
	})

	srv := &http.Server{
		Handler: Handler(Deps{
			DB: pool, Worker: worker.Handler(), Storage: store.Handler(),
			APIs: []Registrar{upl, assets.New(pool)},
		}),
		ReadHeaderTimeout: 10 * time.Second,
		// Requests inherit ctx, so long-poll claims end at shutdown.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	logger.Info("clipmux serve listening", "addr", ln.Addr().String())

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case err := <-errCh:
		return err // stop and wg.Wait run via defer
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
