package apikeys_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
)

func TestKeysAuthenticateUntilRevokedOrTheirAppIsDisabled(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)

	secret, stored, err := apikeys.Create(ctx, pool, "prj_default", "app_dashboard", "backend", []string{apikeys.UploadsWrite, apikeys.AssetsRead})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, "cmx_") || strings.Contains(string(stored.KeyHash), secret) {
		t.Fatalf("secret %q must carry the cmx_ prefix and be stored only hashed", secret)
	}

	key, err := apikeys.Authenticate(ctx, pool, secret)
	if err != nil || key.AppID != "app_dashboard" || !key.Can(apikeys.UploadsWrite) || key.Can(apikeys.AssetsWrite) {
		t.Fatalf("Authenticate = %+v, %v", key, err)
	}
	for _, bad := range []string{"", "cmx_wrong", strings.TrimPrefix(secret, "cmx_")} {
		if _, err := apikeys.Authenticate(ctx, pool, bad); !errors.Is(err, apikeys.ErrInvalid) {
			t.Errorf("Authenticate(%q): err = %v, want ErrInvalid", bad, err)
		}
	}

	if _, err := pool.Exec(ctx, `UPDATE apps SET disabled_at = now() WHERE id = 'app_dashboard'`); err != nil {
		t.Fatal(err)
	}
	if _, err := apikeys.Authenticate(ctx, pool, secret); !errors.Is(err, apikeys.ErrInvalid) {
		t.Fatalf("disabled app: err = %v, want ErrInvalid", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE apps SET disabled_at = NULL; UPDATE api_keys SET revoked_at = now()`); err != nil {
		t.Fatal(err)
	}
	if _, err := apikeys.Authenticate(ctx, pool, secret); !errors.Is(err, apikeys.ErrInvalid) {
		t.Fatalf("revoked key: err = %v, want ErrInvalid", err)
	}
}

func TestCreateRefusesUnknownScopes(t *testing.T) {
	pool := dbtest.New(t)
	if _, _, err := apikeys.Create(context.Background(), pool, "prj_default", "app_dashboard", "k", []string{"apps:write"}); err == nil {
		t.Fatal("an integration key must never get a management scope")
	}
	if _, _, err := apikeys.Create(context.Background(), pool, "prj_default", "app_dashboard", "k", nil); err == nil {
		t.Fatal("a key needs at least one scope")
	}
}
