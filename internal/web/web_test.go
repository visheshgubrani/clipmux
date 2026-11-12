package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

var built = fstest.MapFS{
	"index.html":         {Data: []byte("<!doctype html><div id=root></div>")},
	"assets/app-abc.js":  {Data: []byte("console.log(1)")},
	"assets/app-abc.css": {Data: []byte("body{}")},
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

func TestDeepLinkServesIndexUncached(t *testing.T) {
	h := Handler(built, nil)
	for _, p := range []string{"/", "/library", "/apps/app_1/keys", "/missing.js"} {
		rec := get(h, p)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "id=root") || rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("GET %s = %d %q (cache %q), want index.html uncached", p, rec.Code, rec.Body.String(), rec.Header().Get("Cache-Control"))
		}
	}
	rec := get(h, "/assets/app-abc.js")
	if rec.Code != 200 || rec.Body.String() != "console.log(1)" || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("hashed asset = %d %q %q", rec.Code, rec.Body.String(), rec.Header().Get("Cache-Control"))
	}
}

func TestUnknownAPIPathIsAJSON404NotTheDashboard(t *testing.T) {
	h := Handler(built, nil)
	for _, p := range []string{"/v1/nope", "/v/tok/pid/x", "/p/pid", "/_storage/x", "/worker/v1/x", "/_tls/x", "/healthz", "/metrics", "/debug/pprof/"} {
		rec := get(h, p)
		if rec.Code != 404 || !strings.Contains(rec.Body.String(), `"not_found"`) {
			t.Errorf("GET %s = %d %q, want a JSON 404", p, rec.Code, rec.Body.String())
		}
	}
}

func TestPlaceholderWithoutABuild(t *testing.T) {
	rec := get(Handler(placeholder{}, nil), "/library")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "built without the dashboard") {
		t.Fatalf("no build: %d %q", rec.Code, rec.Body.String())
	}
}

func TestTheSharePageIsStaticWithItsOwnHeaders(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html": {Data: []byte("<!doctype html><div id=root></div>")},
		"share.html": {Data: []byte("<!doctype html><main class=share></main>")},
	}
	mux := http.NewServeMux()
	mux.Handle("GET /s/{token}", Share(fsys, func(*http.Request) string { return "default-src 'self'; frame-ancestors 'none'" }))
	mux.Handle("GET /", Handler(fsys, nil))
	rec := get(mux, "/s/K7QW2M4XRT5VZ3NB6YHC8DJF2A")
	want := map[string]string{
		"Content-Security-Policy": "default-src 'self'; frame-ancestors 'none'",
		"Cache-Control":           "no-store",
		"Referrer-Policy":         "no-referrer",
		"X-Robots-Tag":            "noindex",
		"X-Frame-Options":         "DENY",
		"X-Content-Type-Options":  "nosniff",
	}
	if rec.Code != 200 || rec.Body.String() != "<!doctype html><main class=share></main>" {
		t.Fatalf("share page = %d %q", rec.Code, rec.Body.String())
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	// Anything deeper under /s/ is the server's, never the dashboard.
	if rec := get(mux, "/s/K7QW2M4XRT5VZ3NB6YHC8DJF2A/other"); rec.Code != 404 || !strings.Contains(rec.Body.String(), `"not_found"`) {
		t.Errorf("GET /s/x/other = %d %q, want a JSON 404", rec.Code, rec.Body.String())
	}
	// A binary built without the dashboard has no share page.
	if rec := get(Share(placeholder{}, nil), "/s/x"); rec.Code != 404 {
		t.Errorf("share page without a build = %d, want 404", rec.Code)
	}
}
