// Package server wires `clipmux serve`: the integration API, the worker API,
// the dashboard and built-in delivery on one HTTP listener.
package server

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/accounts"
	"github.com/visheshgubrani/clipmux/internal/agents"
	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/assets"
	"github.com/visheshgubrani/clipmux/internal/cloudflare"
	"github.com/visheshgubrani/clipmux/internal/config"
	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/delivery"
	"github.com/visheshgubrani/clipmux/internal/dispatch"
	"github.com/visheshgubrani/clipmux/internal/lifecycle"
	"github.com/visheshgubrani/clipmux/internal/manage"
	"github.com/visheshgubrani/clipmux/internal/modal"
	"github.com/visheshgubrani/clipmux/internal/outputs"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/retention"
	"github.com/visheshgubrani/clipmux/internal/routes"
	"github.com/visheshgubrani/clipmux/internal/runpod"
	"github.com/visheshgubrani/clipmux/internal/secrets"
	"github.com/visheshgubrani/clipmux/internal/settings"
	"github.com/visheshgubrani/clipmux/internal/setuptest"
	"github.com/visheshgubrani/clipmux/internal/signingkeys"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/stores"
	"github.com/visheshgubrani/clipmux/internal/sweeper"
	"github.com/visheshgubrani/clipmux/internal/tracks"
	"github.com/visheshgubrani/clipmux/internal/uploads"
	"github.com/visheshgubrani/clipmux/internal/web"
	"github.com/visheshgubrani/clipmux/internal/webhooks"
	"github.com/visheshgubrani/clipmux/internal/workerapi"
)

// LeaseSweepInterval is how often lost leases are swept. Leases last 90 s and
// workers heartbeat every 15 s.
const LeaseSweepInterval = 15 * time.Second

// RouteInterval is how often queued media jobs are routed again (D053): a
// primary seen offline sends work to the fallback within OnlineWithin plus
// this.
const RouteInterval = 5 * time.Second

// DispatchInterval is how often pending wakes and stops are sent to
// providers.
const DispatchInterval = 2 * time.Second

// ReapInterval is how often dispatches whose machine has nothing left to do
// are ended and their machines stopped (D055).
const ReapInterval = 30 * time.Second

// UploadSweepInterval is how often abandoned uploads are checked.
const UploadSweepInterval = 5 * time.Minute

// RouteSyncInterval is how often pending route publications are retried;
// commits publish inline, so this only matters after a crash or an error.
const RouteSyncInterval = 15 * time.Second

// ReconcileInterval is how often the Worker's route records are compared
// with Postgres and repaired (docs/architecture.md, "Route records").
const ReconcileInterval = 15 * time.Minute

// WebhookInterval is how often new events fan out and due deliveries are
// sent; an event reaches its endpoints within about this long.
const WebhookInterval = 5 * time.Second

// ControlJobInterval is how often control-plane jobs (asset deletes) are
// run; DELETE runs its job inline, so this mostly covers crashes, pending
// unpublishes and second passes.
const ControlJobInterval = 15 * time.Second

// RetentionInterval is how often rows past their windows are pruned
// (D069).
const RetentionInterval = time.Hour

// SetupTestInterval is how often a queued setup test is picked up.
const SetupTestInterval = 5 * time.Second

// BuiltinStopWait is how long shutdown waits for the built-in worker to hand
// its attempt back before it stops serving.
const BuiltinStopWait = 20 * time.Second

// ShutdownWait is how long requests in flight get to finish at shutdown.
// With BuiltinStopWait it fits compose's 40 s stop grace period.
const ShutdownWait = 15 * time.Second

// passTimeouts bound one pass of each background loop, so a provider or
// storage call that hangs can't stall its loop forever. Loops missing here
// have no bound: control jobs and imports run work that its own lease
// bounds (a move or a 6-hour fetch).
var passTimeouts = map[string]time.Duration{
	"lost_leases":          time.Minute,
	"routing":              time.Minute,
	"dispatch":             5 * time.Minute, // provider calls, 60 s each
	"dispatch_reaper":      2 * time.Minute,
	"machine_reconciler":   5 * time.Minute,
	"stranded_pinned_jobs": time.Minute,
	"speech_wait":          time.Minute,
	"uploads":              5 * time.Minute,
	"routes":               5 * time.Minute,  // storage writes for pending route records
	"route_reconciler":     30 * time.Minute, // lists every route record
	"webhooks":             5 * time.Minute,  // 10 s per delivery, 8 at a time
	"setup_tests":          30 * time.Minute, // waits up to 15 minutes for its encode
	"retention":            10 * time.Minute,
	"file_retention":       30 * time.Minute, // deletes whole output prefixes
}

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
	// ServesHost answers Caddy's on-demand TLS check (/_tls/ask): whether
	// a certificate may be obtained for a hostname. nil refuses every name.
	ServesHost func(host string) bool
	// CSP is the dashboard's Content-Security-Policy for a request; nil
	// allows only the server's own origin.
	CSP func(*http.Request) string
	// Drain is done once the server starts shutting down; /readyz then
	// fails so a load balancer stops sending new work. nil never drains.
	Drain context.Context
}

// Handler returns the root HTTP handler. /healthz reports the process is up;
// /readyz also requires the database.
func Handler(d Deps) http.Handler {
	mux := http.NewServeMux()
	if d.Worker != nil {
		// Every worker call is a POST but a transcription's audio; the
		// worker API routes within it.
		mux.Handle("POST /worker/v1/", d.Worker)
		mux.Handle("GET /worker/v1/attempts/", d.Worker)
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
		if d.Drain != nil && d.Drain.Err() != nil {
			text(w, http.StatusServiceUnavailable, "draining")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := d.DB.Ping(ctx); err != nil {
			text(w, http.StatusServiceUnavailable, "database unavailable")
			return
		}
		text(w, http.StatusOK, "ok")
	})
	// Caddy asks before obtaining a certificate, so the Caddyfile needs no
	// hostnames: only the configured public and media hostnames get one.
	mux.HandleFunc("GET /_tls/ask", func(w http.ResponseWriter, r *http.Request) {
		if d.ServesHost == nil || !d.ServesHost(r.URL.Query().Get("domain")) {
			text(w, http.StatusNotFound, "not a configured hostname")
			return
		}
		text(w, http.StatusOK, "ok")
	})
	mux.Handle("GET /", web.Handler(web.Dist(), d.CSP))
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

	// The key that seals secrets in Postgres; serve refuses to start with one
	// that can't open what the database already holds.
	box, err := secrets.LoadOrCreate(ctx, db.New(pool), db.DefaultProjectID, cfg.DataDir, cfg.SecretsKey, cfg.SecretsKeyPrevious)
	if err != nil {
		return err
	}
	// Every sealed value must open with the key or the previous one: a new
	// key without the old beside it, before reseal-secrets, stops here and
	// names what it couldn't open (D071).
	if err := secrets.CheckVersions(ctx, pool, box); err != nil {
		return err
	}

	// The public and media URLs: pinned by the environment, or chosen by the
	// owner at first boot.
	urls, err := settings.Load(ctx, pool, db.DefaultProjectID, cfg)
	if err != nil {
		return err
	}

	// v1 starts on filesystem storage; per-asset storage configs and the
	// R2/S3 adapters arrive in Phase 2.
	store, err := storage.OpenFilesystem(filepath.Join(cfg.DataDir, "storage"), storage.FilesystemOptions{BaseURL: urls.PublicURL()})
	if err != nil {
		return err
	}
	defer store.Close()
	urls.OnChange(func(u settings.URLs) { store.SetBaseURL(u.Public) })
	logger.Info("storage ready", "kind", "fs", "public_url", urls.PublicURL(), "media_url", urls.MediaURL(), "pinned", urls.URLs().Pinned)

	// The content key secrets, sealed in Postgres; losing them makes every
	// output unplayable.
	keys, err := contentkey.Load(ctx, pool, box, db.DefaultProjectID, cfg.DataDir)
	if err != nil {
		return err
	}
	// The server's own token-signing key (dashboard preview, setup tests).
	if err := signingkeys.EnsureInternal(ctx, pool, box, db.DefaultProjectID); err != nil {
		return err
	}

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	return serve(ctx, cfg, ln, logger, boot{pool: pool, store: store, keys: keys, box: box, urls: urls})
}

// boot is what Run opens before serving.
type boot struct {
	pool  *pgxpool.Pool
	store *storage.Filesystem
	keys  contentkey.Ring
	box   *secrets.Box
	urls  *settings.Store
}

// serve runs the HTTP server and background loops. It returns only after
// every goroutine it started has stopped, so the caller can close the pool
// and storage.
func serve(ctx context.Context, cfg config.Config, ln net.Listener, logger *slog.Logger, b boot) error {
	// drain is done when serve is asked to stop. Shutting down then runs in
	// order: stop handing out work, let the built-in worker hand its attempt
	// back, finish requests in flight, and only then stop the loops.
	drain := ctx
	pool, store, keys, box := b.pool, b.store, b.keys, b.box
	// Each asset's store: the filesystem, or an R2/S3 config (P2-T01).
	set := stores.New(pool, box, store)
	acct := accounts.New(pool, db.DefaultProjectID, b.urls.PublicURL)
	acct.TrustedProxies = cfg.TrustedProxies
	acct.Box = box
	acct.Logger = logger
	// Until the owner registers, every boot prints a fresh setup token; the
	// first visitor needs it to create the owner account.
	if token, open, err := acct.SetupToken(ctx); err != nil {
		return err
	} else if open {
		logger.Info("first boot: open the dashboard and create the owner account with this setup token",
			"url", b.urls.PublicURL(), "setup_token", token)
	}

	q := queue.New(pool)
	q.KeyVersion = func() int32 { return keys.Current().Version }
	committer := outputs.New(pool, q, set)
	committer.Logger = logger
	// The built-in worker's key lives only in memory and is handed to the
	// child worker; it is never logged or stored.
	workerKey := rand.Text()
	// PC agents pair with a code and claim with their own keys (P3-T01).
	agentSvc := agents.New(pool, q, acct, db.DefaultProjectID, b.urls.PublicURL)
	agentSvc.Logger = logger
	// Serverless machines claim one job each with a key minted when the
	// router sends that job to a provider (P4-T02).
	disp := dispatch.New(pool, box, b.urls.PublicURL)
	disp.Logger = logger
	q.Dispatch = disp.Route
	worker := workerapi.New(q, workerapi.Auths{workerapi.BuiltinKey(workerKey), agentSvc, disp}, set, keys, committer)
	worker.Agents = agentSvc
	worker.Logger = logger
	worker.Drain = drain
	worker.Tracks = &tracks.Publisher{Stores: set, Keys: keys}
	upl := uploads.New(pool, set)
	upl.Logger = logger
	playback := delivery.New(pool, set, keys)
	playback.Logger = logger
	committer.OnPublished, committer.OnKeys = playback.Invalidate, playback.InvalidateKeys
	if cfg.MaxUploadBytes > 0 {
		upl.MaxSize = cfg.MaxUploadBytes
	}
	hooks := webhooks.New(pool, box)
	hooks.Logger = logger
	life := lifecycle.New(pool, set, committer)
	life.Logger = logger
	agentSvc.Life = life
	mgmt := manage.New(pool, box, acct, b.urls)
	mgmt.Logger = logger
	tests := setuptest.New(pool, set, box, b.urls, upl, life, cfg.SelfDial)
	tests.Logger = logger
	mgmt.Tests = tests
	mgmt.Life = life
	mgmt.Keys = &manage.ContentKeys{Ring: keys, DataDir: cfg.DataDir}
	mgmt.Cloudflare = &cloudflare.Connector{
		Pool: pool, Stores: set, Keys: keys, URLs: b.urls, ProjectID: db.DefaultProjectID,
		API: cfg.CloudflareAPI, R2Endpoint: cloudflare.EndpointTemplate(cfg.R2Endpoint),
		SetupTest: func(ctx context.Context) error { _, err := tests.Queue(ctx, "urls_saved"); return err },
	}
	// Providers (P4-T04): each kind opens its adapter from its stored row.
	rp := &runpod.Connector{
		Pool: pool, Box: box, ProjectID: db.DefaultProjectID, API: cmp.Or(cfg.RunPodAPI, runpod.DefaultAPI),
		PublicURL: b.urls.PublicURL, AllowPrivateURL: cfg.ExecutorsAllowPrivateURL, DefaultImage: cfg.WorkerImage,
		Resolver: net.DefaultResolver, Logger: logger,
		SetupTest: func(ctx context.Context) error {
			_, err := tests.QueueThrough(ctx, "executor_connected", "runpod")
			return err
		},
	}
	md := &modal.Connector{
		Pool: pool, Box: box, ProjectID: db.DefaultProjectID,
		PublicURL: b.urls.PublicURL, AllowPrivateURL: cfg.ExecutorsAllowPrivateURL, DefaultImage: cfg.WorkerImage,
		Resolver: net.DefaultResolver, Logger: logger,
		SetupTest: func(ctx context.Context) error {
			_, err := tests.QueueThrough(ctx, "executor_connected", "modal")
			return err
		},
	}
	disp.Resolve = func(_ context.Context, row db.ExecutorConfig) (dispatch.Executor, error) {
		switch row.Kind {
		case "runpod":
			return rp.Resolve(row)
		case "modal":
			return md.Resolve(row)
		}
		return nil, fmt.Errorf("no adapter for provider %q", row.Kind)
	}
	mgmt.RunPod, mgmt.Modal = rp, md
	assetsAPI := assets.New(pool)
	assetsAPI.Drain = drain
	// The dashboard calls the integration API with the owner's session,
	// acting as one app; integration keys never reach the management API.
	// One set of per-key limits, so a key's budget is the same whichever
	// API it calls (D067).
	apiLimits := apikeys.NewLimits()
	for _, auth := range []*apikeys.Authenticator{upl.Auth, life.Auth, assetsAPI.Auth} {
		auth.Session = mgmt.ActAsApp
		auth.Limits = apiLimits
	}

	// Defers run last in, first out: stop cancels the loops before wg.Wait
	// waits for them, whichever way serve returns.
	var wg sync.WaitGroup
	defer wg.Wait()
	// The loops outlive the drain: a commit landing while requests finish
	// still publishes its routes.
	ctx, stop := context.WithCancel(context.WithoutCancel(drain))
	defer stop()
	// One replica runs the background loops (an advisory lock); the others
	// serve requests. Every loop is safe to run twice, so a handover costs
	// duplicate work at most.
	leader := sweeper.NewLeader(pool, logger)
	wg.Go(func() { leader.Run(ctx) })
	leader.Await(ctx, 2*time.Second)
	obs := newObserver(pool, leader, cfg.Version)
	loop := func(interval time.Duration, name string, task sweeper.Task) {
		wg.Go(func() {
			sweeper.Every(ctx, interval, passTimeouts[name], name, leader.Only(obs.loop(name, task)), logger)
		})
	}
	loop(LeaseSweepInterval, "lost_leases", func(ctx context.Context) (int, error) { return q.SweepLostLeases(ctx, 100) })
	loop(RouteInterval, "routing", func(ctx context.Context) (int, error) { return q.Route(ctx, 200) })
	loop(DispatchInterval, "dispatch", func(ctx context.Context) (int, error) { return disp.Publish(ctx, 20) })
	loop(ReapInterval, "dispatch_reaper", func(ctx context.Context) (int, error) { return disp.Reap(ctx, 100) })
	// The first pass runs at boot, which is when a crash may have left a
	// machine nobody recorded.
	loop(ReconcileInterval, "machine_reconciler", disp.Reconcile)
	loop(UploadSweepInterval, "stranded_pinned_jobs", func(ctx context.Context) (int, error) { return q.SweepStrandedPinned(ctx, 100) })
	loop(UploadSweepInterval, "speech_wait", func(ctx context.Context) (int, error) { return q.SweepStaleTranscriptions(ctx, 100) })
	loop(UploadSweepInterval, "uploads", func(ctx context.Context) (int, error) { return upl.Sweep(ctx, 500) })
	loop(RouteSyncInterval, "routes", func(ctx context.Context) (int, error) { return committer.SyncRoutes(ctx, 1000) })
	loop(ReconcileInterval, "route_reconciler", func(ctx context.Context) (int, error) {
		return routes.ReconcileProject(ctx, db.New(pool), set, db.DefaultProjectID)
	})
	loop(WebhookInterval, "webhooks", hooks.Run)
	loop(ControlJobInterval, "control_jobs", life.Run)
	loop(ControlJobInterval, "imports", life.RunImports)
	loop(SetupTestInterval, "setup_tests", tests.Run)
	prune := retention.New(pool)
	prune.Logger = logger
	loop(RetentionInterval, "retention", prune.Run)
	files := retention.NewFiles(pool, set, store)
	files.Logger = logger
	loop(RetentionInterval, "file_retention", files.Run)
	// Saving the URLs re-runs the setup test: a wrong hostname should fail
	// here, not in an app.
	mgmt.URLsSaved = func(settings.URLs) {
		if _, err := tests.Queue(ctx, "urls_saved"); err != nil {
			logger.Error("queue setup test", "err", err)
		}
	}
	// With the built-in worker the sample can encode right away, so a fresh
	// install proves its path on first boot.
	if cfg.BuiltinWorker {
		if err := tests.QueueFirst(ctx); err != nil {
			return err
		}
	}
	var builtinDone <-chan struct{} // nil: no built-in worker to wait for
	if cfg.BuiltinWorker {
		bw := &builtinWorker{
			server: loopbackURL(ln.Addr()), key: workerKey, workDir: workerDir(cfg.DataDir),
			modelsDir: filepath.Join(cfg.DataDir, "models"),
			tools:     cfg.Tools, logger: logger, unsandboxed: cfg.UnsandboxedWorker,
		}
		bwCtx, bwStop := context.WithCancel(drain)
		defer bwStop()
		bwDone := make(chan struct{})
		wg.Go(func() { defer close(bwDone); bw.run(bwCtx) })
		builtinDone = bwDone
	}

	// Requests don't inherit drain: one in flight at the signal finishes.
	reqCtx, cancelRequests := context.WithCancel(context.WithoutCancel(drain))
	defer cancelRequests()
	reqLog := requestLog{logger: logger, trusted: cfg.TrustedProxies, clientIP: acct.ClientIP, obs: obs}
	secure := transportSecurity{publicURL: b.urls.PublicURL, mediaURL: b.urls.MediaURL, trusted: cfg.TrustedProxies}
	csp := func(r *http.Request) string {
		origins, err := set.Origins(r.Context(), db.DefaultProjectID)
		if err != nil {
			logger.WarnContext(r.Context(), "dashboard CSP without storage origins", "err", err)
		}
		return dashboardCSP(b.urls.MediaURL(), origins)
	}
	srv := &http.Server{
		Handler: reqLog.wrap(secure.wrap(newRefusals(acct.ClientIP).wrap(defaultDeadlines.wrap(Handler(Deps{
			DB: pool, Worker: worker.Handler(), Storage: store.Handler(), ServesHost: b.urls.ServesHost, Drain: drain, CSP: csp,
			APIs: []Registrar{acct, mgmt, agentSvc, upl, assetsAPI, life, playback},
		}))))),
		// The server's own errors (TLS, a panicking handler) go to the same log.
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		BaseContext:       func(net.Listener) context.Context { return reqCtx },
	}
	// Metrics and pprof listen on an address of their own, never the
	// public one (D066).
	if cfg.MetricsAddr != "" {
		mln, err := net.Listen("tcp", cfg.MetricsAddr)
		if err != nil {
			return fmt.Errorf("metrics listener: %w", err)
		}
		msrv := &http.Server{Handler: obs.Handler(cfg.PProf), ReadHeaderTimeout: 10 * time.Second}
		defer msrv.Close() // runs before wg.Wait; the error is the listener's
		wg.Go(func() { _ = msrv.Serve(mln) })
		logger.Info("metrics listening", "addr", mln.Addr().String(), "pprof", cfg.PProf)
	}
	logger.Info("clipmux serve listening", "addr", ln.Addr().String())

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case err := <-errCh:
		return err // the deferred stop ends the loops, then wg.Wait waits for them
	case <-drain.Done():
	}
	logger.Info("shutting down: draining")
	// The built-in worker got SIGINT with the drain and hands its attempt
	// back through this server, which must still be listening.
	if builtinDone != nil {
		select {
		case <-builtinDone:
		case <-time.After(BuiltinStopWait):
			logger.Warn("the built-in worker is still stopping; shutting down anyway")
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), ShutdownWait)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("requests still running at the shutdown deadline; closing them", "err", err)
		cancelRequests()
		_ = srv.Close() // the error is the listener's, already closed by Shutdown
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
