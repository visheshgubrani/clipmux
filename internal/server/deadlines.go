package server

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// deadlines gives each request read and write deadlines for its kind of
// route. The server has no global ReadTimeout or WriteTimeout: those would
// cut long-polls and large transfers, while having none lets a slow or
// stalled client hold a connection forever.
type deadlines struct {
	// api bounds an ordinary JSON request: reading it, then answering.
	apiRead, apiWrite time.Duration
	// longPoll bounds a claim or events long-poll: the wait plus a margin.
	longPoll time.Duration
	// long bounds the few requests that do slow work inline: Connect, the
	// setup test, deleting an asset, committing an output.
	long time.Duration
	// streamIdle bounds a transfer (storage, playback) by inactivity: each
	// read or write moves the deadline, like the worker's idle watchdog
	// (D016), so a large body takes as long as it needs while bytes flow.
	streamIdle time.Duration
}

// defaultDeadlines are the shipped bounds. Claims wait up to 30 s and event
// polls up to 25 s.
var defaultDeadlines = deadlines{
	apiRead: 30 * time.Second, apiWrite: 60 * time.Second,
	longPoll: 45 * time.Second, long: 5 * time.Minute, streamIdle: time.Minute,
}

// routeClass names how a request is bounded. It is decided from the method
// and path before routing.
type routeClass int

const (
	classAPI routeClass = iota
	classLongPoll
	classLong
	classStream
)

func classify(r *http.Request) routeClass {
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/_storage/"), strings.HasPrefix(p, "/v/"), strings.HasPrefix(p, "/p/"):
		return classStream
	case r.Method == http.MethodPost && p == "/worker/v1/claim",
		r.Method == http.MethodGet && p == "/v1/events":
		return classLongPoll
	case r.Method == http.MethodPost && (p == "/v1/cloudflare/connect" || p == "/v1/setup-test" || p == "/v1/content-keys/rotate" ||
		strings.HasPrefix(p, "/v1/executors/") && strings.HasSuffix(p, "/connect") ||
		strings.HasPrefix(p, "/worker/v1/attempts/") && strings.HasSuffix(p, "/complete")),
		r.Method == http.MethodDelete && strings.HasPrefix(p, "/v1/assets/") && !strings.Contains(p, "/playback-ids/"):
		return classLong
	}
	return classAPI
}

func (d deadlines) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		now := time.Now()
		read, write := d.apiRead, d.apiWrite
		switch classify(r) {
		case classLongPoll:
			read, write = d.apiRead, d.longPoll
		case classLong:
			read, write = d.long, d.long
		case classStream:
			s := &idleStream{ResponseWriter: w, rc: rc, idle: d.streamIdle}
			s.extend(now)
			if r.Body != nil && r.Body != http.NoBody {
				r.Body = &idleBody{ReadCloser: r.Body, s: s}
			}
			next.ServeHTTP(s, r)
			return
		}
		// Every request sets its own: with no WriteTimeout, net/http doesn't
		// reset a keep-alive connection's write deadline between requests.
		setDeadlines(rc, now.Add(read), now.Add(write))
		next.ServeHTTP(w, r)
	})
}

// setDeadlines ignores writers that can't take deadlines (tests' recorders).
func setDeadlines(rc *http.ResponseController, read, write time.Time) {
	if err := rc.SetReadDeadline(read); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return // the connection is gone; the handler will find out
	}
	_ = rc.SetWriteDeadline(write) // as above
}

// idleStream moves its deadlines forward as bytes flow. It moves them at
// most once a second, which is plenty against a minute's idle bound.
type idleStream struct {
	http.ResponseWriter
	rc   *http.ResponseController
	idle time.Duration
	mu   sync.Mutex // a handler may read its body and write from two goroutines
	last time.Time
}

func (s *idleStream) extend(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Sub(s.last) < time.Second {
		return
	}
	s.last = now
	setDeadlines(s.rc, now.Add(s.idle), now.Add(s.idle))
}

func (s *idleStream) Write(b []byte) (int, error) {
	s.extend(time.Now())
	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the connection. idleStream has
// no ReadFrom on purpose: a sendfile can't move its deadline mid-transfer,
// so streams copy through Write.
func (s *idleStream) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// idleBody moves the stream's deadlines as an upload's bytes arrive.
type idleBody struct {
	io.ReadCloser
	s *idleStream
}

func (b *idleBody) Read(p []byte) (int, error) {
	b.s.extend(time.Now())
	return b.ReadCloser.Read(p)
}
