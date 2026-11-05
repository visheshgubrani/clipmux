package accounts

import (
	"net/netip"
	"testing"
	"time"
)

// Computed with Python: hashlib.pbkdf2_hmac('sha256', b'correct horse
// battery staple', bytes(range(16)), 600000, 32), base64 without padding.
func TestPasswordHashMatchesWorkedExample(t *testing.T) {
	salt := make([]byte, 16)
	for i := range salt {
		salt[i] = byte(i)
	}
	const want = "pbkdf2-sha256$600000$AAECAwQFBgcICQoLDA0ODw$7xdxRO7JQgy8EJPSqLNEqSvFBtDU7JwCjdGfgyTYweY"
	if got := hashWith("correct horse battery staple", salt, 600000); got != want {
		t.Fatalf("hash = %s, want %s", got, want)
	}
	if ok, rehash := checkPassword(want, "correct horse battery staple"); !ok || rehash {
		t.Fatalf("check = %v, rehash %v; want a match that needs no rehash", ok, rehash)
	}
	if ok, _ := checkPassword(want, "correct horse battery stapler"); ok {
		t.Fatal("a different password must not match")
	}
	weak := hashWith("correct horse battery staple", salt, 1000)
	if ok, rehash := checkPassword(weak, "correct horse battery staple"); !ok || !rehash {
		t.Fatal("a hash below the current iteration count must ask for a rehash")
	}
	for _, bad := range []string{"", "md5$x", "pbkdf2-sha256$abc$AA$AA", "pbkdf2-sha256$600000$!!$AA"} {
		if ok, _ := checkPassword(bad, "x"); ok {
			t.Errorf("malformed hash %q matched", bad)
		}
	}
}

func TestLoginsAreLimitedPerAddressAndOverall(t *testing.T) {
	now := time.Unix(1700000000, 0)
	l := newLimiter(func() time.Time { return now })
	a, b := netip.MustParseAddr("203.0.113.1"), netip.MustParseAddr("203.0.113.2")
	for range 5 {
		if ok, _ := l.allow(a); !ok {
			t.Fatal("the first five failures are allowed")
		}
		l.fail(a)
	}
	ok, wait := l.allow(a)
	if ok || wait <= 0 || wait > 15*time.Minute {
		t.Fatalf("sixth try: allow %v, wait %v; want refused for up to 15 min", ok, wait)
	}
	if ok, _ := l.allow(b); !ok {
		t.Fatal("another address is not limited by a's failures")
	}
	now = now.Add(15*time.Minute + time.Second)
	if ok, _ := l.allow(a); !ok {
		t.Fatal("failures older than the window no longer count")
	}
	for i := range 20 {
		l.fail(netip.AddrFrom4([4]byte{198, 51, 100, byte(i)}))
	}
	if ok, _ := l.allow(netip.MustParseAddr("192.0.2.9")); ok {
		t.Fatal("20 failures from anywhere in the window lock logins for everyone")
	}
}
