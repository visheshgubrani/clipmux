package main

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/tokens"
)

var keyLine = regexp.MustCompile(`key:\s+(cmx_\S+)`)

func TestCreateKeyCreatesTheAppOnceAndPrintsAWorkingKey(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)

	var first, second bytes.Buffer
	if err := createKey(ctx, pool, "LMS", "backend", []string{apikeys.UploadsWrite}, &first); err != nil {
		t.Fatal(err)
	}
	if err := createKey(ctx, pool, "LMS", "rotation", []string{apikeys.AssetsRead}, &second); err != nil {
		t.Fatal(err)
	}
	var apps int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM apps WHERE name = 'LMS'`).Scan(&apps); err != nil || apps != 1 {
		t.Fatalf("apps named LMS = %d, %v; want 1", apps, err)
	}
	m := keyLine.FindStringSubmatch(first.String())
	if m == nil {
		t.Fatalf("no key in output:\n%s", first.String())
	}
	key, err := apikeys.Authenticate(ctx, pool, m[1])
	if err != nil || !key.Can(apikeys.UploadsWrite) {
		t.Fatalf("printed key: %+v, %v", key, err)
	}
}

func TestAdminRequiresAnApp(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := admin([]string{"create-key"}, func(string) string { return "" }, &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if code := admin(nil, func(string) string { return "" }, &out, &errOut); code != 2 {
		t.Fatalf("no subcommand: exit = %d, want 2", code)
	}
}

func TestCreateSigningKeyStoresOnlyThePublicKey(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	var out bytes.Buffer
	if err := createSigningKey(ctx, pool, "LMS", &out); err != nil {
		t.Fatal(err)
	}
	priv, err := tokens.ParsePrivateKey(out.String()[strings.Index(out.String(), "-----BEGIN"):])
	if err != nil {
		t.Fatalf("printed private key: %v\n%s", err, out.String())
	}
	kid := regexp.MustCompile(`kid: (kid_\S+)`).FindStringSubmatch(out.String())[1]
	var publicPEM string
	var private []byte
	if err := pool.QueryRow(ctx, `SELECT public_key_pem, private_key_encrypted FROM signing_keys WHERE id = $1`, kid).Scan(&publicPEM, &private); err != nil {
		t.Fatal(err)
	}
	if private != nil {
		t.Fatal("an app's private key must never be stored")
	}
	pub, _ := tokens.ParsePublicKey(publicPEM)
	tok, _ := tokens.Sign(priv, kid, tokens.Claims{Sub: "pid_1", Aud: "v", Iat: 1, Exp: 2})
	if p, err := tokens.Parse(tok); err != nil || p.Verify(pub) != nil {
		t.Fatal("a token signed with the printed key must verify with the stored key")
	}
}
