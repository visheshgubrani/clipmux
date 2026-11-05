package signingkeys_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/secrets"
	"github.com/visheshgubrani/clipmux/internal/signingkeys"
	"github.com/visheshgubrani/clipmux/internal/tokens"
)

func TestInternalKeyIsCreatedOnceAndSignsTokensItsPublicKeyVerifies(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	box, _ := secrets.NewBox(1, bytes.Repeat([]byte{1}, 32))
	for range 2 {
		if err := signingkeys.EnsureInternal(ctx, pool, box, "prj_default"); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	var kid, publicPEM string
	var sealed []byte
	if err := pool.QueryRow(ctx, `SELECT count(*) OVER (), id, public_key_pem, private_key_encrypted FROM signing_keys WHERE app_id IS NULL`).Scan(&n, &kid, &publicPEM, &sealed); err != nil {
		t.Fatal(err)
	}
	if n != 1 || bytes.Contains(sealed, []byte("PRIVATE KEY")) {
		t.Fatalf("internal keys = %d (sealed %d bytes); want one, with its private key sealed", n, len(sealed))
	}
	signer, err := signingkeys.Internal(ctx, pool, box, "prj_default")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := signer.Sign(tokens.Claims{Sub: "pid_1", Aud: tokens.AudiencePlayback, Iat: 1, Exp: 2})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := tokens.Parse(tok)
	pub, _ := tokens.ParsePublicKey(publicPEM)
	if err != nil || parsed.Kid != kid || parsed.Verify(pub) != nil {
		t.Fatalf("token kid %q, verify %v; want kid %s verified by the stored public key", parsed.Kid, err, kid)
	}
}

func TestDatabaseRejectsASecondActiveInternalKey(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	box, _ := secrets.NewBox(1, bytes.Repeat([]byte{1}, 32))
	if err := signingkeys.EnsureInternal(ctx, pool, box, "prj_default"); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(ctx, `INSERT INTO signing_keys (id, project_id, public_key_pem, private_key_encrypted) VALUES ('kid_2', 'prj_default', 'x', 'y')`)
	if err == nil || !strings.Contains(err.Error(), "signing_keys_one_internal") {
		t.Fatalf("second active internal key: %v, want the unique index to refuse it", err)
	}
}

func TestAppKeysStoreOnlyThePublicHalf(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	kid, privatePEM, err := signingkeys.CreateForApp(ctx, pool, "prj_default", "app_dashboard")
	if err != nil {
		t.Fatal(err)
	}
	var private []byte
	if err := pool.QueryRow(ctx, `SELECT private_key_encrypted FROM signing_keys WHERE id = $1 AND app_id = 'app_dashboard'`, kid).Scan(&private); err != nil || private != nil {
		t.Fatalf("app key row: private %v, %v; want no private key stored", private, err)
	}
	if _, err := tokens.ParsePrivateKey(privatePEM); err != nil {
		t.Fatalf("returned private key: %v", err)
	}
}
