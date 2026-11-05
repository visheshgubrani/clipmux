package accounts

import (
	"net/netip"
	"slices"
	"sync"
	"time"
)

// Failed logins are limited before any password is hashed: five per address
// and twenty overall in a sliding 15-minute window.
const (
	limitWindow = 15 * time.Minute
	limitPerIP  = 5
	limitAll    = 20
)

type limiter struct {
	now    func() time.Time
	mu     sync.Mutex
	perIP  map[netip.Addr][]time.Time
	global []time.Time
}

func newLimiter(now func() time.Time) *limiter {
	return &limiter{now: now, perIP: map[netip.Addr][]time.Time{}}
}

// recent drops failures older than the window.
func (l *limiter) recent(ts []time.Time) []time.Time {
	cutoff := l.now().Add(-limitWindow)
	return slices.DeleteFunc(ts, func(t time.Time) bool { return !t.After(cutoff) })
}

// allow reports whether a login from ip may be tried, and if not how long
// until it may.
func (l *limiter) allow(ip netip.Addr) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.global = l.recent(l.global)
	mine := l.recent(l.perIP[ip])
	if len(mine) == 0 {
		delete(l.perIP, ip)
	} else {
		l.perIP[ip] = mine
	}
	var oldest time.Time
	switch {
	case len(mine) >= limitPerIP:
		oldest = mine[len(mine)-limitPerIP]
	case len(l.global) >= limitAll:
		oldest = l.global[len(l.global)-limitAll]
	default:
		return true, 0
	}
	return false, max(oldest.Add(limitWindow).Sub(l.now()), time.Second)
}

func (l *limiter) fail(ip netip.Addr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.perIP[ip] = append(l.perIP[ip], now)
	l.global = append(l.global, now)
}

func (l *limiter) reset(ip netip.Addr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.perIP, ip)
}
