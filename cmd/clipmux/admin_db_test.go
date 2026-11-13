package main

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/secrets"
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

func TestCreateKeyDefaultsToTheScopesAnIntegrationNeeds(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	var schema string
	if err := pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	getenv := func(k string) string {
		if k == "DATABASE_URL" {
			return u.String()
		}
		return ""
	}
	cases := []struct {
		args []string
		want []string
	}{
		// The dashboard's and create-integration's defaults, which doctor checks.
		{[]string{"create-key", "--app", "LMS", "--allow-loopback"}, []string{apikeys.UploadsWrite, apikeys.AssetsRead, apikeys.AssetsWrite}},
		// A list typed with spaces after the commas.
		{[]string{"create-key", "--app", "LMS", "--allow-loopback", "--scopes", "uploads:write, assets:read"}, []string{apikeys.UploadsWrite, apikeys.AssetsRead}},
	}
	for _, c := range cases {
		var out, errOut bytes.Buffer
		if code := admin(c.args, getenv, strings.NewReader(""), &out, &errOut); code != 0 {
			t.Fatalf("%v: exit %d: %s", c.args, code, errOut.String())
		}
		m := keyLine.FindStringSubmatch(out.String())
		if m == nil {
			t.Fatalf("no key in output:\n%s", out.String())
		}
		key, err := apikeys.Authenticate(ctx, pool, m[1])
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{apikeys.UploadsWrite, apikeys.AssetsRead, apikeys.AssetsWrite, apikeys.AnalyticsRead} {
			if key.Can(s) != slices.Contains(c.want, s) {
				t.Errorf("%v: key can %s = %v, want scopes %v", c.args, s, key.Can(s), c.want)
			}
		}
	}
}

func TestAdminRequiresAnApp(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := admin([]string{"create-key"}, func(string) string { return "" }, strings.NewReader(""), &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if code := admin(nil, func(string) string { return "" }, strings.NewReader(""), &out, &errOut); code != 2 {
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

func TestCreateWebhookPrintsTheSecretOnceAndStoresItSealed(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	box, err := secrets.NewBox(1, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := createWebhook(ctx, pool, box, "LMS", "https://lms.example.com/api/v1/callback/clipmux", &out); err != nil {
		t.Fatal(err)
	}
	secret := regexp.MustCompile(`secret:\s+(whsec_\S+)`).FindStringSubmatch(out.String())
	id := regexp.MustCompile(`endpoint:\s+(whk_\S+)`).FindStringSubmatch(out.String())
	if secret == nil || id == nil {
		t.Fatalf("no endpoint or secret in output:\n%s", out.String())
	}
	var url string
	var sealed []byte
	if err := pool.QueryRow(ctx, `SELECT url, secret_encrypted FROM webhook_endpoints WHERE id = $1`, id[1]).Scan(&url, &sealed); err != nil {
		t.Fatal(err)
	}
	if url != "https://lms.example.com/api/v1/callback/clipmux" {
		t.Errorf("url = %q", url)
	}
	if bytes.Contains(sealed, []byte(secret[1])) {
		t.Fatal("the secret must be stored sealed, never in the clear")
	}
	if plain, err := box.Open("webhook_endpoints", id[1], sealed); err != nil || string(plain) != secret[1] {
		t.Fatalf("stored secret opens to %q, %v; want the printed one", plain, err)
	}

	for _, bad := range []string{"ftp://lms.example.com/hook", "https://user:pw@lms.example.com/hook", "lms.example.com/hook", "https:///hook", "https://lms.example.com/hook#frag"} {
		if err := createWebhook(ctx, pool, box, "LMS", bad, &bytes.Buffer{}); err == nil {
			t.Errorf("createWebhook(%q) succeeded, want a URL error", bad)
		}
	}
}

// No API key until the public URL changes from loopback: presigned URLs and
// base_url are bound to it (docs/architecture.md, "First boot").
func TestCreateKeyIsRefusedWhileThePublicURLIsLoopback(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if err := keysAllowed(ctx, pool, env(nil), false); err == nil {
		t.Fatal("a fresh install (localhost) must refuse API keys")
	}
	if err := keysAllowed(ctx, pool, env(nil), true); err != nil {
		t.Fatalf("--allow-loopback (local development): %v", err)
	}
	if err := keysAllowed(ctx, pool, env(map[string]string{"CLIPMUX_PUBLIC_URL": "https://video.example.com"}), false); err != nil {
		t.Fatalf("a public URL pinned by the environment: %v", err)
	}
	if err := keysAllowed(ctx, pool, env(map[string]string{"CLIPMUX_PUBLIC_URL": "http://127.0.0.1:8080"}), false); err == nil {
		t.Fatal("a loopback URL pinned by the environment must refuse too")
	}
	if _, err := pool.Exec(ctx, `UPDATE projects SET public_url = 'http://192.168.1.10:8080'`); err != nil {
		t.Fatal(err)
	}
	if err := keysAllowed(ctx, pool, env(nil), false); err != nil {
		t.Fatalf("a LAN URL saved at first boot: %v", err)
	}
}

func TestResetPasswordReadsTheNewPasswordFromStdin(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	if err := resetPassword(ctx, pool, strings.NewReader("a long new password\n"), &bytes.Buffer{}); err == nil {
		t.Fatal("reset-password without an owner must fail")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO owners (id, project_id, email, password_hash) VALUES ('own_1', 'prj_default', 'owner@example.com', 'x')`); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := resetPassword(ctx, pool, strings.NewReader("a long new password\n"), &out); err != nil {
		t.Fatal(err)
	}
	var hash string
	if err := pool.QueryRow(ctx, `SELECT password_hash FROM owners`).Scan(&hash); err != nil || !strings.HasPrefix(hash, "pbkdf2-sha256$600000$") {
		t.Fatalf("stored hash %q, %v", hash, err)
	}
	if err := resetPassword(ctx, pool, strings.NewReader("short\n"), &out); err == nil {
		t.Fatal("a short password must be refused")
	}
}

// admin setup-test --wait queues a test for serve to run and exits non-zero
// unless it passes, so compose and CI can gate on it.
func TestAdminSetupTestWaitExitsNonZeroOnFailure(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	finish := func(status string) {
		for {
			var id string
			if err := pool.QueryRow(ctx, `SELECT id FROM setup_tests WHERE status = 'queued'`).Scan(&id); err == nil {
				_, _ = pool.Exec(ctx, `UPDATE setup_tests SET status = $2, finished_at = now(),
					checks = '[{"name":"upload","ok":true},{"name":"encode","ok":false,"detail":"not ready after 15m0s; is a worker running?"}]' WHERE id = $1`, id, status)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	var out bytes.Buffer
	go finish("failed")
	if err := setupTest(ctx, pool, true, time.Minute, 10*time.Millisecond, &out); err == nil || !strings.Contains(out.String(), "encode") {
		t.Fatalf("a failed test: err %v, output:\n%s", err, out.String())
	}
	out.Reset()
	go finish("passed")
	if err := setupTest(ctx, pool, true, time.Minute, 10*time.Millisecond, &out); err != nil {
		t.Fatalf("a passed test: %v\n%s", err, out.String())
	}
}

func TestCreateIntegrationPrintsAWorkingEnvBlock(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	getenv := func(k string) string {
		return map[string]string{"CLIPMUX_PUBLIC_URL": "https://video.example.test"}[k]
	}
	box, _ := secrets.NewBox(1, bytes.Repeat([]byte{4}, 32))
	var out bytes.Buffer
	if err := createIntegration(ctx, pool, box, getenv, "LMS", "https://lms.example.test/hooks", false, &out); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{}
	for line := range strings.SplitSeq(out.String(), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") {
			vars[k] = v
		}
	}
	for _, k := range []string{"CLIPMUX_URL", "CLIPMUX_API_KEY", "CLIPMUX_SIGNING_KID", "CLIPMUX_SIGNING_KEY", "CLIPMUX_WEBHOOK_SECRET"} {
		if vars[k] == "" {
			t.Errorf("%s missing from:\n%s", k, out.String())
		}
	}
	if vars["CLIPMUX_URL"] != "https://video.example.test" {
		t.Errorf("CLIPMUX_URL = %q", vars["CLIPMUX_URL"])
	}
	key, err := apikeys.Authenticate(ctx, pool, vars["CLIPMUX_API_KEY"])
	if err != nil || !key.Can(apikeys.AssetsWrite) {
		t.Fatalf("printed key: %+v, %v", key, err)
	}
	pem := strings.ReplaceAll(strings.Trim(vars["CLIPMUX_SIGNING_KEY"], `"`), `\n`, "\n")
	if _, err := tokens.ParsePrivateKey(pem); err != nil {
		t.Fatalf("printed signing key: %v", err)
	}
}

func TestCreateIntegrationRefusesLoopbackUnlessAllowed(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	none := func(string) string { return "" }
	var out bytes.Buffer
	err := createIntegration(ctx, pool, nil, none, "LMS", "", false, &out)
	if err == nil || !strings.Contains(err.Error(), "--allow-loopback") || out.Len() != 0 {
		t.Fatalf("loopback: %v, printed %q", err, out.String())
	}
	if err := createIntegration(ctx, pool, nil, none, "LMS", "", true, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "CLIPMUX_URL=http://localhost:8080\n") || strings.Contains(out.String(), "CLIPMUX_WEBHOOK_SECRET") {
		t.Fatalf("output:\n%s", out.String())
	}
}
