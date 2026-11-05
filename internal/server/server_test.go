package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

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
	rec := get(t, Handler(fakeDB{err: errors.New("down")}), "/healthz")
	if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}

func TestReadyzNeedsTheDatabase(t *testing.T) {
	if rec := get(t, Handler(fakeDB{err: errors.New("down")}), "/readyz"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("db down: got %d, want 503", rec.Code)
	}
	if rec := get(t, Handler(fakeDB{}), "/readyz"); rec.Code != http.StatusOK {
		t.Fatalf("db up: got %d, want 200", rec.Code)
	}
}

func TestReadyzAgainstRealPostgres(t *testing.T) {
	pool := dbtest.New(t)
	if rec := get(t, Handler(pool), "/readyz"); rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestDashboardPlaceholder(t *testing.T) {
	if rec := get(t, Handler(fakeDB{}), "/"); rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
}
