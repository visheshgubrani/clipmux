package workerapi_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/outputs"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/stores"
	"github.com/visheshgubrani/clipmux/internal/workerapi"
)

const workerKey = "builtin-test-key"

var capable = map[string]any{
	"contract_versions": []int{1},
	"ffmpeg_version":    "9.0.1",
	"packager_version":  "v3.9.3",
	"encoders":          []string{"libx264"},
	"free_disk_bytes":   50 << 30,
	"wait_seconds":      0,
}

type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	srv   *httptest.Server
	store *storage.Filesystem
	keys  contentkey.Secret
	exec  func(string, ...any)
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := dbtest.New(t)
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	store, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	keys, err := contentkey.FromHex(1, "0f0e0d0c0b0a090807060504030201000f0e0d0c0b0a09080706050403020100")
	if err != nil {
		t.Fatal(err)
	}
	q := queue.New(pool)
	api := workerapi.New(q, workerapi.BuiltinKey(workerKey), stores.Fixed(store), contentkey.NewRing(keys), outputs.New(pool, q, stores.Fixed(store)))
	mux.Handle("POST /worker/v1/", api.Handler())
	for _, m := range []string{"GET", "PUT"} {
		mux.Handle(m+" /_storage/", store.Handler())
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	return &env{t: t, pool: pool, srv: srv, store: store, keys: keys, exec: exec}
}

// enqueue creates an uploaded asset with a source and a queued transcode.
func (e *env) enqueue() {
	e.exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default')`)
	e.exec(`INSERT INTO jobs (id, project_id, asset_id, kind) VALUES ('job_1', 'prj_default', 'ast_1', 'transcode')`)
	if err := e.store.Put(context.Background(), "uploads/ast_1/source", strings.NewReader("source-bytes")); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) claim() map[string]any {
	e.t.Helper()
	code, job := e.post("/worker/v1/claim", workerKey, capable)
	if code != http.StatusOK {
		e.t.Fatalf("claim: %d %v", code, job)
	}
	return job
}

func download(t *testing.T, url string) string {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET source: %d", res.StatusCode)
	}
	return string(b)
}

func TestClaimPayloadCarriesASourceURLAndTheOutputsKey(t *testing.T) {
	e := newEnv(t)
	e.enqueue()
	job := e.claim()

	src, _ := job["source"].(map[string]any)
	if src["size"] != float64(len("source-bytes")) || download(t, str(src, "url")) != "source-bytes" {
		t.Fatalf("source = %v", src)
	}
	enc, _ := job["encryption"].(map[string]any)
	key := e.keys.Derive("ast_1", str(job, "output_id"))
	kid := contentkey.KeyID(str(job, "output_id"))
	if str(enc, "method") != "aes-128" || enc["key_version"] != float64(1) ||
		str(enc, "key") != hex.EncodeToString(key[:]) || str(enc, "key_id") != hex.EncodeToString(kid[:]) {
		t.Fatalf("encryption = %v", enc)
	}
}

func TestSignWritesOnlyGrammarPathsUnderTheAttemptsOwnOutput(t *testing.T) {
	e := newEnv(t)
	e.enqueue()
	job := e.claim()
	token, base := str(job, "attempt_token"), "/worker/v1/attempts/"+str(job, "attempt_id")

	for _, bad := range [][]string{{"../uploads/ast_1/source"}, {"key/1"}, {"720p/evil.sh"}, {"/master.m3u8"}, {}} {
		if code, out := e.post(base+"/sign", token, map[string]any{"paths": bad}); code != http.StatusBadRequest {
			t.Errorf("sign %v: %d %v, want 400", bad, code, out)
		}
	}
	code, out := e.post(base+"/sign", token, map[string]any{"paths": []string{"720p/seg_00001.m4s", "master.m3u8"}, "source": true})
	if code != http.StatusOK || out["action"] != "continue" {
		t.Fatalf("sign: %d %v", code, out)
	}
	urls, _ := out["urls"].(map[string]any)
	put := str(urls, "720p/seg_00001.m4s")
	req, _ := http.NewRequest(http.MethodPut, put, strings.NewReader("segment"))
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("PUT signed segment: %v %v", res.StatusCode, err)
	}
	want := "media/ast_1/" + str(job, "output_id") + "/720p/seg_00001.m4s"
	if _, err := e.store.Head(context.Background(), want); err != nil {
		t.Fatalf("segment not stored at %s: %v", want, err)
	}
	if src, _ := out["source"].(map[string]any); download(t, str(src, "url")) != "source-bytes" {
		t.Fatal("a fresh source URL must download the source")
	}
}

func TestSignBudgetCapsFilesPerAttempt(t *testing.T) {
	e := newEnv(t)
	e.enqueue()
	job := e.claim()
	token, base := str(job, "attempt_token"), "/worker/v1/attempts/"+str(job, "attempt_id")
	e.exec(`UPDATE attempts SET signed_paths = $1`, workerapi.SignBudget-1)
	if code, _ := e.post(base+"/sign", token, map[string]any{"paths": []string{"master.m3u8"}}); code != http.StatusOK {
		t.Fatalf("last allowed path: %d", code)
	}
	if code, out := e.post(base+"/sign", token, map[string]any{"paths": []string{"poster.jpg"}}); code != http.StatusConflict || errorCode(out) != "sign_budget_exceeded" {
		t.Fatalf("over budget: %d %v", code, out)
	}
}

func TestSignTellsAStaleWorkerToStop(t *testing.T) {
	e := newEnv(t)
	e.enqueue()
	job := e.claim()
	e.exec(`UPDATE jobs SET status = 'cancelled'`)
	code, out := e.post("/worker/v1/attempts/"+str(job, "attempt_id")+"/sign", str(job, "attempt_token"), map[string]any{"paths": []string{"master.m3u8"}})
	if code != http.StatusOK || out["action"] != "stop" || out["urls"] != nil {
		t.Fatalf("sign for a cancelled job: %d %v", code, out)
	}
}

func TestClaimFailsAJobWhoseSourceIsGone(t *testing.T) {
	e := newEnv(t)
	e.exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default')`)
	e.exec(`INSERT INTO jobs (id, project_id, asset_id, kind) VALUES ('job_1', 'prj_default', 'ast_1', 'transcode')`)
	if code, _ := e.post("/worker/v1/claim", workerKey, capable); code != http.StatusNoContent {
		t.Fatalf("claim: %d, want 204", code)
	}
	var jobStatus, assetStatus, code string
	if err := e.pool.QueryRow(context.Background(), `SELECT j.status, a.status, a.error_code FROM jobs j JOIN assets a ON a.id = j.asset_id`).Scan(&jobStatus, &assetStatus, &code); err != nil {
		t.Fatal(err)
	}
	if jobStatus != "failed" || assetStatus != "errored" || code != "source_missing" {
		t.Fatalf("sourceless job: job %s, asset %s/%s", jobStatus, assetStatus, code)
	}
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func (e *env) post(path, bearer string, body any) (int, map[string]any) {
	e.t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	var out map[string]any
	if len(data) > 0 {
		if err := json.Unmarshal(data, &out); err != nil {
			e.t.Fatalf("%s: non-JSON body %q", path, data)
		}
	}
	return res.StatusCode, out
}

func errorCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

func with(overrides map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range capable {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

func TestClaimRequiresTheWorkerKey(t *testing.T) {
	e := newEnv(t)
	for _, bearer := range []string{"", "wrong"} {
		if code, body := e.post("/worker/v1/claim", bearer, capable); code != http.StatusUnauthorized || errorCode(body) != "unauthorized" {
			t.Fatalf("bearer %q: %d %v", bearer, code, body)
		}
	}
}

func TestClaimWithNoWorkIs204(t *testing.T) {
	e := newEnv(t)
	if code, _ := e.post("/worker/v1/claim", workerKey, capable); code != http.StatusNoContent {
		t.Fatalf("got %d, want 204", code)
	}
}

func TestClaimRefusesWorkersThatCannotMeetTheContract(t *testing.T) {
	e := newEnv(t)
	e.enqueue()
	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"future contract only", with(map[string]any{"contract_versions": []int{2}}), "unsupported_contract"},
		{"packager before 3.9.1", with(map[string]any{"packager_version": "v3.9.0"}), "packager_too_old"},
		{"no packager version", with(map[string]any{"packager_version": ""}), "packager_too_old"},
	}
	for _, c := range cases {
		code, body := e.post("/worker/v1/claim", workerKey, c.body)
		if code != http.StatusConflict || errorCode(body) != c.want {
			t.Errorf("%s: %d %v, want 409 %s", c.name, code, body, c.want)
		}
	}
	// None of those refusals took the job.
	if code, _ := e.post("/worker/v1/claim", workerKey, capable); code != http.StatusOK {
		t.Fatalf("capable worker: got %d", code)
	}
}

func TestClaimPayloadAndAttemptCalls(t *testing.T) {
	e := newEnv(t)
	e.enqueue()

	code, claim := e.post("/worker/v1/claim", workerKey, capable)
	if code != http.StatusOK {
		t.Fatalf("claim: %d %v", code, claim)
	}
	// Fields from the architecture's job payload.
	if claim["contract_version"] != float64(1) || claim["lease_seconds"] != float64(90) || claim["output_format"] != float64(1) {
		t.Fatalf("payload = %v", claim)
	}
	profile, _ := claim["profile"].(map[string]any)
	if profile["id"] != "default" || profile["version"] != float64(1) {
		t.Fatalf("profile = %v", claim["profile"])
	}
	attemptID, _ := claim["attempt_id"].(string)
	token, _ := claim["attempt_token"].(string)
	if attemptID == "" || token == "" {
		t.Fatalf("payload missing attempt id/token: %v", claim)
	}
	if _, ok := claim["storage"]; ok {
		t.Fatal("the payload must never carry storage credentials")
	}

	base := "/worker/v1/attempts/" + attemptID
	if code, body := e.post(base+"/heartbeat", token, map[string]any{"phase": "transcode", "progress": 0.25}); code != http.StatusOK || body["action"] != "continue" {
		t.Fatalf("heartbeat: %d %v", code, body)
	}
	if code, body := e.post(base+"/heartbeat", token, map[string]any{"progress": 2}); code != http.StatusBadRequest || errorCode(body) != "invalid_request" {
		t.Fatalf("progress out of range: %d %v", code, body)
	}
	if code, _ := e.post(base+"/heartbeat", "not-a-token", map[string]any{}); code != http.StatusUnauthorized {
		t.Fatalf("unknown token: %d", code)
	}
	if code, body := e.post("/worker/v1/attempts/att_other/heartbeat", token, map[string]any{}); code != http.StatusForbidden || errorCode(body) != "attempt_mismatch" {
		t.Fatalf("token for another attempt: %d %v", code, body)
	}
	if code, _ := e.post(base+"/heartbeat", workerKey, map[string]any{}); code != http.StatusUnauthorized {
		t.Fatalf("the worker key is not an attempt token: got %d", code)
	}

	code, body := e.post(base+"/fail", token, map[string]any{"permanent": true, "error_code": "unsupported_codec", "message": "no decoder"})
	if code != http.StatusOK || body["action"] != "stop" {
		t.Fatalf("fail: %d %v", code, body)
	}
	if code, body := e.post(base+"/heartbeat", token, map[string]any{}); code != http.StatusOK || body["action"] != "stop" || body["reason"] != "finished" {
		t.Fatalf("heartbeat after fail: %d %v", code, body)
	}
}
