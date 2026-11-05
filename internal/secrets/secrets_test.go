package secrets

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func testBox(t *testing.T) *Box {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	b, err := NewBox(1, key)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The vector was computed independently with Python's cryptography AESGCM:
// key 00..1f, nonce a0..ab, associated data
// "clipmux-secret-v1\nwebhook_endpoints\nwhk_1", plaintext "whsec_test".
const vector = "0100000001a0a1a2a3a4a5a6a7a8a9aaab91700f48269476da11115153d753f9144676438b987e5978b340"

func TestSealedSecretsOpenOnlyWithTheirKeyAndRow(t *testing.T) {
	b := testBox(t)
	nonce, _ := hex.DecodeString("a0a1a2a3a4a5a6a7a8a9aaab")
	if got := hex.EncodeToString(b.seal("webhook_endpoints", "whk_1", []byte("whsec_test"), nonce)); got != vector {
		t.Fatalf("seal = %s, want %s", got, vector)
	}
	sealed, _ := hex.DecodeString(vector)
	plain, err := b.Open("webhook_endpoints", "whk_1", sealed)
	if err != nil || string(plain) != "whsec_test" {
		t.Fatalf("open = %q, %v", plain, err)
	}
	if _, err := b.Open("webhook_endpoints", "whk_2", sealed); err == nil {
		t.Error("a ciphertext moved to another row must not open")
	}
	if _, err := b.Open("signing_keys", "whk_1", sealed); err == nil {
		t.Error("a ciphertext moved to another table must not open")
	}
	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 1
	if _, err := b.Open("webhook_endpoints", "whk_1", tampered); err == nil {
		t.Error("a tampered ciphertext must not open")
	}
	other, _ := NewBox(1, bytes.Repeat([]byte{7}, 32))
	if _, err := other.Open("webhook_endpoints", "whk_1", sealed); !errors.Is(err, ErrWrongKey) {
		t.Errorf("another key: err = %v, want ErrWrongKey", err)
	}
	v2, _ := NewBox(2, bytes.Repeat([]byte{7}, 32))
	if _, err := v2.Open("webhook_endpoints", "whk_1", sealed); !errors.Is(err, ErrWrongKey) {
		t.Errorf("another key version: err = %v, want ErrWrongKey", err)
	}

	a, c := b.Seal("t", "r", []byte("x")), b.Seal("t", "r", []byte("x"))
	if bytes.Equal(a, c) {
		t.Error("two seals of one value must use fresh nonces")
	}
	if got, err := b.Open("t", "r", a); err != nil || string(got) != "x" {
		t.Errorf("round trip = %q, %v", got, err)
	}
}

func TestKeyIsCreatedOnceWithMode0600AndTheEnvironmentWins(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(dir, ""); !errors.Is(err, ErrNoKey) {
		t.Fatalf("Load before create: err = %v, want ErrNoKey", err)
	}
	a, err := create(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "secrets.key")
	raw, _ := os.ReadFile(path)
	if !regexp.MustCompile(`^v1:[0-9a-f]{64}$`).Match(raw) {
		t.Fatalf("secrets.key = %q, want v1:<64 hex>", raw)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("secrets.key mode = %v, want 0600", info.Mode().Perm())
	}
	if _, err := create(dir); err == nil {
		t.Fatal("create must never overwrite an existing key")
	}
	b, err := Load(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := b.Open("t", "r", a.Seal("t", "r", []byte("x"))); err != nil || string(got) != "x" {
		t.Fatalf("the reloaded key must open what the created one sealed: %q, %v", got, err)
	}

	env := "v3:" + hex.EncodeToString(bytes.Repeat([]byte{9}, 32))
	e, err := Load(dir, env)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Open("t", "r", a.Seal("t", "r", []byte("x"))); !errors.Is(err, ErrWrongKey) {
		t.Fatal("CLIPMUX_SECRETS_KEY must win over secrets.key")
	}
	for _, bad := range []string{"abc", "v1:abcd", "1:" + hex.EncodeToString(make([]byte, 32)), "v0:" + hex.EncodeToString(make([]byte, 32))} {
		if _, err := Load(dir, bad); err == nil || errors.Is(err, ErrNoKey) {
			t.Errorf("Load(%q) = %v, want a format error", bad, err)
		}
	}
}
