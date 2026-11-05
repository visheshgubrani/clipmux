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
	h := Handler(built)
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
	h := Handler(built)
	for _, p := range []string{"/v1/nope", "/v/tok/pid/x", "/p/pid", "/_storage/x", "/worker/v1/x", "/_tls/x", "/healthz"} {
		rec := get(h, p)
		if rec.Code != 404 || !strings.Contains(rec.Body.String(), `"not_found"`) {
			t.Errorf("GET %s = %d %q, want a JSON 404", p, rec.Code, rec.Body.String())
		}
	}
}

func TestPlaceholderWithoutABuild(t *testing.T) {
	rec := get(Handler(placeholder{}), "/library")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "built without the dashboard") {
		t.Fatalf("no build: %d %q", rec.Code, rec.Body.String())
	}
}
