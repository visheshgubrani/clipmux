package lifecycle_test

// Real Postgres and filesystem storage, driven over HTTP. Expected behaviour
// comes from docs/architecture.md, "One mutating job per asset" (a delete
// always wins, and runs cancel → unpublish → delete files → event).

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/assets"
	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/delivery"
	"github.com/visheshgubrani/clipmux/internal/lifecycle"
	"github.com/visheshgubrani/clipmux/internal/outputs"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/stores"
	"github.com/visheshgubrani/clipmux/internal/tracks"
	"github.com/visheshgubrani/clipmux/internal/uploads"
)

const master = `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,URI="audio/index.m3u8",GROUP-ID="a",NAME="x"
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,AUDIO="a"
360p/index.m3u8
`

type rig struct {
	t      *testing.T
	pool   *pgxpool.Pool
	store  *storage.Filesystem
	q      *queue.Queue
	svc    *lifecycle.Service
	routes *outputs.Committer
	upl    *uploads.Service
	url    string
	key    string // assets:read, assets:write, uploads:write for app_dashboard
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.New(t)
	store, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: "http://unused"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	q := queue.New(pool)
	committer := outputs.New(pool, q, stores.Fixed(store))
	svc := lifecycle.New(pool, stores.Fixed(store), committer)
	keys, err := contentkey.FromHex(1, strings.Repeat("00", 32))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	assets.New(pool).Register(mux)
	svc.Register(mux)
	playback := delivery.New(pool, stores.Fixed(store), contentkey.NewRing(keys))
	committer.OnPublished, committer.OnKeys = playback.Invalidate, playback.InvalidateKeys // as serve wires them
	playback.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	key, _, err := apikeys.Create(ctx, pool, "prj_default", "app_dashboard", "backend",
		[]string{apikeys.AssetsRead, apikeys.AssetsWrite, apikeys.UploadsWrite})
	if err != nil {
		t.Fatal(err)
	}
	return &rig{t: t, pool: pool, store: store, q: q, svc: svc, routes: committer, upl: uploads.New(pool, stores.Fixed(store)), url: srv.URL, key: key}
}

func (r *rig) exec(sql string, args ...any) {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(), sql, args...); err != nil {
		r.t.Fatalf("%s: %v", sql, err)
	}
}

func (r *rig) scalar(sql string, args ...any) any {
	r.t.Helper()
	var v any
	if err := r.pool.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		r.t.Fatalf("%s: %v", sql, err)
	}
	return v
}

func (r *rig) put(key, body string) {
	r.t.Helper()
	if err := r.store.Put(context.Background(), key, strings.NewReader(body)); err != nil {
		r.t.Fatal(err)
	}
}

// ready makes ast_1 a ready asset with a public playback ID, a stored source
// and the files of its current output.
func (r *rig) ready() {
	r.exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id, source_uri) VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default', 'uploads/ast_1/source')`)
	r.exec(`INSERT INTO outputs (id, project_id, asset_id, key_version, status, manifest) VALUES ('out_1', 'prj_default', 'ast_1', 1, 'committed', '{}')`)
	r.exec(`UPDATE assets SET status = 'ready', current_output_id = 'out_1' WHERE id = 'ast_1'`)
	r.exec(`INSERT INTO playback_ids (id, project_id, asset_id, policy) VALUES ('pid_pub', 'prj_default', 'ast_1', 'public')`)
	r.put("uploads/ast_1/source", "source bytes")
	r.put("media/ast_1/out_1/master.m3u8", master)
	r.put("media/ast_1/out_1/360p/seg_00001.m4s", "ciphertext")
}

func (r *rig) call(method, path, bearer string) (int, map[string]any) {
	r.t.Helper()
	return r.send(method, path, bearer, nil, "")
}

// send makes a request with an optional JSON body and Idempotency-Key.
func (r *rig) send(method, path, bearer string, body any, idemKey string) (int, map[string]any) {
	r.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = strings.NewReader(string(raw))
	}
	req, _ := http.NewRequest(method, r.url+path, rd)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out
}

func (r *rig) events() []string {
	r.t.Helper()
	rows, err := r.pool.Query(context.Background(), `SELECT payload->>'type' FROM outbox WHERE kind = 'webhook' ORDER BY created_at, id`)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func (r *rig) stored(prefix string) int {
	r.t.Helper()
	objs, _, err := r.store.List(context.Background(), prefix, "")
	if err != nil {
		r.t.Fatal(err)
	}
	return len(objs)
}

func (r *rig) runControlJobs() int {
	r.t.Helper()
	n, err := r.svc.Run(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	return n
}

func TestDeletingAReadyAssetStopsPlaybackAtOnceAndRemovesItsFiles(t *testing.T) {
	r := newRig(t)
	r.ready()
	if status, _ := r.call("GET", "/p/pid_pub/master.m3u8", ""); status != http.StatusOK {
		t.Fatalf("before delete: master = %d, want 200", status)
	}

	status, body := r.call("DELETE", "/v1/assets/ast_1", r.key)
	if status != http.StatusOK || body["status"] != "deleted" {
		t.Fatalf("DELETE = %d %v, want 200 with status deleted", status, body)
	}
	if pids, _ := body["playback_ids"].([]any); len(pids) != 0 {
		t.Errorf("playback_ids = %v, want none once unpublished", pids)
	}
	if status, _ := r.call("GET", "/p/pid_pub/master.m3u8", ""); status != http.StatusNotFound {
		t.Fatalf("after delete: master = %d, want 404", status)
	}
	if n := r.stored("media/ast_1/") + r.stored("uploads/ast_1/"); n != 0 {
		t.Fatalf("%d files left under media/ast_1/ and uploads/ast_1/", n)
	}
	if ev := r.events(); len(ev) != 1 || ev[0] != "video.asset.deleted" {
		t.Fatalf("events = %v, want one video.asset.deleted", ev)
	}
	if status, body := r.call("GET", "/v1/assets/ast_1", r.key); status != http.StatusOK || body["status"] != "deleted" {
		t.Fatalf("GET after delete = %d %v, want 200 deleted", status, body)
	}
	got := map[string]any{
		"job":     r.scalar(`SELECT status FROM jobs WHERE kind = 'delete'`),
		"pid":     r.scalar(`SELECT status FROM playback_ids WHERE id = 'pid_pub'`),
		"version": r.scalar(`SELECT version FROM assets`),
		"files":   r.scalar(`SELECT deleted_at IS NOT NULL FROM assets`),
	}
	want := map[string]any{"job": "succeeded", "pid": "revoked", "version": int64(2), "files": true}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

func TestDeleteCancelsTheRunningJobAndItsWorkerStops(t *testing.T) {
	r := newRig(t)
	r.ready()
	ctx := context.Background()
	if _, err := queue.Enqueue(ctx, r.pool, queue.Job{ProjectID: "prj_default", AssetID: "ast_1", Kind: "reencode"}); err != nil {
		t.Fatal(err)
	}
	claim, err := r.q.Claim(ctx, queue.Worker{Executor: "builtin"})
	if err != nil || claim == nil {
		t.Fatalf("claim: %v, %v", claim, err)
	}

	if status, body := r.call("DELETE", "/v1/assets/ast_1", r.key); status != http.StatusOK {
		t.Fatalf("DELETE = %d %v, want 200", status, body)
	}
	if v, err := r.q.Heartbeat(ctx, claim.Token, queue.Progress{Phase: "transcode"}); err != nil || !v.Stop || v.Reason != queue.ReasonCancelled {
		t.Fatalf("heartbeat = %+v, %v; want stop: cancelled", v, err)
	}
	// The stopped worker still holds PUT URLs for a few minutes.
	late := "media/ast_1/" + claim.Output.ID + "/360p/seg_00001.m4s"
	r.put(late, "late ciphertext")
	if n := r.runControlJobs(); n != 0 {
		t.Fatalf("the second pass ran %d job(s) before the URLs expired", n)
	}
	r.exec(`UPDATE jobs SET finished_at = now() - interval '20 minutes' WHERE kind = 'reencode'`)
	r.exec(`UPDATE jobs SET available_at = now() WHERE kind = 'delete'`)
	if n := r.runControlJobs(); n != 1 {
		t.Fatalf("second pass ran %d job(s), want 1", n)
	}
	if _, err := r.store.Head(ctx, late); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a file written after the first pass survived: %v", err)
	}
	if got := r.scalar(`SELECT status FROM jobs WHERE kind = 'delete'`); got != "succeeded" {
		t.Fatalf("delete job = %v, want succeeded", got)
	}
	if ev := r.events(); len(ev) != 1 || ev[0] != "video.asset.deleted" {
		t.Fatalf("events = %v, want exactly one video.asset.deleted", ev)
	}
}

// Delete stops both families (P5-T13): the re-encode and the
// transcription are cancelled, the transcription's worker is told to stop,
// and the asset's tracks and transcripts go at once.
func TestDeleteCancelsBothFamiliesAndFencesTheTranscription(t *testing.T) {
	r := newRig(t)
	r.ready()
	ctx := context.Background()
	qs := db.New(r.pool)
	if _, err := tracks.Create(ctx, qs, "prj_default", "ast_1", tracks.Generate{Chapters: true}); err != nil {
		t.Fatal(err)
	}
	asset, err := qs.GetAssetByID(ctx, "ast_1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tracks.QueueTranscriptions(ctx, qs, asset, true); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Enqueue(ctx, r.pool, queue.Job{ProjectID: "prj_default", AssetID: "ast_1", Kind: "reencode"}); err != nil {
		t.Fatal(err)
	}
	encode, err := r.q.Claim(ctx, queue.Worker{Executor: "builtin"})
	if err != nil || encode == nil || encode.Job.Kind != "reencode" {
		t.Fatalf("encode claim: %+v, %v", encode, err)
	}
	speech, err := r.q.Claim(ctx, queue.Worker{Executor: "builtin", Kinds: []string{"transcribe"}, Speech: true})
	if err != nil || speech == nil || speech.Job.Kind != "transcribe" {
		t.Fatalf("speech claim: %+v, %v", speech, err)
	}
	r.exec(`INSERT INTO transcripts (track_id, project_id, asset_id, body) SELECT id, project_id, asset_id, '{}' FROM tracks WHERE kind = 'subtitles'`)

	if status, body := r.call("DELETE", "/v1/assets/ast_1", r.key); status != http.StatusOK && status != http.StatusAccepted {
		t.Fatalf("DELETE = %d %v", status, body)
	}
	for _, c := range []*queue.Claim{encode, speech} {
		if v, err := r.q.Heartbeat(ctx, c.Token, queue.Progress{}); err != nil || !v.Stop || v.Reason != queue.ReasonCancelled {
			t.Fatalf("%s heartbeat = %+v, %v; want stop: cancelled", c.Job.Kind, v, err)
		}
	}
	if n := r.scalar(`SELECT count(*) FROM tracks WHERE asset_id = 'ast_1' AND status <> 'deleted'`); n != int64(0) {
		t.Fatalf("%v tracks not deleted", n)
	}
	if n := r.scalar(`SELECT count(*) FROM transcripts`); n != int64(0) {
		t.Fatalf("%v transcripts left", n)
	}
}

func TestDeletingAnAssetWaitingForItsUploadAbortsIt(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	key, err := apikeys.Authenticate(ctx, r.pool, r.key)
	if err != nil {
		t.Fatal(err)
	}
	up, err := r.upl.Create(ctx, key, uploads.CreateRequest{Size: 10}, "")
	if err != nil {
		t.Fatal(err)
	}
	if status, body := r.call("DELETE", "/v1/assets/"+up.AssetID, r.key); status != http.StatusOK || body["status"] != "deleted" {
		t.Fatalf("DELETE = %d %v", status, body)
	}
	if got := r.scalar(`SELECT status FROM uploads WHERE id = $1`, up.UploadID); got != "cancelled" {
		t.Fatalf("upload = %v, want cancelled", got)
	}
	if _, err := r.upl.Complete(ctx, up.UploadID, []storage.Part{{Number: 1, ETag: "x"}}); err == nil || !strings.HasPrefix(err.Error(), "upload_not_active") {
		t.Fatalf("complete after delete = %v, want upload_not_active", err)
	}
	if _, err := r.store.ListParts(ctx, "uploads/"+up.AssetID+"/source", r.scalar(`SELECT multipart_id FROM uploads`).(string)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("multipart staging survived the delete: %v", err)
	}

	// An errored asset deletes too; error_code clears with the status.
	r.exec(`INSERT INTO assets (id, project_id, app_id, status, error_code, storage_id) VALUES ('ast_err', 'prj_default', 'app_dashboard', 'errored', 'unsupported_source', 'sto_default')`)
	if status, body := r.call("DELETE", "/v1/assets/ast_err", r.key); status != http.StatusOK || body["error_code"] != nil {
		t.Fatalf("DELETE errored = %d %v", status, body)
	}
}

func TestDeleteIsIdempotentScopedAndResumesAfterACrash(t *testing.T) {
	r := newRig(t)
	r.ready()
	ctx := context.Background()
	r.exec(`INSERT INTO apps (id, project_id, name) VALUES ('app_other', 'prj_default', 'other')`)
	other, _, _ := apikeys.Create(ctx, r.pool, "prj_default", "app_other", "backend", []string{apikeys.AssetsRead, apikeys.AssetsWrite})
	readOnly, _, _ := apikeys.Create(ctx, r.pool, "prj_default", "app_dashboard", "reader", []string{apikeys.AssetsRead})
	if status, _ := r.call("DELETE", "/v1/assets/ast_1", other); status != http.StatusNotFound {
		t.Fatalf("another app's key: %d, want 404", status)
	}
	if status, _ := r.call("DELETE", "/v1/assets/ast_1", readOnly); status != http.StatusForbidden {
		t.Fatalf("a key without assets:write: %d, want 403", status)
	}
	if status, _ := r.call("DELETE", "/v1/assets/ast_1", ""); status != http.StatusUnauthorized {
		t.Fatalf("no key: %d, want 401", status)
	}

	// The request commits but the server dies before deleting anything.
	r.svc.Inline = 0
	for range 2 {
		if status, body := r.call("DELETE", "/v1/assets/ast_1", r.key); status != http.StatusAccepted || body["status"] != "deleted" {
			t.Fatalf("DELETE while pending = %d %v, want 202 deleted", status, body)
		}
	}
	if n := r.scalar(`SELECT count(*) FROM jobs WHERE kind = 'delete'`); n != int64(1) {
		t.Fatalf("delete jobs = %v, want 1", n)
	}
	if n := r.stored("media/ast_1/"); n == 0 {
		t.Fatal("nothing should be deleted before the job runs")
	}
	if n := r.runControlJobs(); n != 1 {
		t.Fatalf("control jobs run = %d, want 1", n)
	}
	if status, _ := r.call("DELETE", "/v1/assets/ast_1", r.key); status != http.StatusOK {
		t.Fatalf("DELETE once done = %d, want 200", status)
	}
	if n := r.stored("media/ast_1/"); n != 0 {
		t.Fatalf("%d output files left", n)
	}
	if ev := r.events(); len(ev) != 1 {
		t.Fatalf("events = %v, want one", ev)
	}
}
