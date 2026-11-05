package contentkey

import (
	"encoding/hex"
	"os"
	"path/filepath"
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

func TestLoadOrCreatePersistsAPrivateSecret(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreate(dir)
	if err != nil || a.Derive("x", "y") != b.Derive("x", "y") || a.Version != 1 {
		t.Fatalf("reload: %v", err)
	}
	info, _ := os.Stat(filepath.Join(dir, "content.key"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("content.key mode = %v, want 0600", info.Mode().Perm())
	}
	if err := os.WriteFile(filepath.Join(dir, "content.key"), []byte("v1:abcd"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(dir); err == nil {
		t.Fatal("a short secret must be refused, not used")
	}
}
