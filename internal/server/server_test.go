package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
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

func TestServeStopsCleanlyOnShutdown(t *testing.T) {
	pool := dbtest.New(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, ln, slog.New(slog.NewTextHandler(io.Discard, nil)), pool) }()

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
