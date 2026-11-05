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
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/config"
	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/storage"
)

type fakeDB struct{ err error }

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
		done <- serve(ctx, config.Config{}, ln, slog.New(slog.NewTextHandler(io.Discard, nil)), pool, store, contentkey.Secret{Version: 1})
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
}
