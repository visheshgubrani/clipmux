package apppairings_test

// Real-Postgres tests for app credentials and app pairing (P6-T23), through
// the real routes: the CLI's side without a session, the owner's with one.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/accounts"
	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/apppairings"
	"github.com/visheshgubrani/clipmux/internal/config"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/secrets"
	"github.com/visheshgubrani/clipmux/internal/settings"
	"github.com/visheshgubrani/clipmux/internal/tokens"
	"github.com/visheshgubrani/clipmux/internal/webhooks"
)

type rig struct {
	t      *testing.T
	pool   *pgxpool.Pool
	box    *secrets.Box
	url    string
	cookie *http.Cookie
}

func newRig(t *testing.T, publicURL string) *rig {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.New(t)
	box, _ := secrets.NewBox(1, bytes.Repeat([]byte{3}, 32))
	urls, err := settings.Load(ctx, pool, "prj_default", config.Config{PublicURL: publicURL, MediaURL: publicURL, URLsPinned: true})
	if err != nil {
		t.Fatal(err)
	}
	acct := accounts.New(pool, "prj_default", urls.PublicURL)
	svc := apppairings.New(pool, box, acct, urls, "prj_default")
	mux := http.NewServeMux()
	svc.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	token, _, _ := acct.SetupToken(ctx)
	_, session, err := acct.CreateOwner(ctx, token, "owner@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	return &rig{t: t, pool: pool, box: box, url: srv.URL, cookie: &http.Cookie{Name: "clipmux_session", Value: session}}
}

func (r *rig) call(method, path string, body any, owner bool) (int, map[string]any) {
	r.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, r.url+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if owner {
		req.AddCookie(r.cookie)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func (r *rig) owner(method, path string, body any) (int, map[string]any) {
	r.t.Helper()
	return r.call(method, path, body, true)
}

func (r *rig) start() (code, poll string) {
	r.t.Helper()
	status, out := r.call("POST", "/v1/app-pairings", map[string]string{
		"name": "lms", "hostname": "dev-laptop", "client": "@clipmux/node 0.1.0",
	}, false)
	if status != http.StatusCreated {
		r.t.Fatalf("start: %d %v", status, out)
	}
	return out["code"].(string), out["poll_token"].(string)
}

func (r *rig) poll(token string) (int, map[string]any) {
	r.t.Helper()
	return r.call("POST", "/v1/app-pairings/poll", map[string]string{"poll_token": token}, false)
}

func (r *rig) count(sql string, args ...any) int {
	r.t.Helper()
	var n int
	if err := r.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		r.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (r *rig) exec(sql string, args ...any) {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(), sql, args...); err != nil {
		r.t.Fatalf("%s: %v", sql, err)
	}
}

// env parses a .env block's KEY=value lines.
func env(block string) map[string]string {
	out := map[string]string{}
	for line := range strings.SplitSeq(block, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") {
			out[k] = v
		}
	}
	return out
}

// checkCredentials proves each secret works: the API key authenticates with
// the default scopes, the signing key signs tokens the stored public key
// verifies, and the webhook secret is the endpoint's.
func (r *rig) checkCredentials(c map[string]any, webhook bool) {
	r.t.Helper()
	ctx := context.Background()
	key, err := apikeys.Authenticate(ctx, r.pool, c["api_key"].(string))
	if err != nil || key.AppID != c["app_id"] || strings.Join(key.Scopes, ",") != "uploads:write,assets:read,assets:write" {
		r.t.Fatalf("api key: %+v, %v", key, err)
	}
	kid := c["signing_kid"].(string)
	priv, err := tokens.ParsePrivateKey(c["signing_key"].(string))
	if err != nil {
		r.t.Fatal(err)
	}
	var publicPEM string
	if err := r.pool.QueryRow(ctx, `SELECT public_key_pem FROM signing_keys WHERE id = $1 AND app_id = $2`, kid, c["app_id"]).Scan(&publicPEM); err != nil {
		r.t.Fatal(err)
	}
	pub, _ := tokens.ParsePublicKey(publicPEM)
	tok, _ := tokens.Sign(priv, kid, tokens.Claims{Sub: "pid_1", Aud: "v", Iat: 1, Exp: 2})
	if p, err := tokens.Parse(tok); err != nil || p.Verify(pub) != nil {
		r.t.Fatal("a token signed with the issued key must verify with the stored public key")
	}
	e := env(c["env"].(string))
	want := map[string]string{
		"CLIPMUX_URL": "https://video.example.test", "CLIPMUX_API_KEY": c["api_key"].(string), "CLIPMUX_SIGNING_KID": kid,
		"CLIPMUX_SIGNING_KEY": `"` + strings.ReplaceAll(strings.TrimRight(c["signing_key"].(string), "\n"), "\n", `\n`) + `"`,
	}
	if webhook {
		want["CLIPMUX_WEBHOOK_SECRET"] = c["webhook_secret"].(string)
		var sealed []byte
		if err := r.pool.QueryRow(ctx, `SELECT secret_encrypted FROM webhook_endpoints WHERE id = $1`, c["webhook_endpoint_id"]).Scan(&sealed); err != nil {
			r.t.Fatal(err)
		}
		if got, err := r.box.Open("webhook_endpoints", c["webhook_endpoint_id"].(string), sealed); err != nil || string(got) != c["webhook_secret"] {
			r.t.Fatal("the webhook secret handed over must be the endpoint's")
		}
	}
	if len(e) != len(want) {
		r.t.Errorf("env has %d vars, want %d:\n%s", len(e), len(want), c["env"])
	}
	for k, v := range want {
		if e[k] != v {
			r.t.Errorf("%s = %q, want %q", k, e[k], v)
		}
	}
}

func TestApprovedPairingHandsOverCredentialsOnce(t *testing.T) {
	r := newRig(t, "https://video.example.test")
	code, poll := r.start()

	// The owner sees what asked before deciding; the CLI waits.
	status, p := r.owner("GET", "/v1/app-pairings/"+strings.ToLower(code), nil)
	if status != http.StatusOK || p["status"] != "pending" || p["hostname"] != "dev-laptop" || p["client"] != "@clipmux/node 0.1.0" {
		t.Fatalf("pairing as the owner sees it: %d %v", status, p)
	}
	if status, res := r.poll(poll); status != http.StatusOK || res["status"] != "pending" || res["credentials"] != nil {
		t.Fatalf("poll before approval: %d %v", status, res)
	}

	status, p = r.owner("POST", "/v1/app-pairings/"+code+"/approve", map[string]any{
		"new_app_name": "Course site", "webhook_url": "https://lms.example.test/api/clipmux/webhook",
	})
	if status != http.StatusOK || p["status"] != "approved" || p["app_id"] == "" {
		t.Fatalf("approve: %d %v", status, p)
	}
	if n := r.count(`SELECT count(*) FROM app_pairings WHERE credentials_sealed IS NOT NULL`); n != 1 {
		t.Fatalf("sealed credentials waiting = %d, want 1", n)
	}

	status, res := r.poll(poll)
	if status != http.StatusOK || res["status"] != "approved" {
		t.Fatalf("collecting poll: %d %v", status, res)
	}
	c := res["credentials"].(map[string]any)
	if c["app_id"] != p["app_id"] || c["app_name"] != "Course site" {
		t.Fatalf("credentials are for %v, want the approved app %v", c["app_id"], p["app_id"])
	}
	r.checkCredentials(c, true)

	// Never twice: the sealed copy is gone and a second poll finds nothing.
	if n := r.count(`SELECT count(*) FROM app_pairings WHERE credentials_sealed IS NOT NULL`); n != 0 {
		t.Fatalf("sealed credentials left after collection = %d", n)
	}
	if status, res := r.poll(poll); status != http.StatusNotFound || res["credentials"] != nil {
		t.Fatalf("second poll: %d %v", status, res)
	}
	// Approving again changes nothing and issues no second key.
	if status, p := r.owner("POST", "/v1/app-pairings/"+code+"/approve", map[string]any{"new_app_name": "Other"}); status != http.StatusOK || p["status"] != "consumed" {
		t.Fatalf("approve twice: %d %v", status, p)
	}
	if n := r.count(`SELECT count(*) FROM api_keys`); n != 1 {
		t.Fatalf("api keys = %d, want 1", n)
	}
}

func TestPairingForAnExistingAppWithoutWebhook(t *testing.T) {
	r := newRig(t, "https://video.example.test")
	if status, out := r.owner("POST", "/v1/app-pairings/XXXX-XXXX/approve", map[string]any{"new_app_name": "LMS"}); status != http.StatusNotFound || out["error"].(map[string]any)["code"] != "pairing_not_found" {
		t.Fatalf("unknown code: %d %v", status, out)
	}
	r.exec(`INSERT INTO apps (id, project_id, name) VALUES ('app_lms', 'prj_default', 'LMS')`)
	code, poll := r.start()
	if status, p := r.owner("POST", "/v1/app-pairings/"+code+"/approve", map[string]any{"app_id": "app_lms"}); status != http.StatusOK || p["app_id"] != "app_lms" {
		t.Fatalf("approve: %d %v", status, p)
	}
	_, res := r.poll(poll)
	c := res["credentials"].(map[string]any)
	if c["app_id"] != "app_lms" || c["webhook_secret"] != nil {
		t.Fatalf("credentials: %v", c)
	}
	r.checkCredentials(c, false)
	// The key is named after what asked for it, so the owner knows what to revoke.
	if n := r.count(`SELECT count(*) FROM api_keys WHERE app_id = 'app_lms' AND name = 'lms'`); n != 1 {
		t.Fatalf("keys named lms = %d", n)
	}
}

func TestApprovalNeedsOneAppAndARealOne(t *testing.T) {
	r := newRig(t, "https://video.example.test")
	r.exec(`INSERT INTO apps (id, project_id, name) VALUES ('app_lms', 'prj_default', 'LMS')`)
	code, _ := r.start()
	for _, tc := range []struct {
		body   map[string]any
		status int
		code   string
	}{
		{map[string]any{}, http.StatusBadRequest, "invalid_request"},
		{map[string]any{"app_id": "app_lms", "new_app_name": "Both"}, http.StatusBadRequest, "invalid_request"},
		{map[string]any{"app_id": "app_nope"}, http.StatusNotFound, "not_found"},
		{map[string]any{"new_app_name": "LMS"}, http.StatusConflict, "name_taken"},
		{map[string]any{"app_id": "app_lms", "webhook_url": "ftp://lms.example.test"}, http.StatusBadRequest, "invalid_request"},
	} {
		status, out := r.owner("POST", "/v1/app-pairings/"+code+"/approve", tc.body)
		if status != tc.status || out["error"].(map[string]any)["code"] != tc.code {
			t.Errorf("approve %v: %d %v, want %d %s", tc.body, status, out, tc.status, tc.code)
		}
	}
	// None of the refusals left a key, a signing key or an app behind.
	if n := r.count(`SELECT (SELECT count(*) FROM api_keys) + (SELECT count(*) FROM signing_keys) + (SELECT count(*) FROM apps WHERE name <> 'LMS' AND NOT is_builtin)`); n != 0 {
		t.Fatalf("refused approvals left %d rows", n)
	}
	if status, p := r.owner("GET", "/v1/app-pairings/"+code, nil); status != http.StatusOK || p["status"] != "pending" {
		t.Fatalf("after refusals: %d %v", status, p)
	}
}

func TestDeniedPairingNeverYieldsCredentials(t *testing.T) {
	r := newRig(t, "https://video.example.test")
	code, poll := r.start()
	if status, p := r.owner("POST", "/v1/app-pairings/"+code+"/deny", nil); status != http.StatusOK || p["status"] != "denied" {
		t.Fatalf("deny: %d %v", status, p)
	}
	if status, res := r.poll(poll); status != http.StatusOK || res["status"] != "denied" {
		t.Fatalf("poll: %d %v", status, res)
	}
	if status, p := r.owner("POST", "/v1/app-pairings/"+code+"/approve", map[string]any{"new_app_name": "Late"}); status != http.StatusOK || p["status"] != "denied" {
		t.Fatalf("approve after deny: %d %v", status, p)
	}
	if n := r.count(`SELECT count(*) FROM apps WHERE name = 'Late'`); n != 0 {
		t.Fatal("approving a denied pairing must not create its app")
	}
}

func TestPairingsExpire(t *testing.T) {
	r := newRig(t, "https://video.example.test")

	// Expired while pending: nothing can approve it.
	code, poll := r.start()
	if n := r.count(`SELECT count(*) FROM app_pairings WHERE expires_at BETWEEN now() + interval '9 minutes' AND now() + interval '10 minutes'`); n != 1 {
		t.Fatalf("a pairing must last 10 minutes; %d do", n)
	}
	r.exec(`UPDATE app_pairings SET expires_at = now() - interval '1 second'`)
	if status, res := r.poll(poll); status != http.StatusOK || res["status"] != "expired" {
		t.Fatalf("poll: %d %v", status, res)
	}
	if _, p := r.owner("GET", "/v1/app-pairings/"+code, nil); p["status"] != "expired" {
		t.Fatalf("owner view: %v", p)
	}
	if status, p := r.owner("POST", "/v1/app-pairings/"+code+"/approve", map[string]any{"new_app_name": "Late"}); status != http.StatusOK || p["status"] != "expired" {
		t.Fatalf("approve expired: %d %v", status, p)
	}

	// Approved but never collected: the sealed credentials are dropped.
	code, poll = r.start()
	r.owner("POST", "/v1/app-pairings/"+code+"/approve", map[string]any{"new_app_name": "Course site"})
	r.exec(`UPDATE app_pairings SET expires_at = now() - interval '1 second' WHERE credentials_sealed IS NOT NULL`)
	if status, res := r.poll(poll); status != http.StatusOK || res["status"] != "expired" || res["credentials"] != nil {
		t.Fatalf("poll after expiry: %d %v", status, res)
	}
	if n := r.count(`SELECT count(*) FROM app_pairings WHERE credentials_sealed IS NOT NULL`); n != 0 {
		t.Fatal("expired credentials must not be kept")
	}
}

func TestPairingRoutesAreRateLimited(t *testing.T) {
	r := newRig(t, "https://video.example.test")

	// At most 10 wait for the owner at once.
	var poll string
	for range 10 {
		_, poll = r.start()
	}
	status, out := r.call("POST", "/v1/app-pairings", map[string]string{"name": "eleventh"}, false)
	if status != http.StatusTooManyRequests || out["error"].(map[string]any)["code"] != "too_many_pairings" {
		t.Fatalf("eleventh pending pairing: %d %v", status, out)
	}

	// An address starts 20 an hour, as /worker/v1/pairings (D067).
	r.exec(`UPDATE app_pairings SET status = 'denied'`)
	for range 9 { // the refused eleventh counted too
		r.start()
	}
	status, out = r.call("POST", "/v1/app-pairings", map[string]string{"name": "twenty-first"}, false)
	if status != http.StatusTooManyRequests || out["error"].(map[string]any)["code"] != "rate_limited" {
		t.Fatalf("twenty-first start in the hour: %d %v", status, out)
	}

	// And polls 2 a second, with a burst of 4.
	for i := range 4 {
		if status, _ := r.poll(poll); status != http.StatusOK {
			t.Fatalf("poll %d: %d", i+1, status)
		}
	}
	if status, out := r.poll(poll); status != http.StatusTooManyRequests || out["error"].(map[string]any)["code"] != "rate_limited" {
		t.Fatalf("fifth poll at once: %d %v", status, out)
	}
}

func TestOwnerRoutesNeedTheSession(t *testing.T) {
	r := newRig(t, "https://video.example.test")
	code, _ := r.start()
	for _, path := range []string{"/v1/app-pairings/" + code + "/approve", "/v1/app-pairings/" + code + "/deny", "/v1/apps/app_x/credentials"} {
		if status, _ := r.call("POST", path, map[string]any{"new_app_name": "Sneaky"}, false); status != http.StatusUnauthorized {
			t.Errorf("POST %s without a session: %d, want 401", path, status)
		}
	}
	if status, _ := r.call("GET", "/v1/app-pairings/"+code, nil, false); status != http.StatusUnauthorized {
		t.Errorf("GET without a session: %d, want 401", status)
	}
}

// While the public URL is loopback, credentials need the owner's
// confirmation that they are for apps on this machine (D108), whichever way
// they are issued.
func TestLoopbackCredentialsNeedConfirmation(t *testing.T) {
	r := newRig(t, "http://localhost:8080")
	r.exec(`INSERT INTO apps (id, project_id, name) VALUES ('app_lms', 'prj_default', 'LMS')`)

	status, out := r.owner("POST", "/v1/apps/app_lms/credentials", map[string]any{})
	if status != http.StatusConflict || out["error"].(map[string]any)["code"] != "public_url_required" {
		t.Fatalf("credentials on loopback: %d %v", status, out)
	}
	code, poll := r.start()
	status, out = r.owner("POST", "/v1/app-pairings/"+code+"/approve", map[string]any{"app_id": "app_lms"})
	if status != http.StatusConflict || out["error"].(map[string]any)["code"] != "public_url_required" {
		t.Fatalf("approval on loopback: %d %v", status, out)
	}
	if n := r.count(`SELECT count(*) FROM api_keys`); n != 0 {
		t.Fatalf("refused requests created %d keys", n)
	}

	status, c := r.owner("POST", "/v1/apps/app_lms/credentials", map[string]any{"local": true})
	if status != http.StatusCreated || c["url"] != "http://localhost:8080" || !strings.HasPrefix(c["api_key"].(string), "cmx_") {
		t.Fatalf("confirmed credentials: %d %v", status, c)
	}
	if status, p := r.owner("POST", "/v1/app-pairings/"+code+"/approve", map[string]any{"app_id": "app_lms", "local": true}); status != http.StatusOK || p["status"] != "approved" {
		t.Fatalf("confirmed approval: %d %v", status, p)
	}
	if _, res := r.poll(poll); res["status"] != "approved" {
		t.Fatalf("poll: %v", res)
	}
}

func TestCredentialsRouteIssuesTheBundleInOneTransaction(t *testing.T) {
	r := newRig(t, "https://video.example.test")
	r.exec(`INSERT INTO apps (id, project_id, name) VALUES ('app_lms', 'prj_default', 'LMS')`)

	// A refused webhook URL leaves no key or signing key behind.
	status, out := r.owner("POST", "/v1/apps/app_lms/credentials", map[string]any{"webhook_url": "not a url"})
	if status != http.StatusBadRequest || out["error"].(map[string]any)["message"] != webhooks.ErrInvalidURL.Error() {
		t.Fatalf("bad webhook: %d %v", status, out)
	}
	if n := r.count(`SELECT (SELECT count(*) FROM api_keys) + (SELECT count(*) FROM signing_keys)`); n != 0 {
		t.Fatalf("a refused bundle left %d keys", n)
	}
	if status, _ := r.owner("POST", "/v1/apps/app_nope/credentials", map[string]any{}); status != http.StatusNotFound {
		t.Fatalf("unknown app: %d", status)
	}

	status, c := r.owner("POST", "/v1/apps/app_lms/credentials", map[string]any{"webhook_url": "https://lms.example.test/hooks"})
	if status != http.StatusCreated {
		t.Fatalf("credentials: %d %v", status, c)
	}
	r.checkCredentials(c, true)
	if n := r.count(`SELECT count(*) FROM api_keys WHERE app_id = 'app_lms' AND name = 'integration'`); n != 1 {
		t.Fatalf("keys named integration = %d", n)
	}
	// The Worker learns of the signing key (_keys.json) from the same transaction.
	if n := r.count(`SELECT count(*) FROM outbox WHERE kind = 'keys_sync'`); n < 1 {
		t.Fatal("issuing a signing key must queue _keys.json")
	}
}
