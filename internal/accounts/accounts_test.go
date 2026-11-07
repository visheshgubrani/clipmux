package accounts

import (
	"testing"
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
