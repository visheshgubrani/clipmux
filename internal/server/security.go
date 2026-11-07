package server

import (
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/visheshgubrani/clipmux/internal/ratelimit"
)

// dashboardCSP is the dashboard's Content-Security-Policy. Scripts come only
// from the server itself; the page may also reach the media URL (playback,
// posters) and the storage buckets uploads PUT their parts to. Styles allow
// inline because the dialog and player components inject <style> elements;
// blob: covers hls.js's worker and Media Source playback.
func dashboardCSP(mediaURL string, storage []string) string {
	extra := []string{}
	for _, o := range append([]string{originOf(mediaURL)}, storage...) {
		if o != "" && !slices.Contains(extra, o) {
			extra = append(extra, o)
		}
	}
	others := strings.Join(extra, " ")
	with := func(sources string) string { return strings.TrimSpace(sources + " " + others) }
	return strings.Join([]string{
		"default-src 'self'",
		"script-src 'self'",
		"style-src 'self' 'unsafe-inline'",
		"img-src " + with("'self' data: blob:"),
		"media-src " + with("'self' blob:"),
		"connect-src " + with("'self'"),
		"worker-src 'self' blob:",
		"font-src 'self' data:",
		"object-src 'none'",
		"base-uri 'none'",
		"frame-ancestors 'none'",
		"form-action 'self'",
	}, "; ")
}

func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// transportSecurity sends every response with nosniff, and once the public
// URL is https, keeps browsers on https for the public and media hostnames:
// a plain-http request for either is redirected (308), and https responses
// carry HSTS. The scheme is known only from a trusted proxy's
// X-Forwarded-Proto (Caddy terminates TLS), so a direct request is left
// alone.
type transportSecurity struct {
	publicURL, mediaURL func() string
	trusted             []netip.Prefix
}

// hsts is a year, without includeSubDomains or preload: those reach
// hosts this server doesn't run.
const hsts = "max-age=31536000"

func (t transportSecurity) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if t.publicURL != nil && strings.HasPrefix(t.publicURL(), "https://") && t.ownHost(r.Host) {
			switch t.scheme(r) {
			case "http":
				http.Redirect(w, r, "https://"+r.Host+r.URL.RequestURI(), http.StatusPermanentRedirect)
				return
			case "https":
				w.Header().Set("Strict-Transport-Security", hsts)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ownHost reports whether host (with or without a port) is the public or
// media hostname, so a redirect never leads anywhere else.
func (t transportSecurity) ownHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	for _, u := range []string{t.publicURL(), t.mediaURL()} {
		if p, err := url.Parse(u); err == nil && p.Hostname() != "" && strings.EqualFold(p.Hostname(), host) {
			return true
		}
	}
	return false
}

// scheme is how the client reached us: "https" or "http" as a trusted proxy
// says, or "" when nobody trusted said.
func (t transportSecurity) scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	addr, err := netip.ParseAddr(host)
	if err != nil || !slices.ContainsFunc(t.trusted, func(p netip.Prefix) bool { return p.Contains(addr.Unmap()) }) {
		return ""
	}
	proto, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	switch strings.ToLower(strings.TrimSpace(proto)) {
	case "https":
		return "https"
	case "http":
		return "http"
	}
	return ""
}

// refusals limits playback by its failures (D067). A classroom behind one
// NAT makes thousands of good requests, so successes never count; a client
// scanning for playback IDs or replaying stale tokens makes refusals. An
// address with RefusalsPerWindow refusals (401, 403, 404) in RefusalWindow
// gets 429 for every playback request until they age out.
type refusals struct {
	limit    *ratelimit.Failures
	clientIP func(*http.Request) netip.Addr
}

// Delivery's refusal budget per address.
const (
	RefusalWindow     = 5 * time.Minute
	RefusalsPerWindow = 300
)

func newRefusals(clientIP func(*http.Request) netip.Addr) refusals {
	return refusals{limit: ratelimit.NewFailures(RefusalWindow, RefusalsPerWindow, 0), clientIP: clientIP}
}

func (f refusals) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v/") && !strings.HasPrefix(r.URL.Path, "/p/") {
			next.ServeHTTP(w, r)
			return
		}
		key := ratelimit.AddrKey(f.clientIP(r))
		if ok, wait := f.limit.Allow(key); !ok {
			w.Header().Set("Access-Control-Allow-Origin", "*") // a player must see why
			ratelimit.Refuse(w, wait)
			return
		}
		st := &statusOnly{ResponseWriter: w}
		next.ServeHTTP(st, r)
		switch st.status {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			f.limit.Fail(key)
		}
	})
}

// statusOnly notes the status a handler wrote.
type statusOnly struct {
	http.ResponseWriter
	status int
}

func (s *statusOnly) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusOnly) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusOnly) Unwrap() http.ResponseWriter { return s.ResponseWriter }
