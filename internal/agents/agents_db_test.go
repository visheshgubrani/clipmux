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
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/queue"
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
	mux := http.NewServeMux()
	svc.Register(mux)
	svc.RegisterWorker(mux)
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
