// Package ratelimit limits requests in one serve process: token buckets for
// rates, sliding windows for failures (wrong passwords, codes and tokens),
// and slots for concurrent long-polls. It also finds the client's address
// through trusted proxies, which every limit keyed by address needs.
//
// Limits live in memory, so with several replicas each enforces its own;
// one replica is the supported shape (D063).
package ratelimit

import (
	"math"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/visheshgubrani/clipmux/internal/httpjson"
)

// maxKeys bounds each limiter's memory. Past it, keys that have fully
// recovered are dropped; if that isn't enough the limiter forgets
// everything, failing open rather than growing without bound.
const maxKeys = 100_000

// Buckets is a token bucket per key, as GCRA: a key may make Burst requests
// at once and then Rate per second.
type Buckets struct {
	rate  float64
	burst int
	now   func() time.Time
	mu    sync.Mutex
	tat   map[string]time.Time // theoretical arrival time
}

// NewBuckets allows burst requests at once and rate per second after.
func NewBuckets(rate float64, burst int) *Buckets {
	return &Buckets{rate: rate, burst: max(burst, 1), now: time.Now, tat: map[string]time.Time{}}
}

// Take spends one request for key. Refused, it says how long until one is
// allowed.
func (b *Buckets) Take(key string) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	interval := time.Duration(float64(time.Second) / b.rate)
	tat := b.tat[key]
	if tat.Before(now) {
		tat = now
	}
	next := tat.Add(interval)
	if over := next.Sub(now) - time.Duration(b.burst)*interval; over > 0 {
		return false, over
	}
	if len(b.tat) >= maxKeys {
		for k, t := range b.tat {
			if !t.After(now) {
				delete(b.tat, k)
			}
		}
		if len(b.tat) >= maxKeys {
			clear(b.tat)
		}
	}
	b.tat[key] = next
	return true, 0
}

// Failures counts failures per key, and over all keys, in a sliding window.
// A key at its limit, or everyone once the global limit is reached, is
// refused until the oldest counted failure leaves the window.
type Failures struct {
	window         time.Duration
	perKey, global int // 0: no global limit
	now            func() time.Time
	mu             sync.Mutex
	byKey          map[string][]time.Time
	all            []time.Time
}

// NewFailures allows perKey failures per key and global over all of them
// (0 for none) in each window.
func NewFailures(window time.Duration, perKey, global int) *Failures {
	return &Failures{window: window, perKey: perKey, global: global, now: time.Now, byKey: map[string][]time.Time{}}
}

func (f *Failures) recent(ts []time.Time) []time.Time {
	cutoff := f.now().Add(-f.window)
	return slices.DeleteFunc(ts, func(t time.Time) bool { return !t.After(cutoff) })
}

// Allow reports whether key may try again, and if not how long until it
// may.
func (f *Failures) Allow(key string) (bool, time.Duration) { return f.allow(key, true) }

// AllowKey is Allow with only key's own limit: for a caller already known
// some other way (the owner's device), whom failures elsewhere mustn't
// lock out.
func (f *Failures) AllowKey(key string) (bool, time.Duration) { return f.allow(key, false) }

func (f *Failures) allow(key string, global bool) (bool, time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.all = f.recent(f.all)
	mine := f.recent(f.byKey[key])
	if len(mine) == 0 {
		delete(f.byKey, key)
	} else {
		f.byKey[key] = mine
	}
	var oldest time.Time
	switch {
	case len(mine) >= f.perKey:
		oldest = mine[len(mine)-f.perKey]
	case global && f.global > 0 && len(f.all) >= f.global:
		oldest = f.all[len(f.all)-f.global]
	default:
		return true, 0
	}
	return false, max(oldest.Add(f.window).Sub(f.now()), time.Second)
}

// Fail counts a failure for key.
func (f *Failures) Fail(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	if len(f.byKey) >= maxKeys {
		clear(f.byKey)
	}
	f.byKey[key] = append(f.byKey[key], now)
	if f.global > 0 {
		f.all = append(f.all, now)
	}
}

// Reset forgets key's failures, after a success.
func (f *Failures) Reset(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.byKey, key)
}

// Slots allows at most Max requests in flight per key.
type Slots struct {
	max   int
	mu    sync.Mutex
	inUse map[string]int
}

// NewSlots allows n concurrent requests per key.
func NewSlots(n int) *Slots { return &Slots{max: n, inUse: map[string]int{}} }

// Acquire takes a slot for key; release gives it back. ok is false when
// key has none free.
func (s *Slots) Acquire(key string) (release func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inUse[key] >= s.max {
		return nil, false
	}
	s.inUse[key]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.inUse[key]--; s.inUse[key] <= 0 {
				delete(s.inUse, key)
			}
		})
	}, true
}

// --- client addresses ---

// ClientIP is the request's client address: the connection's, or, when the
// connection comes from a trusted proxy (Caddy), the rightmost address in
// X-Forwarded-For that isn't one. A client can prepend anything to that
// header, so only hops a trusted proxy appended are believed.
func ClientIP(r *http.Request, trusted []netip.Prefix) netip.Addr {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	addr, _ := netip.ParseAddr(host)
	if !Trusted(addr, trusted) {
		return addr
	}
	hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		if !Trusted(hop, trusted) {
			return hop
		}
		addr = hop
	}
	return addr
}

// Trusted reports whether a is one of the trusted proxies.
func Trusted(a netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(a.Unmap()) {
			return true
		}
	}
	return false
}

// AddrKey is the key an address is limited by: IPv4 as itself, IPv6 by its
// /64, since one host usually holds a whole /64.
func AddrKey(a netip.Addr) string {
	a = a.Unmap()
	if a.Is6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.String()
}

// --- the refusal ---

// Problem is the 429 for a refused request: code rate_limited, with the
// seconds to wait as the retry_after detail.
func Problem(wait time.Duration) *httpjson.Problem {
	p := httpjson.Fail(http.StatusTooManyRequests, "rate_limited", "too many requests; try again later")
	p.Details = map[string]string{"retry_after": RetryAfter(wait)}
	return p
}

// RetryAfter is wait in whole seconds, at least 1, for a Retry-After header.
func RetryAfter(wait time.Duration) string {
	return strconv.Itoa(max(int(math.Ceil(wait.Seconds())), 1))
}

// Refuse writes the 429 with its Retry-After header.
func Refuse(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", RetryAfter(wait))
	httpjson.WriteProblem(w, Problem(wait))
}
