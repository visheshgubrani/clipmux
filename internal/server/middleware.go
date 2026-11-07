package server

import (
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strings"
	"time"

	"github.com/visheshgubrani/clipmux/internal/httpjson"
	"github.com/visheshgubrani/clipmux/internal/logging"
)

// requestLog gives every request an ID and logs one line for it when it
// ends. The line names the route pattern and a path rebuilt from it, with
// tokens and pairing codes redacted; it never carries the query, the
// Referer or the Authorization header.
type requestLog struct {
	logger *slog.Logger
	// trusted are the proxies whose X-Request-Id is believed.
	trusted []netip.Prefix
	// clientIP finds the client's address through trusted proxies.
	clientIP func(*http.Request) netip.Addr
	// obs counts requests (D066); nil counts nothing.
	obs *observer
}

// redactedWildcards are path wildcards that carry a credential.
var redactedWildcards = map[string]bool{"token": true, "code": true}

func (l requestLog) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := r.Header.Get("X-Request-Id")
		if !validRequestID(id) || !l.peerTrusted(r) {
			id = "req_" + rand.Text()
		}
		w.Header().Set("X-Request-Id", id)
		r = r.WithContext(logging.With(r.Context(), "request_id", id))
		rec := &recorder{ResponseWriter: w}
		if l.obs != nil {
			l.obs.inFlight.Add(1)
		}
		abort := l.serve(next, rec, r)
		if l.obs != nil {
			l.obs.inFlight.Add(-1)
		}

		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		var ip string
		if l.clientIP != nil {
			if a := l.clientIP(r); a.IsValid() {
				ip = a.String()
			}
		}
		l.obs.request(r, status, rec.bytes, time.Since(start))
		l.logger.Log(r.Context(), accessLevel(r, status), "http request",
			"method", r.Method,
			"route", r.Pattern,
			"path", routePath(r),
			"status", status,
			"bytes", rec.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
			"ip", ip,
		)
		if abort {
			// The response was cut short: net/http drops the connection
			// without logging a stack for this value.
			panic(http.ErrAbortHandler)
		}
	})
}

// serve runs the handler, recovering a panic: it is logged with its stack
// and answered 500 if nothing was written yet, so one bad request can't
// take serve down. It reports whether the connection must be dropped: the
// handler aborted, or panicked after its response had started.
func (l requestLog) serve(next http.Handler, rec *recorder, r *http.Request) (abort bool) {
	defer func() {
		v := recover()
		switch {
		case v == nil:
		case v == http.ErrAbortHandler:
			abort = true
		default:
			l.logger.ErrorContext(r.Context(), "handler panicked", "route", r.Pattern, "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
			if rec.written() {
				abort = true
				return
			}
			httpjson.Error(rec, http.StatusInternalServerError, "internal", "internal error")
		}
	}()
	next.ServeHTTP(rec, r)
	return false
}

func (l requestLog) peerTrusted(r *http.Request) bool {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	for _, p := range l.trusted {
		if p.Contains(addr.Unmap()) {
			return true
		}
	}
	return false
}

// validRequestID accepts an ID from a trusted proxy only if it is short and
// plain, so it can't forge log fields.
func validRequestID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._:-", c)) {
			return false
		}
	}
	return true
}

// accessLevel keeps the busiest successful traffic (playback, signed
// storage URLs, worker heartbeats and claims, health checks) at debug.
func accessLevel(r *http.Request, status int) slog.Level {
	switch {
	case status >= 500:
		return slog.LevelWarn
	case status >= 400:
		return slog.LevelInfo
	}
	p := r.URL.Path
	for _, prefix := range []string{"/v/", "/p/", "/_storage/", "/worker/v1/"} {
		if strings.HasPrefix(p, prefix) {
			return slog.LevelDebug
		}
	}
	if p == "/healthz" || p == "/readyz" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

// routePath rebuilds the request's path from its route pattern, with the
// wildcards that carry credentials redacted. A request no pattern matched
// logs its own path, with any playback token hidden, cut to 256 bytes.
func routePath(r *http.Request) string {
	pattern := r.Pattern
	if i := strings.IndexByte(pattern, '/'); i >= 0 {
		pattern = pattern[i:]
	} else {
		pattern = ""
	}
	if !strings.Contains(pattern, "{") {
		p := logging.RedactPath(r.URL.Path)
		if len(p) > 256 {
			p = p[:256]
		}
		return p
	}
	segs := strings.Split(pattern, "/")
	for i, seg := range segs {
		if !strings.HasPrefix(seg, "{") || !strings.HasSuffix(seg, "}") {
			continue
		}
		name := strings.TrimSuffix(strings.Trim(seg, "{}"), "...")
		switch {
		case name == "$":
			segs[i] = ""
		case redactedWildcards[name]:
			segs[i] = "REDACTED"
		default:
			segs[i] = r.PathValue(name)
		}
	}
	return strings.Join(segs, "/")
}

// recorder notes the status and the bytes written. It unwraps, so
// http.ResponseController reaches the connection's own deadlines and
// flushing, and it keeps the connection's ReadFrom (sendfile) for streams.
type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) ReadFrom(src io.Reader) (int64, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	var n int64
	var err error
	if rf, ok := r.ResponseWriter.(io.ReaderFrom); ok {
		n, err = rf.ReadFrom(src)
	} else {
		n, err = io.Copy(struct{ io.Writer }{r.ResponseWriter}, src)
	}
	r.bytes += n
	return n, err
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// written reports whether the handler has started its response.
func (r *recorder) written() bool { return r.status != 0 }
