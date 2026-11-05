package contentkey

import (
	"encoding/hex"
	"testing"
)

// Vectors computed independently with Python's hmac and hashlib, so the
// Go handler and the Cloudflare Worker (Phase 2) can share them.
func TestDerivationVectors(t *testing.T) {
	s := Secret{Version: 1, key: make([]byte, 32)}
	for i := range s.key {
		s.key[i] = byte(i)
	}
	key := s.Derive("ast_1", "out_1")
	if got := hex.EncodeToString(key[:]); got != "8b60ee274c0f0c75195f891d401f5c7f" {
		t.Errorf("Derive = %s", got)
	}
	kid := KeyID("out_1")
	if got := hex.EncodeToString(kid[:]); got != "99b979c50698e1ed9eb5a3a8bf82d629" {
		t.Errorf("KeyID = %s", got)
	}
	if s.Derive("ast_1", "out_2") == key || s.Derive("ast_2", "out_1") == key {
		t.Error("each asset and output must get its own key")
	}
}

func TestRingServesEveryVersionAndUsesTheNewestForNewOutputs(t *testing.T) {
	v1, _ := FromHex(1, "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	v2, _ := FromHex(2, "202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f")
	r := NewRing(v2, v1)
	if r.Current().Version != 2 {
		t.Fatalf("current = v%d, want v2", r.Current().Version)
	}
	if s, ok := r.Get(1); !ok || s.Derive("ast_1", "out_1") != v1.Derive("ast_1", "out_1") {
		t.Fatal("v1 must stay derivable")
	}
	if _, ok := r.Get(3); ok {
		t.Fatal("an unknown version must not resolve")
	}
}
