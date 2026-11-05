package secrets

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
)

func TestServeRefusesAKeyThatCannotOpenStoredSecrets(t *testing.T) {
	ctx := context.Background()
	q := db.New(dbtest.New(t))
	dir := t.TempDir()

	first, err := LoadOrCreate(ctx, q, "prj_default", dir, "")
	if err != nil {
		t.Fatal(err)
	}
	again, err := LoadOrCreate(ctx, q, "prj_default", dir, "")
	if err != nil {
		t.Fatalf("second boot with the same key: %v", err)
	}
	if got, err := again.Open("t", "r", first.Seal("t", "r", []byte("x"))); err != nil || string(got) != "x" {
		t.Fatalf("second boot must load the first key: %q, %v", got, err)
	}

	other := "v1:" + hex.EncodeToString(bytes.Repeat([]byte{5}, 32))
	if _, err := LoadOrCreate(ctx, q, "prj_default", dir, other); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("another key: err = %v, want ErrWrongKey", err)
	}
	// A lost data directory must not quietly mint a new key while the
	// database still holds secrets sealed with the old one.
	if _, err := LoadOrCreate(ctx, q, "prj_default", t.TempDir(), ""); !errors.Is(err, ErrNoKey) {
		t.Fatalf("missing key with sealed secrets present: err = %v, want ErrNoKey", err)
	}
}
