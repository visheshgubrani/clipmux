package manage_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/accounts"
	"github.com/visheshgubrani/clipmux/internal/analytics"
	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/assets"
	"github.com/visheshgubrani/clipmux/internal/chapters"
	"github.com/visheshgubrani/clipmux/internal/config"
	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/delivery"
	"github.com/visheshgubrani/clipmux/internal/manage"
	"github.com/visheshgubrani/clipmux/internal/secrets"
	"github.com/visheshgubrani/clipmux/internal/settings"
	"github.com/visheshgubrani/clipmux/internal/signingkeys"
	"github.com/visheshgubrani/clipmux/internal/speechapi"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/stores"
	"github.com/visheshgubrani/clipmux/internal/tokens"
	"github.com/visheshgubrani/clipmux/internal/uploads"
)

type rig struct {
	t      *testing.T
	pool   *pgxpool.Pool
	url    string
	cookie *http.Cookie
	box    *secrets.Box
	m      *manage.API
}

func newRig(t *testing.T, cfg config.Config) *rig {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.New(t)
	box, _ := secrets.NewBox(1, bytes.Repeat([]byte{2}, 32))
	if cfg.PublicURL == "" {
		cfg.PublicURL, cfg.MediaURL = config.DefaultPublicURL, config.DefaultPublicURL
	}
	urls, err := settings.Load(ctx, pool, "prj_default", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := signingkeys.EnsureInternal(ctx, pool, box, "prj_default"); err != nil {
		t.Fatal(err)
	}
	store, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: "http://unused"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	acct := accounts.New(pool, "prj_default", urls.PublicURL)
	m := manage.New(pool, box, acct, urls)
	m.Speech = speechapi.NewService(pool, box, "prj_default")
	m.Chapters = chapters.NewService(pool, box, "prj_default")
	// Cloudflare's Analytics Engine SQL API, accepting one token.
	sqlAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cf-analytics-read" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, `{"data":[],"rows":0}`)
	}))
	t.Cleanup(sqlAPI.Close)
	m.Edge = analytics.NewEdge(pool, box, "prj_default")
	m.Edge.API = sqlAPI.URL
	m.Stores = stores.New(pool, box, store)
	upl := uploads.New(pool, stores.Fixed(store))
	upl.Auth.Session = m.ActAsApp
	a := assets.New(pool)
	a.Auth.Session = m.ActAsApp
	keys, _ := contentkey.FromHex(1, strings.Repeat("00", 32))
	mux := http.NewServeMux()
	for _, reg := range []interface{ Register(*http.ServeMux) }{acct, m, upl, a, delivery.New(pool, stores.Fixed(store), contentkey.NewRing(keys))} {
		reg.Register(mux)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	token, _, _ := acct.SetupToken(ctx)
	_, session, err := acct.CreateOwner(ctx, token, "owner@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	return &rig{t: t, pool: pool, url: srv.URL, box: box, m: m, cookie: &http.Cookie{Name: "clipmux_session", Value: session}}
}

type res struct {
	status int
	body   map[string]any
	list   []any
}

// call sends a request as the owner (session cookie), or with a bearer key.
func (r *rig) call(method, path string, body any, bearer string, header ...string) res {
	r.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, r.url+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	} else {
		req.AddCookie(r.cookie)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	hres, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer hres.Body.Close()
	raw, _ := io.ReadAll(hres.Body)
	out := res{status: hres.StatusCode}
	_ = json.Unmarshal(raw, &out.body)
	if d, ok := out.body["data"].([]any); ok {
		out.list = d
	}
	return out
}

func (r *rig) exec(sql string, args ...any) {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(), sql, args...); err != nil {
		r.t.Fatalf("%s: %v", sql, err)
	}
}

func code(r res) string {
	e, _ := r.body["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

func (r *rig) setPublicURL() {
	r.t.Helper()
	if got := r.call("PUT", "/v1/settings/urls", map[string]string{"public_url": "https://video.example.com", "media_url": "https://media.example.net"}, ""); got.status != 200 {
		r.t.Fatalf("set URLs = %d %v", got.status, got.body)
	}
}

func TestIntegrationKeyCannotManageApps(t *testing.T) {
	r := newRig(t, config.Config{})
	key, _, _ := apikeys.Create(context.Background(), r.pool, "prj_default", "app_dashboard", "backend", apikeys.AllScopes())
	for _, c := range [][2]string{{"GET", "/v1/apps"}, {"POST", "/v1/apps"}, {"GET", "/v1/library"}, {"PUT", "/v1/settings/urls"}} {
		if got := r.call(c[0], c[1], map[string]string{"name": "x"}, key); got.status != http.StatusUnauthorized {
			t.Errorf("%s %s with an API key = %d, want 401", c[0], c[1], got.status)
		}
	}
	if got := r.call("GET", "/v1/apps", nil, ""); got.status != 200 || len(got.list) != 1 {
		t.Fatalf("owner GET /v1/apps = %d %v, want the Dashboard app", got.status, got.body)
	}
}

func TestSetupReportsWhatTheWizardStillNeeds(t *testing.T) {
	r := newRig(t, config.Config{})
	req, _ := http.NewRequest("GET", r.url+"/v1/setup", nil)
	hres, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer hres.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(hres.Body).Decode(&body)
	want := map[string]any{
		"owner_exists": true, "keys_allowed": false,
		"urls": map[string]any{"public_url": "http://localhost:8080", "media_url": "http://localhost:8080", "confirmed": false, "pinned": false},
	}
	for k, v := range want {
		if !reflect.DeepEqual(body[k], v) {
			t.Errorf("GET /v1/setup %s = %v, want %v", k, body[k], v)
		}
	}
	pinned := newRig(t, config.Config{PublicURL: "https://video.example.com", MediaURL: "https://video.example.com", URLsPinned: true})
	if got := pinned.call("PUT", "/v1/settings/urls", map[string]string{"public_url": "https://other.example.com"}, ""); got.status != http.StatusConflict || code(got) != "pinned" {
		t.Fatalf("saving pinned URLs = %d %v, want 409 pinned", got.status, got.body)
	}
}

// The Dashboard app's uploads are what an unsandboxed agent may encode, so
// nothing but the owner's own browser may create them (finding 3, D123).
func TestTheDashboardAppTakesNoAPIKeys(t *testing.T) {
	r := newRig(t, config.Config{})
	r.setPublicURL()
	if got := r.call("POST", "/v1/apps/app_dashboard/api-keys", map[string]any{"name": "backend"}, ""); got.status != http.StatusConflict || code(got) != "builtin_app" {
		t.Fatalf("key for the Dashboard app = %d %v, want 409 builtin_app", got.status, got.body)
	}
}

func TestAPIKeysWaitForAPublicURLAndAreShownOnce(t *testing.T) {
	r := newRig(t, config.Config{})
	app := r.call("POST", "/v1/apps", map[string]string{"name": "LMS"}, "")
	appID, _ := app.body["id"].(string)
	if app.status != http.StatusCreated || appID == "" || app.body["name"] != "LMS" {
		t.Fatalf("create app = %d %v", app.status, app.body)
	}
	if got := r.call("POST", "/v1/apps/LMS-dup/api-keys", map[string]any{"name": "backend"}, ""); got.status != http.StatusNotFound {
		t.Fatalf("key for an unknown app = %d, want 404", got.status)
	}
	if got := r.call("POST", "/v1/apps/"+appID+"/api-keys", map[string]any{"name": "backend"}, ""); got.status != http.StatusConflict || code(got) != "public_url_required" {
		t.Fatalf("key while the public URL is localhost = %d %v, want 409 public_url_required", got.status, got.body)
	}
	local := r.call("POST", "/v1/apps/"+appID+"/api-keys", map[string]any{"name": "laptop", "local": true}, "")
	if key, _ := local.body["key"].(string); local.status != http.StatusCreated || !strings.HasPrefix(key, "cmx_") {
		t.Fatalf("a key confirmed for this machine while the public URL is localhost = %d %v, want 201", local.status, local.body)
	}
	r.exec(`DELETE FROM api_keys WHERE name = 'laptop'`)
	r.setPublicURL()
	created := r.call("POST", "/v1/apps/"+appID+"/api-keys", map[string]any{"name": "backend", "scopes": []string{"assets:read"}}, "")
	secret, _ := created.body["key"].(string)
	if created.status != http.StatusCreated || !strings.HasPrefix(secret, "cmx_") {
		t.Fatalf("create key = %d %v", created.status, created.body)
	}
	listed := r.call("GET", "/v1/apps/"+appID+"/api-keys", nil, "")
	raw, _ := json.Marshal(listed.body)
	if listed.status != 200 || len(listed.list) != 1 || strings.Contains(string(raw), secret) {
		t.Fatalf("listed keys = %s; want one, never its secret", raw)
	}
	if k := listed.list[0].(map[string]any); k["name"] != "backend" || !reflect.DeepEqual(k["scopes"], []any{"assets:read"}) {
		t.Fatalf("listed key = %v, want its name and scopes", k)
	}
	if got := r.call("POST", "/v1/apps/"+appID+"/api-keys", map[string]any{"name": strings.Repeat("k", 101)}, ""); got.status != http.StatusBadRequest {
		t.Fatalf("a 101-character key name = %d, want 400", got.status)
	}
	if got := r.call("POST", "/v1/apps/"+appID+"/api-keys", map[string]any{"name": "  "}, ""); got.status != http.StatusCreated || got.body["name"] != "default" {
		t.Fatalf("a blank key name = %d %v, want 201 named default", got.status, got.body)
	}
	r.exec(`DELETE FROM api_keys WHERE name = 'default'`)
	if got := r.call("GET", "/v1/assets?updated_since=2020-01-01T00:00:00Z", nil, secret); got.status != 200 {
		t.Fatalf("the new key authenticates: %d", got.status)
	}
	keyID, _ := created.body["id"].(string)
	if got := r.call("DELETE", "/v1/api-keys/"+keyID, nil, ""); got.status != http.StatusNoContent {
		t.Fatalf("revoke = %d", got.status)
	}
	if got := r.call("GET", "/v1/assets?updated_since=2020-01-01T00:00:00Z", nil, secret); got.status != http.StatusUnauthorized {
		t.Fatalf("a revoked key = %d, want 401", got.status)
	}
}

func TestSigningKeysAndWebhookEndpointsShowTheirSecretOnce(t *testing.T) {
	r := newRig(t, config.Config{})
	sk := r.call("POST", "/v1/apps/app_dashboard/signing-keys", nil, "")
	pem, _ := sk.body["private_key_pem"].(string)
	if _, err := tokens.ParsePrivateKey(pem); sk.status != http.StatusCreated || err != nil {
		t.Fatalf("create signing key = %d %v", sk.status, sk.body)
	}
	if got := r.call("GET", "/v1/apps/app_dashboard/signing-keys", nil, ""); len(got.list) != 1 || strings.Contains(r.mustJSON(got.body), "PRIVATE") {
		t.Fatalf("listed signing keys = %v", got.body)
	}
	if got := r.call("DELETE", "/v1/signing-keys/"+sk.body["kid"].(string), nil, ""); got.status != http.StatusNoContent {
		t.Fatalf("revoke signing key = %d", got.status)
	}
	ep := r.call("POST", "/v1/apps/app_dashboard/webhook-endpoints", map[string]string{"url": "https://lms.example.com/hooks"}, "")
	whsec, _ := ep.body["secret"].(string)
	if ep.status != http.StatusCreated || !strings.HasPrefix(whsec, "whsec_") {
		t.Fatalf("create endpoint = %d %v", ep.status, ep.body)
	}
	if got := r.call("GET", "/v1/apps/app_dashboard/webhook-endpoints", nil, ""); len(got.list) != 1 || strings.Contains(r.mustJSON(got.body), whsec) {
		t.Fatalf("listed endpoints = %v; never the secret", got.body)
	}
	if got := r.call("DELETE", "/v1/webhook-endpoints/"+ep.body["id"].(string), nil, ""); got.status != http.StatusNoContent {
		t.Fatalf("disable endpoint = %d", got.status)
	}
	if got := r.call("POST", "/v1/apps/app_dashboard/webhook-endpoints", map[string]string{"url": "ftp://x"}, ""); got.status != http.StatusBadRequest {
		t.Fatalf("a non-http endpoint = %d, want 400", got.status)
	}
}

func (r *rig) mustJSON(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

func TestOwnerUploadsLandInTheDashboardAppOrTheNamedOne(t *testing.T) {
	r := newRig(t, config.Config{})
	lms := r.call("POST", "/v1/apps", map[string]string{"name": "LMS"}, "").body["id"].(string)
	appOf := func(created res) string {
		r.t.Helper()
		var app string
		if err := r.pool.QueryRow(context.Background(), `SELECT app_id FROM assets WHERE id = $1`, created.body["asset_id"]).Scan(&app); err != nil {
			r.t.Fatalf("upload = %d %v: %v", created.status, created.body, err)
		}
		return app
	}
	if got := appOf(r.call("POST", "/v1/uploads", map[string]any{"size": 10}, "")); got != "app_dashboard" {
		t.Fatalf("a dashboard upload went to %s, want app_dashboard", got)
	}
	if got := appOf(r.call("POST", "/v1/uploads", map[string]any{"size": 10}, "", "ClipMux-App", lms)); got != lms {
		t.Fatalf("an upload for LMS went to %s", got)
	}
	if got := r.call("POST", "/v1/uploads", map[string]any{"size": 10}, "", "ClipMux-App", "app_nope"); got.status != http.StatusNotFound {
		t.Fatalf("an upload for an unknown app = %d, want 404", got.status)
	}
	if got := r.call("POST", "/v1/uploads", map[string]any{"size": 10}, "", "Sec-Fetch-Site", "cross-site"); got.status != http.StatusForbidden {
		t.Fatalf("a cross-site upload with the session = %d, want 403", got.status)
	}
}

func TestDisablingAnAppStopsItsKeysAndPlayback(t *testing.T) {
	r := newRig(t, config.Config{})
	r.setPublicURL()
	lms := r.call("POST", "/v1/apps", map[string]string{"name": "LMS"}, "").body["id"].(string)
	key := r.call("POST", "/v1/apps/"+lms+"/api-keys", map[string]any{"name": "backend"}, "").body["key"].(string)
	r.exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_1', 'prj_default', $1, 'processing', 'sto_default')`, lms)
	r.exec(`INSERT INTO outputs (id, project_id, asset_id, key_version, status, manifest) VALUES ('out_1', 'prj_default', 'ast_1', 1, 'committed', '{}');
	        UPDATE assets SET status = 'ready', current_output_id = 'out_1';
	        INSERT INTO playback_ids (id, project_id, asset_id, policy) VALUES ('pid_1', 'prj_default', 'ast_1', 'public')`)
	if got := r.call("POST", "/v1/apps/"+lms+"/disable", nil, ""); got.status != 200 || got.body["disabled"] != true {
		t.Fatalf("disable = %d %v", got.status, got.body)
	}
	if got := r.call("GET", "/v1/assets/ast_1", nil, key); got.status != http.StatusUnauthorized {
		t.Fatalf("a disabled app's key = %d, want 401", got.status)
	}
	if got := r.call("GET", "/p/pid_1/master.m3u8", nil, "x"); got.status != http.StatusForbidden {
		t.Fatalf("a disabled app's playback = %d, want 403", got.status)
	}
	if got := r.call("POST", "/v1/apps/"+lms+"/enable", nil, ""); got.status != 200 || got.body["disabled"] != false {
		t.Fatalf("enable = %d %v", got.status, got.body)
	}
	if got := r.call("POST", "/v1/apps/app_dashboard/disable", nil, ""); got.status != http.StatusConflict {
		t.Fatalf("disabling the Dashboard app = %d, want 409", got.status)
	}
}

func TestLibraryListsEveryAppNewestFirstWithCursor(t *testing.T) {
	r := newRig(t, config.Config{})
	r.exec(`INSERT INTO apps (id, project_id, name) VALUES ('app_lms', 'prj_default', 'LMS');
	        INSERT INTO assets (id, project_id, app_id, status, error_code, storage_id, created_at) VALUES
	          ('ast_a', 'prj_default', 'app_dashboard', 'processing', NULL, 'sto_default', '2026-11-05T10:00:00Z'),
	          ('ast_b', 'prj_default', 'app_lms', 'errored', 'upload_expired', 'sto_default', '2026-11-05T10:01:00Z'),
	          ('ast_c', 'prj_default', 'app_lms', 'processing', NULL, 'sto_default', '2026-11-05T10:02:00Z')`)
	ids := func(got res) []string {
		var out []string
		for _, a := range got.list {
			m := a.(map[string]any)
			out = append(out, m["id"].(string)+"/"+m["app_id"].(string))
		}
		return out
	}
	first := r.call("GET", "/v1/library?limit=2", nil, "")
	if want := []string{"ast_c/app_lms", "ast_b/app_lms"}; !reflect.DeepEqual(ids(first), want) || first.body["next_cursor"] == nil {
		t.Fatalf("first page = %v %v, want %v and a cursor", ids(first), first.body, want)
	}
	second := r.call("GET", "/v1/library?limit=2&cursor="+first.body["next_cursor"].(string), nil, "")
	if want := []string{"ast_a/app_dashboard"}; !reflect.DeepEqual(ids(second), want) || second.body["next_cursor"] != nil {
		t.Fatalf("second page = %v, want %v and no cursor", ids(second), want)
	}
	if got := ids(r.call("GET", "/v1/library?app_id=app_lms&status=errored", nil, "")); !reflect.DeepEqual(got, []string{"ast_b/app_lms"}) {
		t.Fatalf("filtered = %v", got)
	}
}

func TestPreviewTokenIsSignedByTheInternalKeyForAnHour(t *testing.T) {
	r := newRig(t, config.Config{})
	r.exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default');
	        INSERT INTO playback_ids (id, project_id, asset_id, policy) VALUES ('pid_s', 'prj_default', 'ast_1', 'signed')`)
	got := r.call("POST", "/v1/assets/ast_1/preview-token", nil, "")
	tok, _ := got.body["token"].(string)
	parsed, err := tokens.Parse(tok)
	if got.status != 200 || err != nil {
		t.Fatalf("preview token = %d %v (%v)", got.status, got.body, err)
	}
	var internal string
	_ = r.pool.QueryRow(context.Background(), `SELECT id FROM signing_keys WHERE app_id IS NULL`).Scan(&internal)
	if parsed.Kid != internal || parsed.Claims.Sub != "pid_s" || parsed.Claims.Aud != "v" || parsed.Claims.Exp-parsed.Claims.Iat != 3600 {
		t.Fatalf("token kid %s claims %+v; want the internal key, pid_s, aud v, one hour", parsed.Kid, parsed.Claims)
	}
	if got.body["playback_id"] != "pid_s" || !strings.HasSuffix(got.body["url"].(string), "/v/"+tok+"/pid_s/master.m3u8") {
		t.Fatalf("preview body = %v", got.body)
	}
	r.exec(`UPDATE playback_ids SET status = 'revoked', revoked_at = now()`)
	if got := r.call("POST", "/v1/assets/ast_1/preview-token", nil, ""); got.status != http.StatusConflict {
		t.Fatalf("an asset with no playback ID = %d, want 409", got.status)
	}
}

func TestTheJobsPageShowsWhereEachAttemptRanAndOnWhat(t *testing.T) {
	r := newRig(t, config.Config{})
	r.exec(`INSERT INTO agents (id, project_id, name, key_hash, status) VALUES ('agt_pc', 'prj_default', 'Studio PC', 'h', 'approved')`)
	r.exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default')`)
	r.exec(`INSERT INTO jobs (id, project_id, asset_id, kind, routed_to, route_reason, status, lost_count) VALUES ('job_1', 'prj_default', 'ast_1', 'transcode', 'builtin', 'primary_offline', 'running', 1)`)
	r.exec(`INSERT INTO outputs (id, project_id, asset_id, key_version, status) VALUES ('out_a', 'prj_default', 'ast_1', 1, 'writing')`)
	r.exec(`INSERT INTO outputs (id, project_id, asset_id, key_version, status, manifest) VALUES ('out_b', 'prj_default', 'ast_1', 1, 'committed', '{"encoder": {"stage": "cpu", "fallbacks": ["vaapi"]}}')`)
	r.exec(`INSERT INTO attempts (id, project_id, job_id, asset_id, attempt_number, output_id, executor, agent_id, status, token_hash, lease_expires_at, route_reason)
		VALUES ('att_1', 'prj_default', 'job_1', 'ast_1', 1, 'out_a', 'agent', 'agt_pc', 'lost', 'h1', now(), 'primary'),
		       ('att_2', 'prj_default', 'job_1', 'ast_1', 2, 'out_b', 'builtin', NULL, 'running', 'h2', now(), 'primary_offline')`)

	res := r.call("GET", "/v1/jobs?agent_id=agt_pc", nil, "")
	if res.status != http.StatusOK || len(res.list) != 1 {
		t.Fatalf("jobs = %d %v", res.status, res.body)
	}
	job := res.list[0].(map[string]any)
	attempts := job["attempts"].([]any)
	first, second := attempts[0].(map[string]any), attempts[1].(map[string]any)
	if job["lost_count"] != float64(1) || first["agent_name"] != "Studio PC" || first["status"] != "lost" || second["executor"] != "builtin" {
		t.Fatalf("job = %v", job)
	}
	// Why: the job is routed to the server because the PC went away, and
	// each attempt keeps the reason it ran where it did.
	if job["routed_to"] != "builtin" || job["route_reason"] != "primary_offline" ||
		first["route_reason"] != "primary" || second["route_reason"] != "primary_offline" {
		t.Fatalf("routes: job %v/%v, attempts %v/%v", job["routed_to"], job["route_reason"], first["route_reason"], second["route_reason"])
	}
	if enc := second["encoder"].(map[string]any); enc["stage"] != "cpu" || enc["fallbacks"].([]any)[0] != "vaapi" {
		t.Fatalf("encoder = %v", second["encoder"])
	}
	if res := r.call("GET", "/v1/jobs/job_1", nil, ""); res.status != http.StatusOK || res.body["id"] != "job_1" {
		t.Fatalf("job = %d %v", res.status, res.body)
	}
	if res := r.call("GET", "/v1/jobs", nil, "cmx_not_a_session"); res.status != http.StatusUnauthorized {
		t.Fatalf("an API key reached the jobs page: %d", res.status)
	}
}

func TestRoutingSettingsChooseAPrimaryAndAFallback(t *testing.T) {
	r := newRig(t, config.Config{})
	got := r.call("GET", "/v1/settings/routing", nil, "")
	if got.status != http.StatusOK || got.body["primary_executor"] != "workers" || got.body["fallback_executor"] != nil ||
		got.body["queue_age_fallback_minutes"] != nil {
		t.Fatalf("default routing = %d %v, want workers with no fallback", got.status, got.body)
	}
	if b := got.body["builtin_worker"].(map[string]any); b["online"] != false {
		t.Fatalf("builtin_worker = %v, want offline before it ever called", b)
	}

	// My PC, else the server, after 10 minutes in the queue.
	got = r.call("PUT", "/v1/settings/routing", map[string]any{
		"primary_executor": "agents", "fallback_executor": "builtin", "queue_age_fallback_minutes": 10}, "")
	if got.status != http.StatusOK || got.body["primary_executor"] != "agents" || got.body["fallback_executor"] != "builtin" ||
		got.body["queue_age_fallback_minutes"] != float64(10) {
		t.Fatalf("saved routing = %d %v", got.status, got.body)
	}
	r.exec(`UPDATE projects SET builtin_seen_at = now() WHERE id = 'prj_default'`)
	if b := r.call("GET", "/v1/settings/routing", nil, "").body["builtin_worker"].(map[string]any); b["online"] != true {
		t.Fatalf("builtin_worker = %v, want online", b)
	}

	refused := []struct {
		body   map[string]any
		status int
		code   string
	}{
		{map[string]any{"primary_executor": "cloud"}, 400, "invalid_request"},
		{map[string]any{"primary_executor": "workers", "fallback_executor": "builtin"}, 400, "invalid_request"},
		{map[string]any{"primary_executor": "builtin", "fallback_executor": "builtin"}, 400, "invalid_request"},
		{map[string]any{"primary_executor": "agents", "queue_age_fallback_minutes": 10}, 400, "invalid_request"},
		{map[string]any{"primary_executor": "agents", "fallback_executor": "builtin", "queue_age_fallback_minutes": 0}, 400, "invalid_request"},
		{map[string]any{"primary_executor": "modal"}, 409, "executor_not_connected"},
		{map[string]any{"primary_executor": "agents", "fallback_executor": "runpod"}, 409, "executor_not_connected"},
	}
	for _, c := range refused {
		got := r.call("PUT", "/v1/settings/routing", c.body, "")
		if errBody, _ := got.body["error"].(map[string]any); got.status != c.status || errBody["code"] != c.code {
			t.Errorf("PUT %v = %d %v, want %d %s", c.body, got.status, got.body, c.status, c.code)
		}
	}
	if got := r.call("GET", "/v1/settings/routing", nil, ""); got.body["primary_executor"] != "agents" {
		t.Fatalf("a refused change was saved: %v", got.body)
	}
	// A provider becomes a choice once it is connected and its setup test
	// passed (D056).
	r.exec(`INSERT INTO executor_configs (id, project_id, kind, secrets_encrypted, monthly_limit_cents, disk_bytes)
		VALUES ('exe_1', 'prj_default', 'runpod', 'sealed', 5000, 53687091200)`)
	runpodFallback := map[string]any{"primary_executor": "agents", "fallback_executor": "runpod"}
	if got := r.call("PUT", "/v1/settings/routing", runpodFallback, ""); got.status != http.StatusConflict {
		t.Fatalf("an unverified provider as the fallback = %d %v, want 409", got.status, got.body)
	}
	r.exec(`UPDATE executor_configs SET verified_at = now()`)
	got = r.call("PUT", "/v1/settings/routing", runpodFallback, "")
	if ready, _ := got.body["providers_ready"].([]any); got.status != http.StatusOK || got.body["fallback_executor"] != "runpod" ||
		len(ready) != 1 || ready[0] != "runpod" {
		t.Fatalf("a verified provider as the fallback = %d %v", got.status, got.body)
	}
	if got := r.call("PUT", "/v1/settings/routing", map[string]any{"primary_executor": "runpod", "fallback_executor": "runpod"}, ""); got.status != http.StatusBadRequest {
		t.Fatalf("a provider as its own fallback = %d, want 400", got.status)
	}
	if got := r.call("PUT", "/v1/settings/routing", map[string]any{"primary_executor": "workers"}, "cmx_not_a_session"); got.status != http.StatusUnauthorized {
		t.Fatalf("an API key changed routing: %d", got.status)
	}
}

// The speech settings (D075): local by default, and a mode that uses the
// API refused until one is connected.
func TestSpeechSettingsNeedAConnectionForTheAPI(t *testing.T) {
	r := newRig(t, config.Config{})
	got := r.call("GET", "/v1/settings/speech", nil, "")
	if got.status != http.StatusOK || got.body["mode"] != "local" || got.body["connection"] != nil {
		t.Fatalf("default speech = %d %v", got.status, got.body)
	}
	for _, c := range []struct {
		method, path string
		body         map[string]any
		status       int
		code         string
	}{
		{"PUT", "/v1/settings/speech", map[string]any{"mode": "api"}, 409, "speech_not_connected"},
		{"PUT", "/v1/settings/speech", map[string]any{"mode": "local_then_api"}, 409, "speech_not_connected"},
		{"PUT", "/v1/settings/speech", map[string]any{"mode": "cloud"}, 400, "invalid_request"},
		{"POST", "/v1/settings/speech/connect", map[string]any{"base_url": "ftp://x", "model": "m"}, 400, "invalid_request"},
		{"POST", "/v1/settings/speech/connect", map[string]any{"base_url": "https://api.example.com/v1"}, 400, "invalid_request"},
		{"DELETE", "/v1/settings/speech/connect", nil, 404, "not_found"},
	} {
		got := r.call(c.method, c.path, c.body, "")
		if errBody, _ := got.body["error"].(map[string]any); got.status != c.status || errBody["code"] != c.code {
			t.Errorf("%s %s %v = %d %v, want %d %s", c.method, c.path, c.body, got.status, got.body, c.status, c.code)
		}
	}
	if got := r.call("PUT", "/v1/settings/speech", map[string]any{"mode": "local"}, ""); got.status != http.StatusOK || got.body["mode"] != "local" {
		t.Fatalf("local = %d %v", got.status, got.body)
	}
	// Only the owner: an integration key can't read or change them.
	key, _, err := apikeys.Create(context.Background(), r.pool, "prj_default", "app_dashboard", "k", apikeys.AllScopes())
	if err != nil {
		t.Fatal(err)
	}
	if got := r.call("GET", "/v1/settings/speech", nil, key); got.status != http.StatusUnauthorized {
		t.Fatalf("an API key read the speech settings: %d", got.status)
	}
}

// The chapters LLM (P5-T17): none by default, Connect checks its input,
// and only the owner reaches it.
func TestChaptersSettingsAreTheOwners(t *testing.T) {
	r := newRig(t, config.Config{})
	got := r.call("GET", "/v1/settings/chapters", nil, "")
	if got.status != http.StatusOK || got.body["connection"] != nil {
		t.Fatalf("default chapters = %d %v", got.status, got.body)
	}
	for _, c := range []struct {
		method, path string
		body         map[string]any
		status       int
		code         string
	}{
		{"POST", "/v1/settings/chapters/connect", map[string]any{"provider": "gemini", "model": "m"}, 400, "invalid_request"},
		{"POST", "/v1/settings/chapters/connect", map[string]any{"provider": "openai_compatible", "base_url": "ftp://x", "model": "m"}, 400, "invalid_request"},
		{"POST", "/v1/settings/chapters/connect", map[string]any{"provider": "anthropic"}, 400, "invalid_request"},
		{"DELETE", "/v1/settings/chapters/connect", nil, 404, "not_found"},
	} {
		got := r.call(c.method, c.path, c.body, "")
		if errBody, _ := got.body["error"].(map[string]any); got.status != c.status || errBody["code"] != c.code {
			t.Errorf("%s %s %v = %d %v, want %d %s", c.method, c.path, c.body, got.status, got.body, c.status, c.code)
		}
	}
	key, _, err := apikeys.Create(context.Background(), r.pool, "prj_default", "app_dashboard", "k", apikeys.AllScopes())
	if err != nil {
		t.Fatal(err)
	}
	if got := r.call("GET", "/v1/settings/chapters", nil, key); got.status != http.StatusUnauthorized {
		t.Fatalf("an API key read the chapters settings: %d", got.status)
	}
}

// The Connect forms list an API's models with the URL and key typed so far
// (D127): a list, none for an API without the route, a refused key, the
// API's own failure, and a bad request. Nothing is saved, the key never
// comes back, and only the owner reaches them.
func TestConnectFormsListTheAPIsModels(t *testing.T) {
	r := newRig(t, config.Config{})
	const modelsKey = "sk-MODELS_KEY_must_never_show"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(req.URL.Path, "/down/"):
			w.WriteHeader(http.StatusServiceUnavailable)
		case strings.HasPrefix(req.URL.Path, "/none/"):
			http.NotFound(w, req)
		case req.Header.Get("Authorization") != "Bearer "+modelsKey:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"bad key"}}`)
		case req.URL.Path == "/claude/v1/models":
			_, _ = io.WriteString(w, `{"data":[{"type":"model","id":"claude-opus-5-5","display_name":"Claude Opus 5.5",`+
				`"created_at":"2026-09-01T00:00:00Z","lifecycle":"active"}],"has_more":false}`)
		case req.URL.Path == "/v1/models":
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"whisper-1"},{"id":"gpt-test"}]}`)
		default:
			http.NotFound(w, req)
		}
	}))
	defer api.Close()
	listed := `{"data":[{"id":"gpt-test"},{"id":"whisper-1"}],"listed":true}`
	for _, c := range []struct {
		path   string
		body   map[string]any
		status int
		want   string // the answer, or the error code
	}{
		{"/v1/settings/speech/models", map[string]any{"base_url": api.URL + "/v1", "api_key": modelsKey}, 200, listed},
		{"/v1/settings/speech/models", map[string]any{"base_url": api.URL + "/none/v1", "api_key": modelsKey}, 200, `{"data":[],"listed":false}`},
		{"/v1/settings/speech/models", map[string]any{"base_url": api.URL + "/v1", "api_key": "wrong"}, 400, "key_rejected"},
		{"/v1/settings/speech/models", map[string]any{"base_url": api.URL + "/down/v1", "api_key": modelsKey}, 502, "models_unavailable"},
		{"/v1/settings/speech/models", map[string]any{"base_url": "ftp://x"}, 400, "invalid_request"},
		{"/v1/settings/chapters/models", map[string]any{"provider": "openai_compatible", "base_url": api.URL + "/v1", "api_key": modelsKey}, 200, listed},
		{"/v1/settings/chapters/models", map[string]any{"provider": "openai_compatible", "base_url": api.URL + "/v1"}, 400, "key_rejected"},
		{"/v1/settings/chapters/models", map[string]any{"provider": "anthropic", "base_url": api.URL + "/claude/", "api_key": modelsKey}, 200,
			`{"data":[{"id":"claude-opus-5-5","name":"Claude Opus 5.5"}],"listed":true}`},
		{"/v1/settings/chapters/models", map[string]any{"provider": "anthropic"}, 400, "invalid_request"},
		{"/v1/settings/chapters/models", map[string]any{"provider": "gemini", "base_url": api.URL + "/v1"}, 400, "invalid_request"},
	} {
		got := r.call("POST", c.path, c.body, "")
		raw, _ := json.Marshal(got.body)
		if strings.Contains(string(raw), modelsKey) {
			t.Errorf("POST %s %v: the answer holds the key", c.path, c.body)
		}
		if c.status == 200 {
			if got.status != 200 || string(raw) != c.want {
				t.Errorf("POST %s %v = %d %s, want 200 %s", c.path, c.body, got.status, raw, c.want)
			}
			continue
		}
		if errBody, _ := got.body["error"].(map[string]any); got.status != c.status || errBody["code"] != c.want {
			t.Errorf("POST %s %v = %d %s, want %d %s", c.path, c.body, got.status, raw, c.status, c.want)
		}
	}
	var saved int
	if err := r.pool.QueryRow(context.Background(), `SELECT count(*) FROM ai_connections`).Scan(&saved); err != nil || saved != 0 {
		t.Fatalf("listing models saved %d connections (%v)", saved, err)
	}
	key, _, err := apikeys.Create(context.Background(), r.pool, "prj_default", "app_dashboard", "k", apikeys.AllScopes())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/settings/speech/models", "/v1/settings/chapters/models"} {
		if got := r.call("POST", path, map[string]any{"provider": "openai_compatible", "base_url": api.URL + "/v1", "api_key": modelsKey}, key); got.status != http.StatusUnauthorized {
			t.Errorf("an API key listed models at %s: %d", path, got.status)
		}
	}
}

// D099: the library searches titles and passthroughs, leaves deleted assets
// out on the server, and gives each item a poster URL.
func TestLibrarySearchesTitlesAndHidesDeletedAssets(t *testing.T) {
	r := newRig(t, config.Config{})
	r.exec(`INSERT INTO assets (id, project_id, app_id, status, error_code, storage_id, title, passthrough, created_at) VALUES
	          ('ast_frac', 'prj_default', 'app_dashboard', 'processing', NULL, 'sto_default', 'Fractions, part 1', 'lesson-9', '2026-11-05T10:00:00Z'),
	          ('ast_pct', 'prj_default', 'app_dashboard', 'processing', NULL, 'sto_default', '100% of a number', 'lesson_10', '2026-11-05T10:01:00Z'),
	          ('ast_gone', 'prj_default', 'app_dashboard', 'deleted', NULL, 'sto_default', 'Fractions, old', NULL, '2026-11-05T10:02:00Z'),
	          ('ast_up', 'prj_default', 'app_dashboard', 'waiting_upload', NULL, 'sto_default', NULL, NULL, '2026-11-05T10:03:00Z'),
	          ('ast_bad', 'prj_default', 'app_dashboard', 'errored', 'upload_expired', 'sto_default', NULL, 'lesson-11', '2026-11-05T10:04:00Z')`)
	ids := func(path string) []string {
		t.Helper()
		got := r.call("GET", path, nil, "")
		if got.status != 200 {
			t.Fatalf("%s = %d %v", path, got.status, got.body)
		}
		out := []string{}
		for _, a := range got.list {
			out = append(out, a.(map[string]any)["id"].(string))
		}
		return out
	}
	for path, want := range map[string][]string{
		"/v1/library":                                  {"ast_bad", "ast_up", "ast_pct", "ast_frac"},
		"/v1/library?include_deleted=true":             {"ast_bad", "ast_up", "ast_gone", "ast_pct", "ast_frac"},
		"/v1/library?status=deleted":                   {"ast_gone"},
		"/v1/library?q=fractions":                      {"ast_frac"},
		"/v1/library?q=FRACTIONS&include_deleted=true": {"ast_gone", "ast_frac"},
		"/v1/library?q=lesson-1":                       {"ast_bad"},
		"/v1/library?q=lesson_":                        {"ast_pct"}, // _ is a character, not a wildcard
		"/v1/library?q=100%25":                         {"ast_pct"},
		"/v1/library?q=%25":                            {"ast_pct"},
		"/v1/library?q=ast_up":                         {"ast_up"}, // an exact asset ID
		"/v1/library?q=ast_u":                          {},
	} {
		if got := ids(path); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", path, got, want)
		}
	}
	if got := r.call("GET", "/v1/library?include_deleted=maybe", nil, ""); got.status != http.StatusBadRequest {
		t.Errorf("include_deleted=maybe = %d, want 400", got.status)
	}
	first := r.call("GET", "/v1/library?q=fractions", nil, "").list[0].(map[string]any)
	if first["title"] != "Fractions, part 1" || first["poster_url"] != nil {
		t.Errorf("item = %v; want its title, and no poster before an output", first)
	}

	counts := r.call("GET", "/v1/library/counts", nil, "")
	if want := map[string]any{"ready": 0.0, "processing": 2.0, "errored": 1.0, "waiting_upload": 1.0}; counts.status != 200 || !reflect.DeepEqual(counts.body, want) {
		t.Errorf("counts = %d %v, want %v", counts.status, counts.body, want)
	}
	if got := r.call("GET", "/v1/library/counts?app_id=app_other", nil, ""); !reflect.DeepEqual(got.body, map[string]any{"ready": 0.0, "processing": 0.0, "errored": 0.0, "waiting_upload": 0.0}) {
		t.Errorf("another app's counts = %v, want zeros", got.body)
	}
}

func TestLibraryPostersUseAPublicIDOrAThumbnailToken(t *testing.T) {
	r := newRig(t, config.Config{})
	r.exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id, created_at) VALUES
	          ('ast_pub', 'prj_default', 'app_dashboard', 'processing', 'sto_default', '2026-11-05T10:00:00Z'),
	          ('ast_sig', 'prj_default', 'app_dashboard', 'processing', 'sto_default', '2026-11-05T10:01:00Z');
	        INSERT INTO outputs (id, project_id, asset_id, key_version, status, manifest) VALUES
	          ('out_p', 'prj_default', 'ast_pub', 1, 'committed', '{}'), ('out_s', 'prj_default', 'ast_sig', 1, 'committed', '{}');
	        UPDATE assets SET status = 'ready', current_output_id = CASE id WHEN 'ast_pub' THEN 'out_p' ELSE 'out_s' END;
	        INSERT INTO playback_ids (id, project_id, asset_id, policy, created_at) VALUES
	          ('pid_ps', 'prj_default', 'ast_pub', 'signed', '2026-11-05T10:00:00Z'),
	          ('pid_pp', 'prj_default', 'ast_pub', 'public', '2026-11-05T10:00:01Z'),
	          ('pid_ss', 'prj_default', 'ast_sig', 'signed', '2026-11-05T10:01:00Z')`)
	got := r.call("GET", "/v1/library", nil, "")
	poster := map[string]string{}
	for _, a := range got.list {
		m := a.(map[string]any)
		poster[m["id"].(string)], _ = m["poster_url"].(string)
	}
	if want := config.DefaultPublicURL + "/p/pid_pp/poster.jpg"; poster["ast_pub"] != want {
		t.Errorf("public poster = %q, want %q", poster["ast_pub"], want)
	}
	prefix, suffix := config.DefaultPublicURL+"/v/", "/pid_ss/poster.jpg"
	tok := strings.TrimSuffix(strings.TrimPrefix(poster["ast_sig"], prefix), suffix)
	parsed, err := tokens.Parse(tok)
	if !strings.HasPrefix(poster["ast_sig"], prefix) || !strings.HasSuffix(poster["ast_sig"], suffix) || err != nil {
		t.Fatalf("signed poster = %q (%v)", poster["ast_sig"], err)
	}
	var internal string
	_ = r.pool.QueryRow(context.Background(), `SELECT id FROM signing_keys WHERE app_id IS NULL`).Scan(&internal)
	if parsed.Kid != internal || parsed.Claims.Sub != "pid_ss" || parsed.Claims.Aud != "t" || parsed.Claims.Exp-parsed.Claims.Iat != 3600 {
		t.Errorf("poster token kid %s claims %+v; want the internal key, pid_ss, aud t, one hour", parsed.Kid, parsed.Claims)
	}
	r.exec(`UPDATE playback_ids SET status = 'revoked', revoked_at = now() WHERE asset_id = 'ast_sig'`)
	for _, a := range r.call("GET", "/v1/library", nil, "").list {
		if m := a.(map[string]any); m["id"] == "ast_sig" && m["poster_url"] != nil {
			t.Errorf("an asset with no live ID has poster %v, want null", m["poster_url"])
		}
	}
}

// A share link's own playback ID (D104) is never a preview's or a poster's:
// revoking the link must not end either.
func TestPreviewsAndPostersLeaveShareLinksAlone(t *testing.T) {
	r := newRig(t, config.Config{})
	r.exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default');
	        INSERT INTO outputs (id, project_id, asset_id, key_version, status, manifest) VALUES ('out_1', 'prj_default', 'ast_1', 1, 'committed', '{}');
	        UPDATE assets SET status = 'ready', current_output_id = 'out_1';
	        INSERT INTO playback_ids (id, project_id, asset_id, policy) VALUES ('pid_shr', 'prj_default', 'ast_1', 'signed');
	        INSERT INTO share_links (id, project_id, app_id, asset_id, playback_id, secret_hash, secret_sealed)
	          VALUES ('shr_1', 'prj_default', 'app_dashboard', 'ast_1', 'pid_shr', sha256('s'), '\x00')`)
	if got := r.call("POST", "/v1/assets/ast_1/preview-token", nil, ""); got.status != http.StatusConflict {
		t.Errorf("preview with only a share link's ID = %d %v, want 409", got.status, got.body)
	}
	for _, a := range r.call("GET", "/v1/library", nil, "").list {
		if m := a.(map[string]any); m["poster_url"] != nil {
			t.Errorf("poster through a share link's ID = %v, want null", m["poster_url"])
		}
	}
	r.exec(`INSERT INTO playback_ids (id, project_id, asset_id, policy, created_at) VALUES ('pid_own', 'prj_default', 'ast_1', 'signed', now() + interval '1 second')`)
	if got := r.call("POST", "/v1/assets/ast_1/preview-token", nil, ""); got.body["playback_id"] != "pid_own" {
		t.Errorf("preview = %d %v, want pid_own", got.status, got.body)
	}
}

// Edge analytics (P7-T05): the owner connects a read-only token once
// Cloudflare delivery is on; a bad token is refused and nothing is kept.
func TestEdgeAnalyticsConnectsWithAReadOnlyToken(t *testing.T) {
	r := newRig(t, config.Config{})
	if got := r.call("GET", "/v1/cloudflare/analytics", nil, ""); got.status != 200 || got.body["connected"] != false {
		t.Fatalf("before = %d %v", got.status, got.body)
	}
	if got := r.call("POST", "/v1/cloudflare/analytics", map[string]string{"token": "cf-analytics-read"}, ""); got.status != 409 || code(got) != "not_connected" {
		t.Fatalf("without Cloudflare = %d %v", got.status, got.body)
	}
	r.exec(`INSERT INTO cloudflare_connections (project_id, account_id, zone_id, hostname, bucket, script_name, token_id, storage_id, worker_version)
	        VALUES ('prj_default', 'acc_1', 'zone_1', 'media.example.com', 'clipmux-media', 'clipmux-media', 'tok_1', 'sto_default', 'v1')`)
	if got := r.call("POST", "/v1/cloudflare/analytics", map[string]string{"token": "cf-wrong"}, ""); got.status != 400 || code(got) != "token_rejected" {
		t.Fatalf("a bad token = %d %v", got.status, got.body)
	}
	got := r.call("POST", "/v1/cloudflare/analytics", map[string]string{"token": "cf-analytics-read"}, "")
	if got.status != 200 || got.body["connected"] != true || got.body["account_id"] != "acc_1" {
		t.Fatalf("connect = %d %v", got.status, got.body)
	}
	if _, ok := got.body["token"]; ok {
		t.Fatal("the token came back")
	}
	if got := r.call("DELETE", "/v1/cloudflare/analytics", nil, ""); got.status != 204 {
		t.Fatalf("disconnect = %d %v", got.status, got.body)
	}
	if got := r.call("DELETE", "/v1/cloudflare/analytics", nil, ""); got.status != 404 || code(got) != "not_connected" {
		t.Fatalf("a second disconnect = %d %v", got.status, got.body)
	}
}
