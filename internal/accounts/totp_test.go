package accounts

import (
	"testing"
	"time"
)

// RFC 6238 appendix B, SHA-1, cut to six digits.
func TestTOTPMatchesTheRFCVectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	for unix, want := range map[int64]string{59: "287082", 1111111109: "081804", 1234567890: "005924", 2000000000: "279037"} {
		if got := totpCode(secret, unix/30); got != want {
			t.Errorf("T=%d: %s, want %s", unix, got, want)
		}
	}
	now := time.Unix(1111111109, 0)
	if step := matchStep(secret, "081804", now); step != 1111111109/30 {
		t.Fatalf("matchStep = %d", step)
	}
	if matchStep(secret, totpCode(secret, now.Unix()/30-1), now) < 0 || matchStep(secret, totpCode(secret, now.Unix()/30+1), now) < 0 {
		t.Fatal("one step of drift either way must be accepted")
	}
	if matchStep(secret, totpCode(secret, now.Unix()/30-2), now) >= 0 || matchStep(secret, "12345", now) >= 0 {
		t.Fatal("two steps off, or a short code, must be refused")
	}
}
