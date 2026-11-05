package lifecycle_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/publicnet"
)

// origin serves a video at /video.mp4 and counts requests.
func origin(t *testing.T, status int) (*httptest.Server, *int) {
	t.Helper()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write([]byte("an imported video"))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// localURL names the test origin by hostname: a literal loopback address is
// refused when the import is requested.
func localURL(srv *httptest.Server, path string) string {
	return "http://localhost:" + strconv.Itoa(int(netip.MustParseAddrPort(srv.Listener.Addr().String()).Port())) + path
}

// reachLocal lets imports reach the local test origin, which the production
// client refuses (it is loopback).
func (r *rig) reachLocal() {
	r.svc.Fetch = publicnet.Client(publicnet.Options{Allow: func(netip.Addr) bool { return true }})
}

func (r *rig) runImports() {
	r.t.Helper()
	if _, err := r.svc.RunImports(context.Background()); err != nil {
		r.t.Fatal(err)
	}
}

func TestAnImportedURLBecomesTheSourceAndQueuesATranscode(t *testing.T) {
	r := newRig(t)
	r.reachLocal()
	src, _ := origin(t, http.StatusOK)
	status, view := r.send(http.MethodPost, "/v1/assets", r.key, map[string]any{"url": localURL(src, "/video.mp4"), "passthrough": "lesson-9"}, "imp-1")
	if status != http.StatusCreated || view["status"] != "processing" || view["passthrough"] != "lesson-9" {
		t.Fatalf("POST /v1/assets = %d %v", status, view)
	}
	id := view["id"].(string)
	// A retry with the same key returns the same asset.
	if _, again := r.send(http.MethodPost, "/v1/assets", r.key, map[string]any{"url": localURL(src, "/video.mp4"), "passthrough": "lesson-9"}, "imp-1"); again["id"] != id {
		t.Fatalf("idempotent replay = %v", again["id"])
	}
	r.runImports()
	rc, err := r.store.Get(context.Background(), "uploads/"+id+"/source")
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if got := r.scalar(`SELECT string_agg(kind || ':' || status, ',' ORDER BY created_at) FROM jobs WHERE asset_id = $1`, id); got != "import:succeeded,transcode:queued" {
		t.Fatalf("jobs = %v", got)
	}
}

func TestAPrivateURLIsRefusedBeforeAnythingIsCreated(t *testing.T) {
	r := newRig(t)
	status, body := r.send(http.MethodPost, "/v1/assets", r.key, map[string]any{"url": "http://169.254.169.254/latest/meta-data"}, "")
	if status != http.StatusBadRequest || body["error"].(map[string]any)["code"] != "import_url_refused" {
		t.Fatalf("POST = %d %v", status, body)
	}
	if n := r.scalar(`SELECT count(*) FROM assets`); n != int64(0) {
		t.Fatalf("assets = %v, want none", n)
	}
}

// A hostname passes the request-time check; the production client refuses
// it when it resolves to loopback, and the import fails for good.
func TestAHostnameThatResolvesInsideIsRefusedAtConnect(t *testing.T) {
	r := newRig(t)
	src, hits := origin(t, http.StatusOK)
	status, view := r.send(http.MethodPost, "/v1/assets", r.key, map[string]any{"url": localURL(src, "/video.mp4")}, "")
	if status != http.StatusCreated {
		t.Fatalf("POST = %d %v", status, view)
	}
	r.runImports()
	if *hits != 0 {
		t.Fatal("the import reached a loopback server")
	}
	if got := r.scalar(`SELECT status || '/' || error_code FROM assets WHERE id = $1`, view["id"]); got != "errored/import_url_refused" {
		t.Fatalf("asset = %v", got)
	}
	if ev := r.events(); len(ev) == 0 || ev[len(ev)-1] != "video.asset.errored" {
		t.Fatalf("events = %v", ev)
	}
}

func TestATransientFailureRetriesThenErrors(t *testing.T) {
	r := newRig(t)
	r.reachLocal()
	src, hits := origin(t, http.StatusServiceUnavailable)
	_, view := r.send(http.MethodPost, "/v1/assets", r.key, map[string]any{"url": localURL(src, "/video.mp4")}, "")
	for range 3 {
		r.runImports()
		r.exec(`UPDATE jobs SET available_at = now() WHERE kind = 'import'`)
	}
	if *hits != 3 {
		t.Fatalf("fetched %d times, want 3", *hits)
	}
	if got := r.scalar(`SELECT status || '/' || error_code FROM assets WHERE id = $1`, view["id"]); got != "errored/import_failed" {
		t.Fatalf("asset = %v", got)
	}
}

func TestANotFoundURLFailsAtOnce(t *testing.T) {
	r := newRig(t)
	r.reachLocal()
	src, hits := origin(t, http.StatusNotFound)
	_, view := r.send(http.MethodPost, "/v1/assets", r.key, map[string]any{"url": localURL(src, "/gone.mp4")}, "")
	r.runImports()
	if got := r.scalar(`SELECT status || '/' || error_code FROM assets WHERE id = $1`, view["id"]); *hits != 1 || got != "errored/import_failed" {
		t.Fatalf("hits %d, asset %v", *hits, got)
	}
}
