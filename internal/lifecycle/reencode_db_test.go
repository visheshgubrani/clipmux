package lifecycle_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/uploads"
)

func errorOf(body map[string]any) map[string]any {
	e, _ := body["error"].(map[string]any)
	return e
}

func TestReencodeQueuesAJobAndAnswers409WhileOneIsActive(t *testing.T) {
	r := newRig(t)
	r.ready()
	status, body := r.send("POST", "/v1/assets/ast_1/reencode", r.key, nil, "")
	job, _ := body["id"].(string)
	if status != http.StatusAccepted || job == "" || body["kind"] != "reencode" || body["status"] != "queued" {
		t.Fatalf("reencode = %d %v, want 202 with a queued reencode job", status, body)
	}
	if got := r.scalar(`SELECT status FROM assets`); got != "ready" {
		t.Fatalf("asset = %v, want it still ready while re-encoding", got)
	}
	status, body = r.send("POST", "/v1/assets/ast_1/reencode", r.key, nil, "")
	if e := errorOf(body); status != http.StatusConflict || e["code"] != "job_active" || e["job_id"] != job {
		t.Fatalf("second reencode = %d %v, want 409 job_active naming %s", status, body, job)
	}
	readOnly, _, _ := apikeys.Create(context.Background(), r.pool, "prj_default", "app_dashboard", "reader", []string{apikeys.AssetsRead})
	if status, _ := r.send("POST", "/v1/assets/ast_1/reencode", readOnly, nil, ""); status != http.StatusForbidden {
		t.Fatalf("reencode with assets:read only = %d, want 403", status)
	}
	if status, _ := r.send("POST", "/v1/assets/ast_nope/reencode", r.key, nil, ""); status != http.StatusNotFound {
		t.Fatalf("reencode of an unknown asset = %d, want 404", status)
	}
}

func TestSupersedeCancelsTheActiveJobFirst(t *testing.T) {
	r := newRig(t)
	r.ready()
	ctx := context.Background()
	_, first := r.send("POST", "/v1/assets/ast_1/reencode", r.key, nil, "")
	claim, err := r.q.Claim(ctx, queue.Worker{Executor: "builtin"})
	if err != nil || claim == nil || claim.Job.ID != first["id"] {
		t.Fatalf("claim = %v, %v", claim, err)
	}
	status, second := r.send("POST", "/v1/assets/ast_1/reencode", r.key, map[string]any{"supersede": true}, "")
	if status != http.StatusAccepted || second["id"] == first["id"] {
		t.Fatalf("superseding reencode = %d %v", status, second)
	}
	if got := r.scalar(`SELECT status FROM jobs WHERE id = $1`, first["id"]); got != "cancelled" {
		t.Fatalf("superseded job = %v, want cancelled", got)
	}
	if v, err := r.q.Heartbeat(ctx, claim.Token, queue.Progress{}); err != nil || !v.Stop || v.Reason != queue.ReasonCancelled {
		t.Fatalf("superseded worker's heartbeat = %+v, %v; want stop: cancelled", v, err)
	}
}

func TestReencodingAnErroredAssetReturnsItToProcessing(t *testing.T) {
	r := newRig(t)
	r.exec(`INSERT INTO assets (id, project_id, app_id, status, error_code, storage_id, source_uri, version) VALUES
	        ('ast_err', 'prj_default', 'app_dashboard', 'errored', 'processing_failed', 'sto_default', 'uploads/ast_err/source', 3),
	        ('ast_gone', 'prj_default', 'app_dashboard', 'errored', 'processing_failed', 'sto_default', 'uploads/ast_gone/source', 3),
	        ('ast_del', 'prj_default', 'app_dashboard', 'deleted', NULL, 'sto_default', NULL, 3)`)
	r.put("uploads/ast_err/source", "source bytes")
	if status, body := r.send("POST", "/v1/assets/ast_err/reencode", r.key, nil, ""); status != http.StatusAccepted {
		t.Fatalf("reencode errored = %d %v", status, body)
	}
	got := map[string]any{
		"status": r.scalar(`SELECT status FROM assets WHERE id = 'ast_err'`),
		"code":   r.scalar(`SELECT error_code IS NULL FROM assets WHERE id = 'ast_err'`),
		"v":      r.scalar(`SELECT version FROM assets WHERE id = 'ast_err'`),
	}
	if want := map[string]any{"status": "processing", "code": true, "v": int64(4)}; got["status"] != want["status"] || got["code"] != want["code"] || got["v"] != want["v"] {
		t.Fatalf("errored asset after reencode = %v, want %v", got, want)
	}
	if _, body := r.send("POST", "/v1/assets/ast_gone/reencode", r.key, nil, ""); errorOf(body)["code"] != "source_missing" {
		t.Fatalf("reencode without a stored source = %v, want 409 source_missing", body)
	}
	if _, body := r.send("POST", "/v1/assets/ast_del/reencode", r.key, nil, ""); errorOf(body)["code"] != "asset_deleted" {
		t.Fatalf("reencode of a deleted asset = %v, want 409 asset_deleted", body)
	}
	key, _ := apikeys.Authenticate(context.Background(), r.pool, r.key)
	up, err := r.upl.Create(context.Background(), key, uploads.CreateRequest{Size: 10}, "")
	if err != nil {
		t.Fatal(err)
	}
	if status, body := r.send("POST", "/v1/assets/"+up.AssetID+"/reencode", r.key, nil, ""); status != http.StatusConflict || errorOf(body)["code"] != "asset_not_ready" {
		t.Fatalf("reencode while waiting for its upload = %d %v, want 409 asset_not_ready", status, body)
	}
}

func TestReplayedReencodeReturnsTheSameJob(t *testing.T) {
	r := newRig(t)
	r.ready()
	_, first := r.send("POST", "/v1/assets/ast_1/reencode", r.key, nil, "retry-1")
	status, again := r.send("POST", "/v1/assets/ast_1/reencode", r.key, nil, "retry-1")
	if status != http.StatusAccepted || again["id"] != first["id"] {
		t.Fatalf("replay = %d %v, want 202 with job %v", status, again, first["id"])
	}
	if status, body := r.send("POST", "/v1/assets/ast_1/reencode", r.key, map[string]any{"supersede": true}, "retry-1"); status != http.StatusUnprocessableEntity || errorOf(body)["code"] != "idempotency_key_reused" {
		t.Fatalf("same key, different body = %d %v, want 422", status, body)
	}
	if n := r.scalar(`SELECT count(*) FROM jobs WHERE kind = 'reencode'`); n != int64(1) {
		t.Fatalf("reencode jobs = %v, want 1", n)
	}
}

// A key belongs to one route: reusing an upload's key on another endpoint is
// a different request, not a replay.
func TestAKeyReusedOnAnotherEndpointIsRejected(t *testing.T) {
	r := newRig(t)
	r.ready()
	key, _ := apikeys.Authenticate(context.Background(), r.pool, r.key)
	if _, err := r.upl.Create(context.Background(), key, uploads.CreateRequest{Size: 10}, "shared-key"); err != nil {
		t.Fatal(err)
	}
	if status, body := r.send("POST", "/v1/assets/ast_1/reencode", r.key, nil, "shared-key"); status != http.StatusUnprocessableEntity {
		t.Fatalf("reencode with an upload's key = %d %v, want 422", status, body)
	}
}

func TestAddedPlaybackIDIsPublishedAndPlayable(t *testing.T) {
	r := newRig(t)
	r.ready()
	status, body := r.send("POST", "/v1/assets/ast_1/playback-ids", r.key, map[string]any{"policy": "public"}, "")
	pid, _ := body["id"].(string)
	if status != http.StatusCreated || pid == "" || body["policy"] != "public" || body["playable"] != true || body["status"] != "active" {
		t.Fatalf("add playback ID = %d %v", status, body)
	}
	if status, _ := r.call("GET", "/p/"+pid+"/master.m3u8", ""); status != http.StatusOK {
		t.Fatalf("new public ID plays: %d, want 200", status)
	}
	if v := r.scalar(`SELECT version FROM assets`); v != int64(2) {
		t.Fatalf("version = %v, want 2", v)
	}
	if ev := r.events(); len(ev) != 1 || ev[0] != "video.asset.updated" {
		t.Fatalf("events = %v, want one video.asset.updated once the route is live", ev)
	}
	if status, _ := r.send("POST", "/v1/assets/ast_1/playback-ids", r.key, map[string]any{"policy": "private"}, ""); status != http.StatusBadRequest {
		t.Fatalf("unknown policy = %d, want 400", status)
	}
}

func TestRevokedPlaybackIDStopsAtOnceAndAnswersOnceConfirmed(t *testing.T) {
	r := newRig(t)
	r.ready()
	status, body := r.call("DELETE", "/v1/assets/ast_1/playback-ids/pid_pub", r.key)
	if status != http.StatusOK || body["id"] != "pid_pub" || body["status"] != "revoked" {
		t.Fatalf("revoke = %d %v, want 200 revoked", status, body)
	}
	if status, _ := r.call("GET", "/p/pid_pub/master.m3u8", ""); status != http.StatusNotFound {
		t.Fatalf("revoked ID: %d, want 404", status)
	}
	if ev := r.events(); len(ev) != 1 || ev[0] != "video.asset.updated" {
		t.Fatalf("events = %v, want one video.asset.updated", ev)
	}
	if status, _ := r.call("DELETE", "/v1/assets/ast_1/playback-ids/pid_pub", r.key); status != http.StatusOK {
		t.Fatalf("revoking twice = %d, want 200", status)
	}
	if status, _ := r.call("DELETE", "/v1/assets/ast_1/playback-ids/pid_nope", r.key); status != http.StatusNotFound {
		t.Fatalf("unknown ID = %d, want 404", status)
	}
}

func TestUnconfirmedRevocationAnswers202AndFinishesLater(t *testing.T) {
	r := newRig(t)
	r.ready()
	r.svc.Inline = 0 // the route write hasn't been confirmed yet
	status, body := r.call("DELETE", "/v1/assets/ast_1/playback-ids/pid_pub", r.key)
	if status != http.StatusAccepted || body["status"] != "revoking" {
		t.Fatalf("revoke = %d %v, want 202 revoking", status, body)
	}
	if status, _ := r.call("GET", "/p/pid_pub/master.m3u8", ""); status != http.StatusNotFound {
		t.Fatalf("a revoking ID must stop at once: %d", status)
	}
	if _, err := r.routes.SyncRoutes(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	if got := r.scalar(`SELECT status FROM playback_ids WHERE id = 'pid_pub'`); got != "revoked" {
		t.Fatalf("after publication = %v, want revoked", got)
	}
}
