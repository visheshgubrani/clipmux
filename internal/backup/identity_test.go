package backup

import (
	"bytes"
	"crypto/ecdh"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/visheshgubrani/clipmux/internal/secrets"
)

func box(t *testing.T, version uint32, fill byte) *secrets.Box {
	t.Helper()
	b, err := secrets.NewBox(version, bytes.Repeat([]byte{fill}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// bech32 must agree with age's own: the recipient we encode from a public key
// is the one age prints, and age parses the identity we encode.
func TestBech32AgreesWithAge(t *testing.T) {
	scalar := bytes.Repeat([]byte{7}, 32)
	priv, err := ecdh.X25519().NewPrivateKey(scalar)
	if err != nil {
		t.Fatal(err)
	}
	id, err := age.ParseX25519Identity(encodeIdentity(scalar))
	if err != nil {
		t.Fatalf("age rejected the identity we encoded: %v", err)
	}
	want, err := bech32Encode("age", priv.PublicKey().Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if got := id.Recipient().String(); got != want {
		t.Errorf("recipient = %s, our bech32 of the public key = %s", got, want)
	}
	if !strings.HasPrefix(id.String(), "AGE-SECRET-KEY-1") || id.String() != encodeIdentity(scalar) {
		t.Errorf("identity = %s, want what we encoded: %s", id, encodeIdentity(scalar))
	}
}

func TestTheIdentityFollowsTheSecretsKeyAndItsVersion(t *testing.T) {
	cur := box(t, 2, 2)
	if err := cur.AddPrevious("v1:" + strings.Repeat("01", 32)); err != nil {
		t.Fatal(err)
	}
	ids, err := identities(cur)
	if err != nil || len(ids) != 2 {
		t.Fatalf("identities = %d, %v, want the current key's and the previous one's", len(ids), err)
	}
	again, _ := identities(cur)
	if ids[0].(*age.X25519Identity).String() != again[0].(*age.X25519Identity).String() {
		t.Error("the same key must give the same identity")
	}
	other, _ := identities(box(t, 2, 3))
	if ids[0].(*age.X25519Identity).String() == other[0].(*age.X25519Identity).String() {
		t.Error("another key must give another identity")
	}
	// The scalar is HKDF-SHA256(key, info "clipmux-backup-v1"), worked out
	// with Python's hmac: a change of the derivation must be a deliberate one.
	scalar, _ := cur.Derive(2, infoIdentity)
	if got, want := hex.EncodeToString(scalar), "4ae87b751e049b89f8a133b7b9cddf12865302be7938f05fe1d04fae59201d3b"; got != want {
		t.Fatalf("derived %s, want %s", got, want)
	}
	if got := ids[0].(*age.X25519Identity).String(); got != encodeIdentity(scalar) {
		t.Errorf("identity = %s, want the one encoded from that scalar", got)
	}
}

func TestSealedBackupsOpenWithTheIdentityAndWithEachRecipient(t *testing.T) {
	b := box(t, 1, 5)
	own, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	recips, err := recipients(b, []string{own.Recipient().String()})
	if err != nil {
		t.Fatal(err)
	}
	var sealed bytes.Buffer
	w, err := age.Encrypt(&sealed, recips...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "a dump"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed.String(), "a dump") {
		t.Fatal("the dump is in the clear")
	}
	for name, ids := range map[string][]age.Identity{"the install's identity": mustIdentities(t, b), "the owner's own key": {own}} {
		r, err := age.Decrypt(bytes.NewReader(sealed.Bytes()), ids...)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got, _ := io.ReadAll(r); string(got) != "a dump" {
			t.Errorf("%s: got %q", name, got)
		}
	}
	if _, err := age.Decrypt(bytes.NewReader(sealed.Bytes()), mustIdentities(t, box(t, 1, 6))...); err == nil {
		t.Error("another install's key opened the backup")
	}
	if _, err := recipients(b, []string{"not-a-recipient"}); err == nil || !strings.Contains(err.Error(), "CLIPMUX_BACKUP_RECIPIENTS") {
		t.Errorf("a bad recipient must name the variable: %v", err)
	}
}

func mustIdentities(t *testing.T, b *secrets.Box) []age.Identity {
	t.Helper()
	ids, err := identities(b)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}
