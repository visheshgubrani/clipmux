package workerapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/queue"
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
	t    *testing.T
	srv  *httptest.Server
	exec func(string, ...any)
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := dbtest.New(t)
	api := workerapi.New(queue.New(pool), workerapi.BuiltinKey(workerKey))
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	return &env{t: t, srv: srv, exec: exec}
}

func (e *env) enqueue() {
	e.exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default')`)
	e.exec(`INSERT INTO jobs (id, project_id, asset_id, kind) VALUES ('job_1', 'prj_default', 'ast_1', 'transcode')`)
}

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
