package auth

import "testing"

func TestNewSetupTokenIsURLSafeAndUnique(t *testing.T) {
	a, b := NewSetupToken(), NewSetupToken()
	if a == b {
		t.Fatal("two tokens are equal")
	}
	if len(a) != 32 { // 24 bytes, unpadded base64url
		t.Fatalf("len = %d, want 32", len(a))
	}
}
