package agents_test

// Real-Postgres tests for pairing PC agents (P3-T01), driven through the
// same client code `clipmux worker` uses, against the real routes.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/accounts"
	"github.com/visheshgubrani/clipmux/internal/agents"
	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/lifecycle"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/stores"
	"github.com/visheshgubrani/clipmux/internal/worker"
)

type rig struct {
	t      *testing.T
	pool   *pgxpool.Pool
	svc    *agents.Service
	q      *queue.Queue
	url    string
	cookie *http.Cookie
	exec   func(sql string, args ...any)
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.New(t)
	acct := accounts.New(pool, "prj_default", func() string { return "http://localhost:8080" })
	q := queue.New(pool)
	svc := agents.New(pool, q, acct, "prj_default", func() string { return "https://clipmux.example" })
	store, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: "http://unused"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	svc.Life = lifecycle.New(pool, stores.Fixed(store), nil)
	mux := http.NewServeMux()
	svc.Register(mux)
	svc.RegisterWorker(mux)
	svc.Life.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	token, _, _ := acct.SetupToken(ctx)
	_, session, err := acct.CreateOwner(ctx, token, "owner@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	return &rig{
		t: t, pool: pool, svc: svc, q: q, url: srv.URL, cookie: &http.Cookie{Name: "clipmux_session", Value: session},
		exec: func(sql string, args ...any) {
			t.Helper()
			if _, err := pool.Exec(ctx, sql, args...); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		},
	}
}

// owner calls an owner route with the session cookie.
func (r *rig) owner(method, path string, body any) (int, map[string]any) {
	r.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, r.url+path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(r.cookie)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func (r *rig) authenticates(key string) (queue.Worker, bool) {
	return r.svc.AuthenticateWorker(context.Background(), key)
}

func TestDashboardCodePairsOnceAndArrivesApproved(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	status, inv := r.owner("POST", "/v1/agents/pairings", map[string]string{"name": "Studio PC"})
	if status != http.StatusCreated {
		t.Fatalf("invite = %d %v", status, inv)
	}
	code := inv["code"].(string)
	if want := "clipmux worker --server https://clipmux.example --pair " + code; inv["command"] != want {
		t.Fatalf("command = %q, want %q", inv["command"], want)
	}

	// A person may type the code in lower case without the dash.
	typed := strings.ToLower(strings.ReplaceAll(code, "-", ""))
	cfg, err := worker.Redeem(ctx, http.DefaultClient, r.url, typed, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "Studio PC" || cfg.Server != r.url || !strings.HasPrefix(cfg.Key, "cmxa_") {
		t.Fatalf("config = %+v", cfg)
	}
	w, ok := r.authenticates(cfg.Key)
	if !ok || w.Executor != "agent" || w.AgentID != cfg.AgentID || w.LocalOnly {
		t.Fatalf("agent key authenticates as %+v, %v", w, ok)
	}
	if _, err := worker.Redeem(ctx, http.DefaultClient, r.url, code, ""); !isRefusal(err, "pairing_not_found") {
		t.Fatalf("second redeem err = %v, want pairing_not_found", err)
	}
	_, pv := r.owner("GET", "/v1/agents/pairings/"+code, nil)
	if pv["status"] != "consumed" || pv["agent_id"] != cfg.AgentID {
		t.Fatalf("pairing after redeem = %v, want consumed by the new agent", pv)
	}
	_, list := r.owner("GET", "/v1/agents", nil)
	got := list["data"].([]any)
	if len(got) != 1 || got[0].(map[string]any)["status"] != "approved" || got[0].(map[string]any)["online"] != true {
		t.Fatalf("agents = %v, want one approved, online agent", got)
	}
}

func TestAnExpiredCodeIsRefused(t *testing.T) {
	r := newRig(t)
	_, inv := r.owner("POST", "/v1/agents/pairings", nil)
	r.exec(`UPDATE agent_pairings SET expires_at = now() - interval '1 second'`)
	_, err := worker.Redeem(context.Background(), http.DefaultClient, r.url, inv["code"].(string), "")
	if !isRefusal(err, "pairing_not_found") {
		t.Fatalf("redeem of an expired code err = %v, want pairing_not_found", err)
	}
}

func TestAgentStartedPairingWaitsForTheOwner(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	p, err := worker.StartPairing(ctx, http.DefaultClient, r.url, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if p.VerifyURL != "https://clipmux.example/workers/connect?code="+p.Code {
		t.Fatalf("verify url = %q", p.VerifyURL)
	}
	// Reading the code off the agent's screen isn't enough to take its key.
	if _, err := worker.Redeem(ctx, http.DefaultClient, r.url, p.Code, ""); !isRefusal(err, "pairing_not_found") {
		t.Fatalf("redeeming an agent-started code err = %v", err)
	}
	_, pv := r.owner("GET", "/v1/agents/pairings/"+p.Code, nil)
	if pv["status"] != "pending" || pv["agent_started"] != true || pv["name"] != "laptop" || pv["ip"] != "127.0.0.1" {
		t.Fatalf("pairing as the owner sees it = %v", pv)
	}

	done := make(chan worker.AgentConfig, 1)
	errs := make(chan error, 1)
	go func() {
		cfg, err := worker.AwaitApproval(ctx, http.DefaultClient, r.url, p)
		errs <- err
		done <- cfg
	}()
	time.Sleep(100 * time.Millisecond) // the agent is now polling
	if status, body := r.owner("POST", "/v1/agents/pairings/"+p.Code+"/approve", nil); status != http.StatusOK || body["status"] != "approved" {
		t.Fatalf("approve = %d %v", status, body)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	cfg := <-done
	if _, ok := r.authenticates(cfg.Key); !ok || cfg.Name != "laptop" {
		t.Fatalf("approved config %+v doesn't authenticate", cfg)
	}
	// The key is handed over once; a replayed poll gets nothing.
	if _, err := worker.AwaitApproval(ctx, http.DefaultClient, r.url, p); !isRefusal(err, "pairing_not_found") {
		t.Fatalf("replayed poll err = %v", err)
	}
}

func TestADeniedPairingEnds(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	p, err := worker.StartPairing(ctx, http.DefaultClient, r.url, "")
	if err != nil {
		t.Fatal(err)
	}
	r.owner("POST", "/v1/agents/pairings/"+p.Code+"/deny", nil)
	if _, err := worker.AwaitApproval(ctx, http.DefaultClient, r.url, p); !errors.Is(err, worker.ErrPairingDenied) {
		t.Fatalf("err = %v, want denied", err)
	}
}

func TestAgentStartedPairingsAreBounded(t *testing.T) {
	r := newRig(t)
	for range agents.MaxPendingPairings {
		if _, err := worker.StartPairing(context.Background(), http.DefaultClient, r.url, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := worker.StartPairing(context.Background(), http.DefaultClient, r.url, ""); !isRefusal(err, "too_many_pairings") {
		t.Fatalf("err = %v, want too_many_pairings", err)
	}
}

func TestRevokingAnAgentCutsOffItsKeyAndFencesItsWork(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	_, inv := r.owner("POST", "/v1/agents/pairings", nil)
	cfg, err := worker.Redeem(ctx, http.DefaultClient, r.url, inv["code"].(string), "pc")
	if err != nil {
		t.Fatal(err)
	}
	r.exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default')`)
	if _, err := queue.Enqueue(ctx, r.pool, queue.Job{ProjectID: "prj_default", AssetID: "ast_1", Kind: "transcode"}); err != nil {
		t.Fatal(err)
	}
	w, _ := r.authenticates(cfg.Key)
	c, err := r.q.Claim(ctx, w)
	if err != nil || c == nil {
		t.Fatalf("claim = %v, %v", c, err)
	}

	status, body := r.owner("DELETE", "/v1/agents/"+cfg.AgentID, nil)
	if status != http.StatusOK || body["fenced_attempts"] != float64(1) || body["agent"].(map[string]any)["status"] != "revoked" {
		t.Fatalf("revoke = %d %v", status, body)
	}
	if _, ok := r.authenticates(cfg.Key); ok {
		t.Fatal("a revoked agent's key still authenticates")
	}
	if v, err := r.q.Heartbeat(ctx, c.Token, queue.Progress{}); err != nil || !v.Stop {
		t.Fatalf("heartbeat = %+v, %v; want stop", v, err)
	}
	if status, _ := r.owner("DELETE", "/v1/agents/"+cfg.AgentID, nil); status != http.StatusOK {
		t.Fatalf("revoking twice = %d, want 200", status)
	}
	if status, _ := r.owner("PATCH", "/v1/agents/"+cfg.AgentID, map[string]string{"scope": "local"}); status != http.StatusNotFound {
		t.Fatalf("updating a revoked agent = %d, want 404", status)
	}
}

func TestOwnerRoutesNeedTheSession(t *testing.T) {
	r := newRig(t)
	res, err := http.Post(r.url+"/v1/agents/pairings", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invite without a session = %d, want 401", res.StatusCode)
	}
}

func isRefusal(err error, code string) bool {
	var pe *worker.PairingError
	return errors.As(err, &pe) && pe.Code == code
}

// bearer calls a route with an agent or API key.
func (r *rig) bearer(key, path string, body any) (int, map[string]any) {
	r.t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", r.url+path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func (r *rig) pairAgent(name string) worker.AgentConfig {
	r.t.Helper()
	_, inv := r.owner("POST", "/v1/agents/pairings", nil)
	cfg, err := worker.Redeem(context.Background(), http.DefaultClient, r.url, inv["code"].(string), name)
	if err != nil {
		r.t.Fatal(err)
	}
	return cfg
}

func (r *rig) job(assetID string) (pinned string, status string, code string) {
	r.t.Helper()
	var p, c *string
	if err := r.pool.QueryRow(context.Background(),
		`SELECT pinned_agent_id, status, error_code FROM jobs WHERE asset_id = $1 ORDER BY created_at DESC LIMIT 1`, assetID).Scan(&p, &status, &c); err != nil {
		r.t.Fatal(err)
	}
	if p != nil {
		pinned = *p
	}
	if c != nil {
		code = *c
	}
	return pinned, status, code
}

func TestALocalSourceIsPinnedToItsAgent(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	pc, other := r.pairAgent("pc"), r.pairAgent("other")
	key, _, err := apikeys.Create(ctx, r.pool, "prj_default", "app_dashboard", "cli", []string{apikeys.AssetsWrite})
	if err != nil {
		t.Fatal(err)
	}

	status, src := r.bearer(pc.Key, "/worker/v1/sources", map[string]any{"size": 1234})
	if status != http.StatusCreated {
		t.Fatalf("register = %d %v", status, src)
	}
	status, asset := r.bearer(key, "/v1/assets", map[string]any{"agent_source_id": src["id"], "passthrough": "lesson-9"})
	if status != http.StatusCreated || asset["status"] != "processing" {
		t.Fatalf("attach = %d %v", status, asset)
	}
	if pinned, _, _ := r.job(asset["id"].(string)); pinned != pc.AgentID {
		t.Fatalf("job pinned to %q, want %s", pinned, pc.AgentID)
	}
	if status, body := r.bearer(key, "/v1/assets", map[string]any{"agent_source_id": src["id"]}); status != http.StatusNotFound {
		t.Fatalf("attaching a source twice = %d %v", status, body)
	}

	ow, _ := r.authenticates(other.Key)
	if c, err := r.q.Claim(ctx, ow); err != nil || c != nil {
		t.Fatalf("another agent claimed %v, %v", c, err)
	}
	pw, _ := r.authenticates(pc.Key)
	c, err := r.q.Claim(ctx, pw)
	if err != nil || c == nil || c.Job.AssetID != asset["id"] {
		t.Fatalf("its own agent claim = %v, %v", c, err)
	}
	if got, size, err := r.svc.LocalSource(ctx, c.Job.AssetID); err != nil || got != src["id"] || size != 1234 {
		t.Fatalf("LocalSource = %q %d %v", got, size, err)
	}
}

func TestTheOwnersPushNeedsNoAPIKey(t *testing.T) {
	r := newRig(t)
	pc := r.pairAgent("pc")
	_, src := r.bearer(pc.Key, "/worker/v1/sources", map[string]any{"size": 10})
	status, asset := r.bearer(pc.Key, "/worker/v1/sources/"+src["id"].(string)+"/attach", map[string]any{"playback_policy": "public"})
	if status != http.StatusCreated || asset["app_id"] != "app_dashboard" {
		t.Fatalf("attach as the owner = %d %v, want an asset of the Dashboard app", status, asset)
	}
	other := r.pairAgent("other")
	_, theirs := r.bearer(other.Key, "/worker/v1/sources", map[string]any{"size": 10})
	if status, _ := r.bearer(pc.Key, "/worker/v1/sources/"+theirs["id"].(string)+"/attach", nil); status != http.StatusNotFound {
		t.Fatalf("attaching another agent's source = %d, want 404", status)
	}
}

func TestAJobWaitsForItsPCThenEndsIfItNeverReturns(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	pc := r.pairAgent("pc")
	_, src := r.bearer(pc.Key, "/worker/v1/sources", map[string]any{"size": 10})
	_, asset := r.bearer(pc.Key, "/worker/v1/sources/"+src["id"].(string)+"/attach", nil)
	id := asset["id"].(string)

	r.exec(`UPDATE agents SET last_seen_at = now() - interval '6 days'`)
	if n, err := r.q.SweepStrandedPinned(ctx, 10); err != nil || n != 0 {
		t.Fatalf("a PC away for 6 days: swept %d, %v; want its job still waiting", n, err)
	}
	if _, status, _ := r.job(id); status != "queued" {
		t.Fatalf("job = %s, want queued", status)
	}
	r.exec(`UPDATE agents SET last_seen_at = now() - interval '8 days'`)
	if n, err := r.q.SweepStrandedPinned(ctx, 10); err != nil || n != 1 {
		t.Fatalf("a PC away for 8 days: swept %d, %v", n, err)
	}
	if _, status, code := r.job(id); status != "failed" || code != "agent_unavailable" {
		t.Fatalf("job = %s %s, want failed agent_unavailable", status, code)
	}
}

func TestAnAgentForgetsSourcesWhoseAssetsWereDeleted(t *testing.T) {
	r := newRig(t)
	pc := r.pairAgent("pc")
	_, kept := r.bearer(pc.Key, "/worker/v1/sources", map[string]any{"size": 10})
	_, gone := r.bearer(pc.Key, "/worker/v1/sources", map[string]any{"size": 10})
	_, fresh := r.bearer(pc.Key, "/worker/v1/sources", map[string]any{"size": 10})
	_, a1 := r.bearer(pc.Key, "/worker/v1/sources/"+kept["id"].(string)+"/attach", nil)
	_, a2 := r.bearer(pc.Key, "/worker/v1/sources/"+gone["id"].(string)+"/attach", nil)
	_ = a1
	r.exec(`UPDATE assets SET status = 'deleted' WHERE id = $1`, a2["id"])

	ids := []string{kept["id"].(string), gone["id"].(string), fresh["id"].(string), "src_unknown"}
	_, out := r.bearer(pc.Key, "/worker/v1/sources/sync", map[string]any{"ids": ids})
	got, _ := json.Marshal(out["gone"])
	if want := `["` + gone["id"].(string) + `","src_unknown"]`; string(got) != want {
		t.Fatalf("gone = %s, want %s (a fresh, unattached source is kept a day)", got, want)
	}
}

func TestARevokedAgentsSourceCantBeReencoded(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	pc := r.pairAgent("pc")
	_, src := r.bearer(pc.Key, "/worker/v1/sources", map[string]any{"size": 10})
	_, asset := r.bearer(pc.Key, "/worker/v1/sources/"+src["id"].(string)+"/attach", nil)
	r.exec(`UPDATE jobs SET status = 'succeeded'`)
	r.exec(`UPDATE assets SET status = 'errored', error_code = 'transcode_failed'`)
	r.owner("DELETE", "/v1/agents/"+pc.AgentID, nil)
	key, _, _ := apikeys.Create(ctx, r.pool, "prj_default", "app_dashboard", "cli", []string{apikeys.AssetsWrite})
	status, body := r.bearer(key, "/v1/assets/"+asset["id"].(string)+"/reencode", nil)
	if status != http.StatusConflict || body["error"].(map[string]any)["code"] != "source_unavailable" {
		t.Fatalf("re-encode = %d %v, want 409 source_unavailable", status, body)
	}
}
