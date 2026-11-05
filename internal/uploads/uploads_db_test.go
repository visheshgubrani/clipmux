package uploads_test

// Real Postgres + real filesystem storage over HTTP, with 8-byte parts so
// multipart behaviour is visible. Expected behaviour comes from
// docs/architecture.md, "Multipart upload flow" and "Abandoned uploads".

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/assets"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/stores"
	"github.com/visheshgubrani/clipmux/internal/uploads"
)

const partSize = 8

type env struct {
	t        *testing.T
	pool     *pgxpool.Pool
	store    *storage.Filesystem
	svc      *uploads.Service
	url      string
	key      string // uploads:write + assets:read for app "LMS"
	storeDir string
	other    string // a key for another app
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.New(t)
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	storeDir := t.TempDir()
	store, err := storage.OpenFilesystem(storeDir, storage.FilesystemOptions{BaseURL: srv.URL, PartSize: partSize})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	for _, m := range []string{"GET", "PUT", "OPTIONS"} {
		mux.Handle(m+" /_storage/", store.Handler())
	}
	svc := uploads.New(pool, stores.Fixed(store))
	svc.MaxSize = 1000
	svc.Register(mux)
	assets.New(pool).Register(mux)
	if _, err := pool.Exec(ctx, `UPDATE projects SET media_url = 'https://media.example'`); err != nil {
		t.Fatal(err)
	}

	e := &env{t: t, pool: pool, store: store, svc: svc, url: srv.URL, storeDir: storeDir}
	for _, app := range []string{"app_lms", "app_site"} {
		if _, err := pool.Exec(ctx, `INSERT INTO apps (id, project_id, name) VALUES ($1, 'prj_default', $1)`, app); err != nil {
			t.Fatal(err)
		}
	}
	e.key, _, _ = apikeys.Create(ctx, pool, "prj_default", "app_lms", "backend", []string{apikeys.UploadsWrite, apikeys.AssetsRead})
	e.other, _, _ = apikeys.Create(ctx, pool, "prj_default", "app_site", "backend", []string{apikeys.UploadsWrite, apikeys.AssetsRead})
	return e
}

func (e *env) call(method, path, bearer string, body any, header map[string]string) (int, map[string]any, http.Header) {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		r = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, e.url+path, r)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out, res.Header
}

type created struct {
	UploadID, AssetID, Token string
	PartSize                 int
}

func (e *env) create(size int, extra map[string]any, header map[string]string) created {
	e.t.Helper()
	body := map[string]any{"size": size, "passthrough": "lesson-42"}
	for k, v := range extra {
		body[k] = v
	}
	code, out, _ := e.call("POST", "/v1/uploads", e.key, body, header)
	if code != http.StatusCreated {
		e.t.Fatalf("create: %d %v", code, out)
	}
	return created{UploadID: str(out, "upload_id"), AssetID: str(out, "asset_id"), Token: str(out, "upload_token"), PartSize: int(out["part_size"].(float64))}
}

func (e *env) sign(c created, parts ...int) []string {
	e.t.Helper()
	code, out, _ := e.call("POST", "/v1/uploads/"+c.UploadID+"/parts", c.Token, map[string]any{"part_numbers": parts}, nil)
	if code != http.StatusOK {
		e.t.Fatalf("sign %v: %d %v", parts, code, out)
	}
	var urls []string
	for _, p := range out["parts"].([]any) {
		urls = append(urls, p.(map[string]any)["url"].(string))
	}
	return urls
}

// put uploads a part the way a browser does and returns its ETag.
func (e *env) put(url, body string) string {
	e.t.Helper()
	req, _ := http.NewRequest("PUT", url, strings.NewReader(body))
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != http.StatusOK {
		e.t.Fatalf("part PUT: %v %v", res.StatusCode, err)
	}
	res.Body.Close()
	return res.Header.Get("ETag")
}

func (e *env) complete(c created, etags map[int]string) (int, map[string]any) {
	e.t.Helper()
	var parts []map[string]any
	for n := 1; n <= len(etags); n++ {
		parts = append(parts, map[string]any{"part_number": n, "etag": etags[n]})
	}
	code, out, _ := e.call("POST", "/v1/uploads/"+c.UploadID+"/complete", c.Token, map[string]any{"parts": parts}, nil)
	return code, out
}

func (e *env) row(sql string, args ...any) []any {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), sql, args...)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		e.t.Fatalf("no row for %s", sql)
	}
	v, _ := rows.Values()
	return v
}

func (e *env) sweep() int {
	e.t.Helper()
	n, err := e.svc.Sweep(context.Background(), 100)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func errCode(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	return str(e, "code")
}

// The order @uppy/aws-s3 calls its callbacks in (D011): createMultipartUpload
// (via the app backend), signPart per part, listParts on resume,
// completeMultipartUpload.
func TestUppyCallOrderUploadsAndQueuesProcessing(t *testing.T) {
	e := newEnv(t)
	c := e.create(20, map[string]any{"playback_policy": "signed"}, map[string]string{"Idempotency-Key": "lesson-42-v1"})
	if c.PartSize != partSize || c.Token == "" {
		t.Fatalf("create = %+v", c)
	}

	etags := map[int]string{}
	etags[1] = e.put(e.sign(c, 1)[0], "AAAAAAAA")
	// The tab reloads: listParts tells the browser what is already stored.
	code, listed, _ := e.call("GET", "/v1/uploads/"+c.UploadID+"/parts", c.Token, nil, nil)
	if code != http.StatusOK || len(listed["parts"].([]any)) != 1 {
		t.Fatalf("listParts after reload: %d %v", code, listed)
	}
	etags[2] = e.put(e.sign(c, 2)[0], "BBBBBBBB")
	etags[3] = e.put(e.sign(c, 3)[0], "CCCC")

	code, done := e.complete(c, etags)
	if code != http.StatusOK || str(done, "status") != "processing" || str(done, "id") != c.AssetID {
		t.Fatalf("complete: %d %v", code, done)
	}
	// A retried complete (lost response) is harmless.
	if code, again := e.complete(c, etags); code != http.StatusOK || str(again, "status") != "processing" {
		t.Fatalf("second complete: %d %v", code, again)
	}

	got := e.row(`SELECT a.status, a.source_uri, j.kind, j.status, u.status FROM assets a
		JOIN jobs j ON j.asset_id = a.id JOIN uploads u ON u.asset_id = a.id WHERE a.id = $1`, c.AssetID)
	if got[0] != "processing" || got[1] != "uploads/"+c.AssetID+"/source" || got[2] != "transcode" || got[3] != "queued" || got[4] != "completed" {
		t.Fatalf("after complete: %v", got)
	}
	if n := e.row(`SELECT count(*) FROM jobs WHERE asset_id = $1`, c.AssetID)[0]; n != int64(1) {
		t.Fatalf("jobs = %v, want exactly 1", n)
	}
	rc, err := e.store.Get(context.Background(), "uploads/"+c.AssetID+"/source")
	if err != nil {
		t.Fatal(err)
	}
	source, _ := io.ReadAll(rc)
	rc.Close()
	if string(source) != "AAAAAAAABBBBBBBBCCCC" {
		t.Fatalf("source = %q", source)
	}

	// The app reads the asset back, by ID and by its own reference.
	code, asset, _ := e.call("GET", "/v1/assets/"+c.AssetID, e.key, nil, nil)
	pids, _ := asset["playback_ids"].([]any)
	if code != http.StatusOK || str(asset, "status") != "processing" || len(pids) != 1 || str(pids[0].(map[string]any), "policy") != "signed" ||
		str(asset, "base_url") != "https://media.example" || pids[0].(map[string]any)["playable"] != false {
		t.Fatalf("GET asset: %d %v", code, asset)
	}
	code, found, _ := e.call("GET", "/v1/assets?passthrough=lesson-42", e.key, nil, nil)
	if data, _ := found["data"].([]any); code != http.StatusOK || len(data) != 1 {
		t.Fatalf("lookup by passthrough: %d %v", code, found)
	}
	if code, _, _ := e.call("GET", "/v1/assets/"+c.AssetID, e.other, nil, nil); code != http.StatusNotFound {
		t.Fatalf("another app read the asset: %d", code)
	}
}

func TestCreateValidatesInputAndScope(t *testing.T) {
	e := newEnv(t)
	readOnly, _, _ := apikeys.Create(context.Background(), e.pool, "prj_default", "app_lms", "ro", []string{apikeys.AssetsRead})
	cases := []struct {
		name, bearer string
		body         map[string]any
		code         int
		err          string
	}{
		{"no key", "", map[string]any{"size": 10}, 401, "unauthorized"},
		{"no uploads:write", readOnly, map[string]any{"size": 10}, 403, "insufficient_scope"},
		{"zero size", e.key, map[string]any{"size": 0}, 400, "invalid_request"},
		{"over the instance limit", e.key, map[string]any{"size": 1001}, 413, "too_large"},
		{"unknown policy", e.key, map[string]any{"size": 10, "playback_policy": "private"}, 400, "invalid_request"},
		{"long passthrough", e.key, map[string]any{"size": 10, "passthrough": strings.Repeat("x", 256)}, 400, "invalid_request"},
	}
	for _, c := range cases {
		code, out, _ := e.call("POST", "/v1/uploads", c.bearer, c.body, nil)
		if code != c.code || errCode(out) != c.err {
			t.Errorf("%s: %d %v, want %d %s", c.name, code, out, c.code, c.err)
		}
	}
	if n := e.row(`SELECT count(*) FROM assets`)[0]; n != int64(0) {
		t.Fatalf("rejected creates left %v assets", n)
	}
}

func TestIdempotentCreateNeverDuplicatesAnAsset(t *testing.T) {
	e := newEnv(t)
	idem := map[string]string{"Idempotency-Key": "k-1"}
	first := e.create(20, nil, idem)
	retry := e.create(20, nil, idem)
	if retry.UploadID != first.UploadID || retry.AssetID != first.AssetID {
		t.Fatalf("retry created a new upload: %+v vs %+v", retry, first)
	}
	// Tokens are never stored, so a replay rotates the token.
	if retry.Token == first.Token {
		t.Fatal("replayed token must be freshly issued")
	}
	if code, _, _ := e.call("GET", "/v1/uploads/"+first.UploadID+"/parts", first.Token, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("rotated-out token still works: %d", code)
	}
	if code, _, _ := e.call("GET", "/v1/uploads/"+first.UploadID+"/parts", retry.Token, nil, nil); code != http.StatusOK {
		t.Fatalf("new token: %d", code)
	}

	// Once the upload has finished, a replay returns the same IDs and no
	// token, rather than a token that can never work.
	e.pool.Exec(context.Background(), `UPDATE uploads SET status = 'completed' WHERE id = $1`, first.UploadID)
	if code, out, _ := e.call("POST", "/v1/uploads", e.key, map[string]any{"size": 20, "passthrough": "lesson-42"}, idem); code != http.StatusCreated || str(out, "upload_id") != first.UploadID || out["upload_token"] != nil {
		t.Fatalf("replay of a finished upload: %d %v", code, out)
	}

	code, out, _ := e.call("POST", "/v1/uploads", e.key, map[string]any{"size": 21, "passthrough": "lesson-42"}, idem)
	if code != http.StatusUnprocessableEntity || errCode(out) != "idempotency_key_reused" {
		t.Fatalf("same key, different body: %d %v", code, out)
	}
	// Keys are per app: another app may use the same key.
	code, _, _ = e.call("POST", "/v1/uploads", e.other, map[string]any{"size": 20}, idem)
	if code != http.StatusCreated {
		t.Fatalf("another app's key: %d", code)
	}
	if n := e.row(`SELECT count(*) FROM assets WHERE app_id = 'app_lms'`)[0]; n != int64(1) {
		t.Fatalf("LMS assets = %v, want 1", n)
	}
}

func TestPartsAreSignedOnlyWithinTheDeclaredSize(t *testing.T) {
	e := newEnv(t)
	c := e.create(20, nil, nil) // 3 parts
	for _, body := range []map[string]any{
		{"part_numbers": []int{4}},
		{"part_numbers": []int{0}},
		{"part_numbers": []int{}},
		{"part_numbers": make([]int, 101)},
	} {
		if code, out, _ := e.call("POST", "/v1/uploads/"+c.UploadID+"/parts", c.Token, body, nil); code != http.StatusBadRequest {
			t.Errorf("%v: %d %v, want 400", body, code, out)
		}
	}
	other := e.create(20, nil, nil)
	if code, out, _ := e.call("POST", "/v1/uploads/"+c.UploadID+"/parts", other.Token, map[string]any{"part_numbers": []int{1}}, nil); code != http.StatusForbidden || errCode(out) != "upload_mismatch" {
		t.Fatalf("token for another upload: %d %v", code, out)
	}
	if code, _, _ := e.call("POST", "/v1/uploads/"+c.UploadID+"/parts", e.key, map[string]any{"part_numbers": []int{1}}, nil); code != http.StatusUnauthorized {
		t.Fatalf("an API key is not an upload token: %d", code)
	}
}

func TestBrowserCanCallTheUploadEndpointsCrossOrigin(t *testing.T) {
	e := newEnv(t)
	c := e.create(20, nil, nil)
	code, _, h := e.call("OPTIONS", "/v1/uploads/"+c.UploadID+"/parts", "", nil, map[string]string{
		"Origin": "https://lms.example", "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "authorization,content-type",
	})
	if code != http.StatusNoContent || h.Get("Access-Control-Allow-Origin") != "*" || !strings.Contains(strings.ToLower(h.Get("Access-Control-Allow-Headers")), "authorization") {
		t.Fatalf("preflight: %d %v", code, h)
	}
}

func TestIncompleteCompleteIsRejectedAndRetryable(t *testing.T) {
	e := newEnv(t)
	c := e.create(20, nil, nil)
	urls := e.sign(c, 1, 2, 3)
	etags := map[int]string{1: e.put(urls[0], "AAAAAAAA"), 2: e.put(urls[1], "BBBBBBBB")}

	code, out := e.complete(c, etags)
	if code != http.StatusBadRequest || errCode(out) != "invalid_parts" {
		t.Fatalf("complete with a part missing: %d %v", code, out)
	}
	if s := e.row(`SELECT status FROM uploads WHERE id = $1`, c.UploadID)[0]; s != "active" {
		t.Fatalf("upload = %v, want still active", s)
	}
	etags[3] = e.put(urls[2], "CCCC")
	if code, out := e.complete(c, etags); code != http.StatusOK {
		t.Fatalf("complete after the last part: %d %v", code, out)
	}
}

func TestCompleteRecoversWhenStorageFinishedButTheDatabaseDidNot(t *testing.T) {
	e := newEnv(t)
	c := e.create(8, nil, nil)
	etag := e.put(e.sign(c, 1)[0], "AAAAAAAA")
	// Simulate a crash between the storage completion and the commit.
	var mp string
	e.pool.QueryRow(context.Background(), `SELECT multipart_id FROM uploads WHERE id = $1`, c.UploadID).Scan(&mp)
	if err := e.store.CompleteMultipart(context.Background(), "uploads/"+c.AssetID+"/source", mp, []storage.Part{{Number: 1, ETag: etag}}); err != nil {
		t.Fatal(err)
	}
	if code, out := e.complete(c, map[int]string{1: etag}); code != http.StatusOK || str(out, "status") != "processing" {
		t.Fatalf("retry after a crash: %d %v", code, out)
	}
}

func TestCancelByTheBrowserOrTheBackend(t *testing.T) {
	e := newEnv(t)
	c := e.create(20, nil, nil)
	url := e.sign(c, 1)[0]

	if code, _, _ := e.call("POST", "/v1/uploads/"+c.UploadID+"/cancel", e.other, nil, nil); code != http.StatusNotFound {
		t.Fatalf("another app cancelled the upload: %d", code)
	}
	code, out, _ := e.call("POST", "/v1/uploads/"+c.UploadID+"/cancel", c.Token, nil, nil)
	if code != http.StatusOK || str(out, "status") != "errored" || str(out, "error_code") != "upload_cancelled" {
		t.Fatalf("cancel: %d %v", code, out)
	}
	if code, _, _ := e.call("POST", "/v1/uploads/"+c.UploadID+"/cancel", c.Token, nil, nil); code != http.StatusOK {
		t.Fatalf("cancel twice: %d, want 200", code)
	}
	req, _ := http.NewRequest("PUT", url, strings.NewReader("AAAAAAAA"))
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != http.StatusNotFound {
		t.Fatalf("part PUT after cancel: %d, want 404", res.StatusCode)
	}
	if code, out := e.complete(c, map[int]string{1: `"x"`}); code != http.StatusConflict || errCode(out) != "upload_not_active" {
		t.Fatalf("complete after cancel: %d %v", code, out)
	}
	if e.svc.Sweep(context.Background(), 100); e.row(`SELECT status FROM uploads WHERE id = $1`, c.UploadID)[0] != "cancelled" {
		t.Fatal("the sweeper must never complete a cancelled upload")
	}
	ev := e.row(`SELECT payload->>'type', payload->'data'->>'error_code' FROM outbox WHERE asset_id = $1`, c.AssetID)
	if ev[0] != "video.asset.errored" || ev[1] != "upload_cancelled" {
		t.Fatalf("event = %v", ev)
	}

	// The app's backend can cancel with its API key too.
	b := e.create(20, nil, nil)
	if code, out, _ := e.call("POST", "/v1/uploads/"+b.UploadID+"/cancel", e.key, nil, nil); code != http.StatusOK {
		t.Fatalf("backend cancel: %d %v", code, out)
	}
}

func TestSweeperCompletesUploadsWhoseTabClosed(t *testing.T) {
	e := newEnv(t)
	c := e.create(20, nil, nil)
	urls := e.sign(c, 1, 2, 3)
	e.put(urls[0], "AAAAAAAA")
	e.put(urls[1], "BBBBBBBB")
	e.put(urls[2], "CCCC") // every part landed, then the tab closed before complete

	if n := e.sweep(); n != 1 {
		t.Fatalf("sweep handled %d, want 1", n)
	}
	got := e.row(`SELECT a.status, u.status, (SELECT count(*) FROM jobs WHERE asset_id = a.id) FROM assets a JOIN uploads u ON u.asset_id = a.id WHERE a.id = $1`, c.AssetID)
	if got[0] != "processing" || got[1] != "completed" || got[2] != int64(1) {
		t.Fatalf("after sweep: %v", got)
	}
}

func TestSweeperExpiresOnlyAfterADayWithoutActivity(t *testing.T) {
	e := newEnv(t)
	c := e.create(20, nil, nil)
	urls := e.sign(c, 1, 2, 3)
	idle := func() {
		if _, err := e.pool.Exec(context.Background(), `UPDATE uploads SET last_activity_at = now() - interval '25 hours' WHERE id = $1`, c.UploadID); err != nil {
			t.Fatal(err)
		}
	}

	// A slow upload: a new part landed since the last sweep, so it is not idle.
	e.put(urls[0], "AAAAAAAA")
	idle()
	e.sweep()
	if s := e.row(`SELECT status, parts_seen FROM uploads WHERE id = $1`, c.UploadID); s[0] != "active" || s[1] != int32(1) {
		t.Fatalf("slow upload after sweep: %v, want active with 1 part seen", s)
	}

	// Now nothing new for over a day.
	idle()
	e.sweep()
	got := e.row(`SELECT u.status, a.status, a.error_code FROM uploads u JOIN assets a ON a.id = u.asset_id WHERE u.id = $1`, c.UploadID)
	if got[0] != "expired" || got[1] != "errored" || got[2] != "upload_expired" {
		t.Fatalf("idle upload: %v", got)
	}
	req, _ := http.NewRequest("PUT", urls[1], strings.NewReader("BBBBBBBB"))
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != http.StatusNotFound {
		t.Fatalf("an expired upload's storage must be aborted: part PUT %d", res.StatusCode)
	}
}

func TestSigningPartsCountsAsActivity(t *testing.T) {
	e := newEnv(t)
	c := e.create(20, nil, nil)
	e.pool.Exec(context.Background(), `UPDATE uploads SET last_activity_at = now() - interval '25 hours' WHERE id = $1`, c.UploadID)
	e.sign(c, 1)
	var idle bool
	e.pool.QueryRow(context.Background(), `SELECT last_activity_at < now() - interval '1 minute' FROM uploads WHERE id = $1`, c.UploadID).Scan(&idle)
	if idle {
		t.Fatal("a part URL request must count as activity")
	}
}

func TestADisabledAppsUploadTokensStopWorking(t *testing.T) {
	e := newEnv(t)
	c := e.create(20, nil, nil)
	e.pool.Exec(context.Background(), `UPDATE apps SET disabled_at = now() WHERE id = 'app_lms'`)
	if code, _, _ := e.call("POST", "/v1/uploads/"+c.UploadID+"/parts", c.Token, map[string]any{"part_numbers": []int{1}}, nil); code != http.StatusUnauthorized {
		t.Fatalf("upload token of a disabled app: %d, want 401", code)
	}
}

func TestAnUploadThatKeepsFailingCantStarveTheSweeper(t *testing.T) {
	e := newEnv(t)
	broken, healthy := e.create(20, nil, nil), e.create(20, nil, nil)
	e.pool.Exec(context.Background(), `UPDATE uploads SET last_activity_at = now() - interval '26 hours' WHERE id = $1`, broken.UploadID)
	e.pool.Exec(context.Background(), `UPDATE uploads SET last_activity_at = now() - interval '25 hours' WHERE id = $1`, healthy.UploadID)
	// Corrupt the broken upload's staging so every pass over it errors.
	var mp string
	e.pool.QueryRow(context.Background(), `SELECT multipart_id FROM uploads WHERE id = $1`, broken.UploadID).Scan(&mp)
	if err := os.WriteFile(filepath.Join(e.storeDir, "multipart", mp, "meta.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		_, _ = e.svc.Sweep(context.Background(), 1) // one upload per pass
	}
	if s := e.row(`SELECT status FROM uploads WHERE id = $1`, healthy.UploadID)[0]; s != "expired" {
		t.Fatalf("healthy idle upload = %v after two passes, want expired", s)
	}
}
