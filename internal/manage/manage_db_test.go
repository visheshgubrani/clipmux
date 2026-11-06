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
	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/assets"
	"github.com/visheshgubrani/clipmux/internal/config"
	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/delivery"
	"github.com/visheshgubrani/clipmux/internal/manage"
	"github.com/visheshgubrani/clipmux/internal/secrets"
	"github.com/visheshgubrani/clipmux/internal/settings"
	"github.com/visheshgubrani/clipmux/internal/signingkeys"
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
	return &rig{t: t, pool: pool, url: srv.URL, box: box, cookie: &http.Cookie{Name: "clipmux_session", Value: session}}
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
	if got := r.call("PUT", "/v1/settings/routing", map[string]any{"primary_executor": "workers"}, "cmx_not_a_session"); got.status != http.StatusUnauthorized {
		t.Fatalf("an API key changed routing: %d", got.status)
	}
}
