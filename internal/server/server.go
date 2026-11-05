// Package server wires `clipmux serve`: the integration API, the worker API,
// the dashboard and built-in delivery on one HTTP listener.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/auth"
	"github.com/visheshgubrani/clipmux/internal/config"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/sweeper"
	"github.com/visheshgubrani/clipmux/internal/workerapi"
	"github.com/visheshgubrani/clipmux/web"
)

// LeaseSweepInterval is how often lost leases are swept. Leases last 90 s and
// workers heartbeat every 15 s.
const LeaseSweepInterval = 15 * time.Second

// Pinger reports whether the database is reachable.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Deps are the parts the root handler routes to.
type Deps struct {
	DB Pinger
	// Worker serves /worker/v1; nil leaves those routes unmounted.
	Worker http.Handler
}

// Handler returns the root HTTP handler. /healthz reports the process is up;
// /readyz also requires the database.
func Handler(d Deps) http.Handler {
	database := d.DB
	mux := http.NewServeMux()
	if d.Worker != nil {
		// Every worker call is a POST; the worker API routes within it.
		mux.Handle("POST /worker/v1/", d.Worker)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		text(w, http.StatusOK, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := database.Ping(ctx); err != nil {
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

// Run connects to Postgres, applies pending migrations and serves until ctx
// is cancelled.
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
	return serve(ctx, cfg, logger, pool)
}

func serve(ctx context.Context, cfg config.Config, logger *slog.Logger, pool *pgxpool.Pool) error {
	// Until the owner table exists (P1-T10) every boot is a first boot.
	logger.Info("first boot: create the owner account with this setup token", "setup_token", auth.NewSetupToken())

	q := queue.New(pool)
	// The built-in worker's key lives only in memory and is handed to the
	// child worker (P1-T06); it is never logged or stored.
	worker := workerapi.New(q, workerapi.BuiltinKey(auth.NewSetupToken()))
	worker.Logger = logger
	go sweeper.Every(ctx, LeaseSweepInterval, "lost_leases", func(ctx context.Context) (int, error) {
		return q.SweepLostLeases(ctx, 100)
	}, logger)

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: Handler(Deps{DB: pool, Worker: worker.Handler()}), ReadHeaderTimeout: 10 * time.Second}
	logger.Info("clipmux serve listening", "addr", ln.Addr().String())

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
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
}
