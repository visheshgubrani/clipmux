package ratelimit

import (
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestABucketAllowsItsBurstThenItsRate(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	b := NewBuckets(1, 3)
	b.now = c.now
	for i := 1; i <= 3; i++ {
		if ok, _ := b.Take("key_a"); !ok {
			t.Fatalf("request %d of the burst refused", i)
		}
	}
	ok, wait := b.Take("key_a")
	if ok || wait != time.Second || RetryAfter(wait) != "1" {
		t.Fatalf("4th: ok %v wait %v, want refused for 1 s", ok, wait)
	}
	if ok, _ := b.Take("key_b"); !ok {
		t.Fatal("another key shares the bucket")
	}
	c.t = c.t.Add(time.Second)
	if ok, _ := b.Take("key_a"); !ok {
		t.Fatal("not allowed again after 1 s")
	}
	if ok, _ := b.Take("key_a"); ok {
		t.Fatal("a refilled bucket gave more than one")
	}
}

func TestFailuresPerKeyAndOverall(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	f := NewFailures(15*time.Minute, 5, 20)
	f.now = c.now
	for range 5 {
		f.Fail("203.0.113.1")
	}
	if ok, wait := f.Allow("203.0.113.1"); ok || wait != 15*time.Minute {
		t.Fatalf("6th try: %v %v, want refused for the window", ok, wait)
	}
	if ok, _ := f.Allow("203.0.113.2"); !ok {
		t.Fatal("another address refused")
	}
	for i := range 15 {
		f.Fail("198.51.100." + string(rune('a'+i)))
	}
	if ok, _ := f.Allow("192.0.2.9"); ok {
		t.Fatal("past the global limit, a fresh address must wait too")
	}
	if ok, _ := f.AllowKey("192.0.2.9"); !ok {
		t.Fatal("AllowKey: past the global limit, a fresh address's own limit still allows it")
	}
	if ok, _ := f.AllowKey("203.0.113.1"); ok {
		t.Fatal("AllowKey: an address at its own limit must wait")
	}
	c.t = c.t.Add(15*time.Minute + time.Second)
	if ok, _ := f.Allow("203.0.113.1"); !ok {
		t.Fatal("failures outside the window still count")
	}
	f.Fail("x")
	f.Reset("x")
	if ok, _ := NewFailures(time.Minute, 1, 0).Allow("x"); !ok {
		t.Fatal("no global limit means none")
	}
}

func TestSlots(t *testing.T) {
	s := NewSlots(2)
	r1, ok1 := s.Acquire("k")
	_, ok2 := s.Acquire("k")
	if _, ok3 := s.Acquire("k"); !ok1 || !ok2 || ok3 {
		t.Fatalf("got %v %v %v, want two slots", ok1, ok2, ok3)
	}
	r1()
	r1() // releasing twice frees one slot only
	if _, ok := s.Acquire("k"); !ok {
		t.Fatal("a released slot isn't free")
	}
	if _, ok := s.Acquire("k"); ok {
		t.Fatal("a double release freed two slots")
	}
}

func TestClientIPBelievesOnlyTrustedHops(t *testing.T) {
	caddy := []netip.Prefix{netip.MustParsePrefix("172.16.0.0/12")}
	for _, c := range []struct{ remote, xff, want string }{
		{"203.0.113.5:1", "198.51.100.1", "203.0.113.5"},             // untrusted peer: header ignored
		{"172.18.0.2:1", "198.51.100.1", "198.51.100.1"},             // via Caddy
		{"172.18.0.2:1", "6.6.6.6, 198.51.100.1", "198.51.100.1"},    // a forged first hop is skipped
		{"172.18.0.2:1", "198.51.100.1, 172.18.0.9", "198.51.100.1"}, // two trusted proxies
		{"172.18.0.2:1", "", "172.18.0.2"},                           // the proxy itself
		{"[2001:db8::1]:1", "", "2001:db8::1"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := ClientIP(r, caddy); got.String() != c.want {
			t.Errorf("%s %q: got %s, want %s", c.remote, c.xff, got, c.want)
		}
	}
	if a, b := AddrKey(netip.MustParseAddr("2001:db8:1:2:aaaa::1")), AddrKey(netip.MustParseAddr("2001:db8:1:2:bbbb::9")); a != b || a != "2001:db8:1:2::/64" {
		t.Fatalf("IPv6 /64 grouping: %s %s", a, b)
	}
	if AddrKey(netip.MustParseAddr("::ffff:192.0.2.1")) != "192.0.2.1" {
		t.Fatal("an IPv4-mapped address is its IPv4")
	}
}

func TestTheRefusal(t *testing.T) {
	rec := httptest.NewRecorder()
	Refuse(rec, 1500*time.Millisecond)
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "2" ||
		!strings.Contains(rec.Body.String(), `"code":"rate_limited"`) || !strings.Contains(rec.Body.String(), `"retry_after":"2"`) {
		t.Fatalf("got %d %q %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
}
