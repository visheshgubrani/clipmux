package server

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestTheDashboardCSPNamesTheMediaURLAndBuckets(t *testing.T) {
	got := dashboardCSP("https://media.example.net", []string{"https://acct.r2.cloudflarestorage.com", "https://media.example.net"})
	want := "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data: blob: https://media.example.net https://acct.r2.cloudflarestorage.com; " +
		"media-src 'self' blob: https://media.example.net https://acct.r2.cloudflarestorage.com; " +
		"connect-src 'self' https://media.example.net https://acct.r2.cloudflarestorage.com; " +
		"worker-src 'self' blob:; font-src 'self' data:; object-src 'none'; base-uri 'none'; " +
		"frame-ancestors 'none'; form-action 'self'"
	if got != want {
		t.Fatalf("CSP\n got %s\nwant %s", got, want)
	}
	if got := dashboardCSP("", nil); got != "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
		"img-src 'self' data: blob:; media-src 'self' blob:; connect-src 'self'; worker-src 'self' blob:; "+
		"font-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'" {
		t.Fatalf("CSP with no other origins: %s", got)
	}
}

func TestPlainHTTPIsRedirectedOnlyForConfiguredHosts(t *testing.T) {
	public, media := "https://video.example.com", "https://media.example.net"
	sec := transportSecurity{
		publicURL: func() string { return public }, mediaURL: func() string { return media },
		trusted: []netip.Prefix{netip.MustParsePrefix("172.16.0.0/12")},
	}
	h := sec.wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	do := func(host, remote, proto string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/library?x=1", nil)
		req.Host, req.RemoteAddr = host, remote
		if proto != "" {
			req.Header.Set("X-Forwarded-Proto", proto)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	caddy := "172.18.0.3:5000"
	if rec := do("video.example.com", caddy, "http"); rec.Code != 308 || rec.Header().Get("Location") != "https://video.example.com/library?x=1" {
		t.Fatalf("public over http: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := do("media.example.net:80", caddy, "http"); rec.Code != 308 || rec.Header().Get("Location") != "https://media.example.net:80/library?x=1" {
		t.Fatalf("media over http: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := do("video.example.com", caddy, "https"); rec.Code != 204 || rec.Header().Get("Strict-Transport-Security") != "max-age=31536000" {
		t.Fatalf("over https: %d hsts %q", rec.Code, rec.Header().Get("Strict-Transport-Security"))
	}
	for _, c := range []struct{ host, remote, proto string }{
		{"evil.example", caddy, "http"},                // never redirect to another host
		{"video.example.com", "203.0.113.9:1", "http"}, // an untrusted peer's header means nothing
		{"app:8080", caddy, ""},                        // Caddy's /_tls/ask, the built-in worker
	} {
		rec := do(c.host, c.remote, c.proto)
		if rec.Code != 204 || rec.Header().Get("Strict-Transport-Security") != "" {
			t.Errorf("%+v: %d hsts %q", c, rec.Code, rec.Header().Get("Strict-Transport-Security"))
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%+v: no nosniff", c)
		}
	}
	public = "http://localhost:8080" // first boot on plain http: nothing to enforce
	if rec := do("localhost:8080", caddy, "http"); rec.Code != 204 {
		t.Fatalf("http public URL: %d", rec.Code)
	}
}

func TestPlaybackIsLimitedByRefusalsOnly(t *testing.T) {
	f := newRefusals(func(r *http.Request) netip.Addr { return netip.MustParseAddr(r.Header.Get("X-Test-IP")) })
	h := f.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/unknown/master.m3u8") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("#EXTM3U"))
	}))
	do := func(ip, path string) int {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("X-Test-IP", ip)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	// A classroom behind one address: many successes, never limited.
	for range 2000 {
		if code := do("198.51.100.7", "/p/pid_1/master.m3u8"); code != 200 {
			t.Fatalf("a successful playback request got %d", code)
		}
	}
	for i := 1; i <= RefusalsPerWindow; i++ {
		if code := do("203.0.113.66", "/p/unknown/master.m3u8"); code != 404 {
			t.Fatalf("refusal %d got %d before the budget was spent", i, code)
		}
	}
	if code := do("203.0.113.66", "/p/pid_1/master.m3u8"); code != 429 {
		t.Fatalf("after %d refusals: %d, want 429", RefusalsPerWindow, code)
	}
	if code := do("198.51.100.7", "/p/pid_1/master.m3u8"); code != 200 {
		t.Fatalf("another address was limited: %d", code)
	}
	if code := do("203.0.113.66", "/v1/assets"); code != 200 {
		t.Fatalf("only playback paths are limited by refusals: %d", code)
	}
}

// Guessing share secrets (D104) spends the same budget: unknown links
// answer 404 and expired ones 410, and either way the address is limited.
func TestShareSecretsAreLimitedByRefusals(t *testing.T) {
	f := newRefusals(func(*http.Request) netip.Addr { return netip.MustParseAddr("203.0.113.66") })
	h := f.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/s/EXPIRED/playback":
			w.WriteHeader(http.StatusGone)
		case "/s/GOOD/playback":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	do := func(path string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", path, nil))
		return rec.Code
	}
	for i := 1; i <= RefusalsPerWindow; i++ {
		path := "/s/GUESS/playback"
		if i%2 == 0 {
			path = "/s/EXPIRED/playback"
		}
		if code := do(path); code == http.StatusTooManyRequests {
			t.Fatalf("refusal %d was limited before the budget was spent", i)
		}
	}
	if code := do("/s/GOOD/playback"); code != http.StatusTooManyRequests {
		t.Fatalf("after %d refusals: %d, want 429", RefusalsPerWindow, code)
	}
}
