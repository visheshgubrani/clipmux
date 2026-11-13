package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/config"
	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/outbox"
	"github.com/visheshgubrani/clipmux/internal/sandbox"
	"github.com/visheshgubrani/clipmux/internal/secrets"
	"github.com/visheshgubrani/clipmux/internal/settings"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/webhooks"
)

type fakeDB struct{ err error }

func testRing(t *testing.T) contentkey.Ring {
	t.Helper()
	s, err := contentkey.FromHex(1, strings.Repeat("00", 32))
	if err != nil {
		t.Fatal(err)
	}
	return contentkey.NewRing(s)
}

func testBoot(t *testing.T, pool *pgxpool.Pool, store *storage.Filesystem, box *secrets.Box) boot {
	t.Helper()
	urls, err := settings.Load(context.Background(), pool, "prj_default", config.Config{PublicURL: config.DefaultPublicURL, MediaURL: config.DefaultPublicURL})
	if err != nil {
		t.Fatal(err)
	}
	return boot{pool: pool, store: store, keys: testRing(t), box: box, urls: urls}
}

func testBox(t *testing.T) *secrets.Box {
	t.Helper()
	b, err := secrets.NewBox(1, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (f fakeDB) Ping(context.Context) error { return f.err }

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestHealthz(t *testing.T) {
	rec := get(t, Handler(Deps{DB: fakeDB{err: errors.New("down")}}), "/healthz")
	if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}

func TestReadyzNeedsTheDatabase(t *testing.T) {
	if rec := get(t, Handler(Deps{DB: fakeDB{err: errors.New("down")}}), "/readyz"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("db down: got %d, want 503", rec.Code)
	}
	if rec := get(t, Handler(Deps{DB: fakeDB{}}), "/readyz"); rec.Code != http.StatusOK {
		t.Fatalf("db up: got %d, want 200", rec.Code)
	}
}

func TestReadyzAgainstRealPostgres(t *testing.T) {
	pool := dbtest.New(t)
	if rec := get(t, Handler(Deps{DB: pool}), "/readyz"); rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestDashboardPlaceholder(t *testing.T) {
	if rec := get(t, Handler(Deps{DB: fakeDB{}}), "/"); rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestWorkerRoutesAreMounted(t *testing.T) {
	worker := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	rec := httptest.NewRecorder()
	Handler(Deps{DB: fakeDB{}, Worker: worker}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/worker/v1/claim", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("got %d, want the worker handler", rec.Code)
	}
}

func TestReadyzFailsWhileDraining(t *testing.T) {
	drain, startDrain := context.WithCancel(context.Background())
	h := Handler(Deps{DB: fakeDB{}, Drain: drain})
	if rec := get(t, h, "/readyz"); rec.Code != http.StatusOK {
		t.Fatalf("before the drain: %d", rec.Code)
	}
	startDrain()
	if rec := get(t, h, "/readyz"); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "draining") {
		t.Fatalf("draining: %d %q", rec.Code, rec.Body.String())
	}
	if rec := get(t, h, "/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("healthz must stay up while draining: %d", rec.Code)
	}
}

func TestStorageRoutesAreMountedBesideTheDashboard(t *testing.T) {
	store := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := Handler(Deps{DB: fakeDB{}, Storage: store})
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodOptions} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/_storage/objects/uploads/a/source", nil))
		if rec.Code != http.StatusTeapot {
			t.Errorf("%s /_storage/: got %d, want the storage handler", method, rec.Code)
		}
	}
	if rec := get(t, h, "/"); rec.Code != http.StatusOK {
		t.Fatalf("dashboard: got %d", rec.Code)
	}
}

func TestServeStopsCleanlyOnShutdown(t *testing.T) {
	pool := dbtest.New(t)
	store, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: "http://localhost"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, config.Config{}, ln, slog.New(slog.NewTextHandler(io.Discard, nil)), testBoot(t, pool, store, testBox(t)))
	}()

	url := "http://" + ln.Addr().String() + "/readyz"
	for i := 0; ; i++ {
		res, err := http.Get(url)
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				break
			}
		}
		if i == 50 {
			t.Fatalf("server never became ready: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v, want nil on shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after shutdown")
	}
}

func TestServeReturnsWhenTheListenerFails(t *testing.T) {
	pool := dbtest.New(t)
	store, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: "http://localhost"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close() // Serve fails at once; the background loops must still stop.
	done := make(chan error, 1)
	go func() {
		done <- serve(context.Background(), config.Config{}, ln, slog.New(slog.NewTextHandler(io.Discard, nil)), testBoot(t, pool, store, testBox(t)))
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("serve returned nil, want the listener's error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve hung after its listener failed")
	}
}

func TestServeDeliversWebhooks(t *testing.T) {
	pool := dbtest.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	box := testBox(t)
	got := make(chan string, 1)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- r.Header.Get("X-Webhook-Event"):
		default:
		}
	}))
	defer receiver.Close()
	if _, _, err := webhooks.CreateEndpoint(ctx, db.New(pool), box, "prj_default", "app_dashboard", receiver.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default')`); err != nil {
		t.Fatal(err)
	}
	if err := outbox.AssetEvent(ctx, db.New(pool), "ast_1", outbox.AssetReady, nil); err != nil {
		t.Fatal(err)
	}
	store, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: "http://localhost"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, config.Config{}, ln, slog.New(slog.NewTextHandler(io.Discard, nil)), testBoot(t, pool, store, box))
	}()
	select {
	case event := <-got:
		if event != "video.asset.ready" {
			t.Fatalf("delivered %q, want video.asset.ready", event)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve never delivered the queued event")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestLoopbackURL(t *testing.T) {
	cases := map[string]string{
		"0.0.0.0:8080":     "http://127.0.0.1:8080",
		"[::]:8080":        "http://127.0.0.1:8080",
		"127.0.0.1:9000":   "http://127.0.0.1:9000",
		"192.168.1.5:8080": "http://192.168.1.5:8080", // listening on one interface only
	}
	for in, want := range cases {
		addr, err := net.ResolveTCPAddr("tcp", in)
		if err != nil {
			t.Fatal(err)
		}
		if got := loopbackURL(addr); got != want {
			t.Errorf("loopbackURL(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestBuiltinWorkerEnvironmentIsMinimal(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://secret")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	b := &builtinWorker{server: "http://127.0.0.1:8080", key: "k", tools: config.Config{}.Tools}
	for _, kv := range b.childEnv() {
		if strings.Contains(kv, "secret") {
			t.Fatalf("the child inherited %q", kv)
		}
	}
	if args := strings.Join(b.args(), " "); !strings.Contains(args, "--storage-via-server") || !strings.Contains(args, "--sandbox") {
		t.Fatalf("child args %q: the built-in worker must reach storage over loopback and confine its tools", args)
	}
	b.unsandboxed = true
	if args := strings.Join(b.args(), " "); strings.Contains(args, "--sandbox") {
		t.Fatalf("child args %q with CLIPMUX_UNSANDBOXED_WORKER: no --sandbox", args)
	}
}

func TestCaddyAskAllowsOnlyConfiguredHostnames(t *testing.T) {
	h := Handler(Deps{DB: fakeDB{}, ServesHost: func(host string) bool { return host == "video.example.com" }})
	for path, want := range map[string]int{
		"/_tls/ask?domain=video.example.com": http.StatusOK,
		"/_tls/ask?domain=evil.example.com":  http.StatusNotFound,
		"/_tls/ask":                          http.StatusNotFound,
	} {
		if rec := get(t, h, path); rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
	}
	if rec := get(t, Handler(Deps{DB: fakeDB{}}), "/_tls/ask?domain=video.example.com"); rec.Code != http.StatusNotFound {
		t.Errorf("with no settings every name is refused, got %d", rec.Code)
	}
}

// Until the owner exists every boot prints a setup token; after that, none.
func TestSetupTokenIsPrintedOnlyWhileNoOwnerExists(t *testing.T) {
	pool := dbtest.New(t)
	boot := func() string {
		t.Helper()
		store, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: "http://localhost"})
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		var logs strings.Builder
		var mu sync.Mutex
		logger := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
			mu.Lock()
			defer mu.Unlock()
			return logs.Write(p)
		}), nil))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- serve(ctx, config.Config{}, ln, logger, testBoot(t, pool, store, testBox(t))) }()
		for i := 0; ; i++ {
			if res, err := http.Get("http://" + ln.Addr().String() + "/healthz"); err == nil {
				res.Body.Close()
				break
			}
			if i == 100 {
				t.Fatal("serve never answered")
			}
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		return logs.String()
	}
	if logs := boot(); !strings.Contains(logs, "setup_token=") {
		t.Fatalf("a fresh install must print its setup token:\n%s", logs)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO owners (id, project_id, email, password_hash) VALUES ('own_1', 'prj_default', 'o@example.com', 'x')`); err != nil {
		t.Fatal(err)
	}
	if logs := boot(); strings.Contains(logs, "setup_token") {
		t.Fatalf("with an owner, no setup token may be printed:\n%s", logs)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// Without Landlock a tool reads any file serve's user can, including the
// data directory's keys (finding 1): the built-in worker stays off unless
// the owner opted out with CLIPMUX_UNSANDBOXED_WORKER.
func TestBuiltinWorkerRequiresLandlock(t *testing.T) {
	enforced := sandbox.Status{Rlimits: true, NoNewPrivs: true, Seccomp: true}
	withLandlock := enforced
	withLandlock.LandlockABI = 4
	for _, c := range []struct {
		name        string
		st          sandbox.Status
		err         error
		unsandboxed bool
		want        bool
	}{
		{"landlock", withLandlock, nil, false, true},
		{"no landlock", enforced, nil, false, false},
		{"no landlock, opted out", enforced, nil, true, true},
		{"no sandbox", sandbox.Status{}, errors.New("only none holds"), false, false},
		{"no sandbox, opted out", sandbox.Status{}, errors.New("only none holds"), true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := &builtinWorker{logger: slog.New(slog.DiscardHandler), unsandboxed: c.unsandboxed,
				probe: func(context.Context, string) (sandbox.Status, error) { return c.st, c.err }}
			if got := b.sandboxAllows(t.Context(), "clipmux"); got != c.want {
				t.Fatalf("sandboxAllows = %v, want %v", got, c.want)
			}
			// Opting out for want of Landlock keeps seccomp and the limits.
			if confined := strings.Contains(strings.Join(b.args(), " "), "--sandbox"); c.want && confined != (c.err == nil) {
				t.Fatalf("args %q: --sandbox = %v, want %v", b.args(), confined, c.err == nil)
			}
		})
	}
}
