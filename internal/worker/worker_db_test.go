package worker

// End to end on one machine: real Postgres, filesystem storage and the
// worker API over HTTP, and the real ffmpeg and Shaka Packager. Skips without
// TEST_DATABASE_URL or the media tools.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/delivery"
	"github.com/visheshgubrani/clipmux/internal/media"
	"github.com/visheshgubrani/clipmux/internal/outputs"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/storage/s3test"
	"github.com/visheshgubrani/clipmux/internal/stores"
	"github.com/visheshgubrani/clipmux/internal/tokens"
	"github.com/visheshgubrani/clipmux/internal/workerapi"
)

type rig struct {
	pool   *pgxpool.Pool
	store  storage.Storage
	media  string // base URL serving delivery
	worker *Worker
	api    *workerapi.API
}

func tools(t *testing.T) media.Tools {
	t.Helper()
	tl := media.DefaultTools()
	if p := os.Getenv("CLIPMUX_PACKAGER"); p != "" {
		tl.Packager = p
	}
	for _, bin := range []string{tl.FFmpeg, tl.FFprobe, tl.Packager} {
		if _, err := exec.LookPath(bin); err != nil {
			if os.Getenv("CLIPMUX_REQUIRE_MEDIA_TOOLS") == "1" {
				t.Fatalf("%s is required: %v", bin, err)
			}
			t.Skipf("%s not found", bin)
		}
	}
	return tl
}

func newRig(t *testing.T) *rig {
	t.Helper()
	return newRigOn(t, workerapi.BuiltinKey("k"), fsStore(t))
}

func fsStore(t *testing.T) func(base string, mux *http.ServeMux) storage.Storage {
	return func(base string, mux *http.ServeMux) storage.Storage {
		fs, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: base})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { fs.Close() })
		for _, m := range []string{"GET", "PUT"} {
			mux.Handle(m+" /_storage/", fs.Handler())
		}
		return fs
	}
}

// newS3Rig runs the same pipeline on an S3-compatible bucket: the worker
// downloads and uploads through presigned S3 URLs, never through serve.
func newS3Rig(t *testing.T) *rig {
	t.Helper()
	return newRigOn(t, workerapi.BuiltinKey("k"), s3Store(t))
}

func s3Store(t *testing.T) func(string, *http.ServeMux) storage.Storage {
	return func(string, *http.ServeMux) storage.Storage {
		b := s3test.New(t)
		st, err := storage.OpenS3(storage.S3Options{Endpoint: b.Endpoint, Bucket: b.Name, AccessKeyID: b.AccessKeyID, SecretAccessKey: b.SecretAccessKey})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
}

func newRigOn(t *testing.T, auth workerapi.WorkerAuth, open func(base string, mux *http.ServeMux) storage.Storage) *rig {
	t.Helper()
	tl := tools(t)
	pool := dbtest.New(t)
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	store := open(srv.URL, mux)
	keys, err := contentkey.FromHex(1, "0f0e0d0c0b0a090807060504030201000f0e0d0c0b0a09080706050403020100")
	if err != nil {
		t.Fatal(err)
	}
	q := queue.New(pool)
	committer := outputs.New(pool, q, stores.Fixed(store))
	api := workerapi.New(q, auth, stores.Fixed(store), contentkey.NewRing(keys), committer)
	mux.Handle("POST /worker/v1/", api.Handler())
	playback := delivery.New(pool, stores.Fixed(store), contentkey.NewRing(keys))
	committer.OnPublished, committer.OnKeys = playback.Invalidate, playback.InvalidateKeys // as serve wires them
	playback.Register(mux)
	return &rig{pool: pool, store: store, media: srv.URL, api: api, worker: &Worker{
		Server: srv.URL, Key: "k", WorkDir: t.TempDir(), Tools: tl, HTTP: http.DefaultClient,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Wait: time.Millisecond,
	}}
}

// uploaded stores a generated 8 s clip as an asset's source and queues it.
func (r *rig) uploaded(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	clip := filepath.Join(t.TempDir(), "clip.mp4")
	out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=25",
		"-f", "lavfi", "-i", "sine=frequency=330", "-t", "8", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", clip).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	f, _ := os.Open(clip)
	defer f.Close()
	if err := r.store.Put(ctx, "uploads/ast_1/source", f); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default')`,
		`INSERT INTO jobs (id, project_id, asset_id, kind, routed_to) VALUES ('job_1', 'prj_default', 'ast_1', 'transcode', 'workers')`,
	} {
		if _, err := r.pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorkerUploadsAnEncryptedOutputThroughSignedURLs(t *testing.T) {
	r := newRig(t)
	r.uploaded(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	job, err := r.worker.claim(ctx)
	if err != nil || job == nil {
		t.Fatalf("claim: %v, %v", job, err)
	}
	dir := filepath.Join(r.worker.WorkDir, job.AttemptID)
	m, code, err := r.worker.process(ctx, job, dir, func(string, float64) {})
	if err != nil {
		t.Fatalf("process (%s): %v", code, err)
	}

	prefix := "media/ast_1/" + job.OutputID + "/"
	objs, _, err := r.store.List(ctx, prefix, "")
	if err != nil {
		t.Fatal(err)
	}
	stored := map[string]bool{}
	for _, o := range objs {
		rel := strings.TrimPrefix(o.Key, prefix)
		if !media.ValidOutputPath(rel) {
			t.Errorf("stored %q outside the output grammar", rel)
		}
		stored[rel] = true
	}
	for _, f := range m.Files {
		if !stored[f.Path] {
			t.Errorf("manifest lists %s but it was not uploaded", f.Path)
		}
	}
	for _, want := range []string{"manifest.json", "master.m3u8", "poster.jpg", "360p/index.m3u8", "360p/init.mp4", "360p/seg_00001.m4s", "audio/index.m3u8"} {
		if !stored[want] {
			t.Errorf("%s was not uploaded", want)
		}
	}
	rc, err := r.store.Get(ctx, prefix+"manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var got media.Manifest
	_ = json.NewDecoder(rc).Decode(&got)
	rc.Close()
	if got.KeyVersion != 1 || len(got.Renditions) != 1 || got.Renditions[0].Name != "360p" {
		t.Fatalf("stored manifest = %+v", got)
	}

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("process should leave cleanup to handle: %v", err)
	}
}

func TestACancelledJobStopsTheWorkerWithoutReportingAFailure(t *testing.T) {
	r := newRig(t)
	r.uploaded(t)
	r.worker.HeartbeatEvery = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	job, err := r.worker.claim(ctx)
	if err != nil || job == nil {
		t.Fatalf("claim: %v, %v", job, err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE jobs SET status = 'cancelled'`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := r.worker.handle(ctx, job); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatal("the worker kept going after the server said stop")
	}
	var status string
	var failures int
	if err := r.pool.QueryRow(ctx, `SELECT a.status, j.failed_count FROM attempts a JOIN jobs j ON j.id = a.job_id`).Scan(&status, &failures); err != nil {
		t.Fatal(err)
	}
	if status != "running" || failures != 0 {
		t.Fatalf("attempt %s with %d failures: a stop is not a failure", status, failures)
	}
	if _, err := os.Stat(filepath.Join(r.worker.WorkDir, job.AttemptID)); !os.IsNotExist(err) {
		t.Fatal("the attempt directory must be removed")
	}
	objs, _, _ := r.store.List(ctx, "media/", "")
	if len(objs) != 0 {
		t.Fatalf("a stopped attempt uploaded %d files", len(objs))
	}
}

func TestAWorkerShuttingDownHandsItsAttemptBack(t *testing.T) {
	r := newRig(t)
	r.uploaded(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job, err := r.worker.claim(ctx)
	if err != nil || job == nil {
		t.Fatalf("claim: %v, %v", job, err)
	}
	time.AfterFunc(300*time.Millisecond, cancel) // SIGINT mid-encode
	if err := r.worker.handle(ctx, job); !errors.Is(err, context.Canceled) {
		t.Fatalf("handle: %v, want context.Canceled", err)
	}
	var status, code, jobStatus string
	var lost, failures int
	if err := r.pool.QueryRow(context.Background(),
		`SELECT a.status, coalesce(a.error_code, ''), j.status, j.lost_count, j.failed_count FROM attempts a JOIN jobs j ON j.id = a.job_id`,
	).Scan(&status, &code, &jobStatus, &lost, &failures); err != nil {
		t.Fatal(err)
	}
	if status != "lost" || code != "worker_stopped" || jobStatus != "queued" || lost != 1 || failures != 0 {
		t.Fatalf("attempt %s/%s, job %s lost %d failed %d; want lost/worker_stopped, queued, 1, 0",
			status, code, jobStatus, lost, failures)
	}
}

func TestAnUnusableSourceFailsPermanently(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.store.Put(ctx, "uploads/ast_1/source", strings.NewReader(strings.Repeat("not a video ", 100))); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default')`,
		`INSERT INTO jobs (id, project_id, asset_id, kind, routed_to) VALUES ('job_1', 'prj_default', 'ast_1', 'transcode', 'workers')`,
	} {
		if _, err := r.pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := r.worker.RunOnce(ctx); err != nil || !got {
		t.Fatalf("RunOnce = %v, %v", got, err)
	}
	var job, asset, code string
	if err := r.pool.QueryRow(ctx, `SELECT j.status, a.status, a.error_code FROM jobs j JOIN assets a ON a.id = j.asset_id`).Scan(&job, &asset, &code); err != nil {
		t.Fatal(err)
	}
	if job != "failed" || asset != "errored" || code != "unsupported_source" {
		t.Fatalf("job %s, asset %s/%s; want failed, errored/unsupported_source with no retry", job, asset, code)
	}
}

// The whole loop on one machine: claim, encode, encrypt, upload, commit,
// publish, ready.
func TestRunOnceTakesAnUploadedSourceToReady(t *testing.T) {
	r := newRig(t)
	r.uploaded(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := r.pool.Exec(ctx, `INSERT INTO playback_ids (id, project_id, asset_id, policy) VALUES ('pid_1', 'prj_default', 'ast_1', 'signed')`); err != nil {
		t.Fatal(err)
	}

	got, err := r.worker.RunOnce(ctx)
	if err != nil || !got {
		t.Fatalf("RunOnce = %v, %v", got, err)
	}
	var asset, output, job, event string
	err = r.pool.QueryRow(ctx, `SELECT a.status, o.status, j.status, e.payload->>'type'
		FROM assets a JOIN outputs o ON o.id = a.current_output_id JOIN jobs j ON j.asset_id = a.id
		JOIN outbox e ON e.asset_id = a.id AND e.kind = 'webhook'`).Scan(&asset, &output, &job, &event)
	if err != nil {
		t.Fatal(err)
	}
	if asset != "ready" || output != "committed" || job != "succeeded" || event != "video.asset.ready" {
		t.Fatalf("asset %s, output %s, job %s, event %s", asset, output, job, event)
	}
	entries, _ := os.ReadDir(r.worker.WorkDir)
	if len(entries) != 0 {
		t.Fatalf("work dir not cleaned: %d entries", len(entries))
	}
}

// Milestone M1 (docs/plans/P1-core-product.md): an upload goes to ready, and
// an HLS client plays the signed, encrypted output over HTTP: it fetches the
// generated master, the pinned media playlists, the content key and the
// encrypted segments, and decodes frames. ffmpeg is the client here; the
// browser players are the Phase 1 exit test.
func TestM1SignedEncryptedPlaybackOverHTTP(t *testing.T) {
	m1(t, newRig(t))
}

// P2-T01: the walking skeleton holds on S3-compatible storage too.
func TestM1SignedEncryptedPlaybackOnS3(t *testing.T) {
	m1(t, newS3Rig(t))
}

func m1(t *testing.T, r *rig) {
	t.Helper()
	r.uploaded(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	privPEM, pubPEM, err := tokens.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`INSERT INTO playback_ids (id, project_id, asset_id, policy) VALUES ('pid_1', 'prj_default', 'ast_1', 'signed')`,
		`INSERT INTO signing_keys (id, project_id, app_id, public_key_pem) VALUES ('kid_1', 'prj_default', 'app_dashboard', '` + pubPEM + `')`,
	} {
		if _, err := r.pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	priv, _ := tokens.ParsePrivateKey(privPEM)
	now := time.Now().Unix()
	tok, _ := tokens.Sign(priv, "kid_1", tokens.Claims{Sub: "pid_1", Aud: "v", Iat: now, Exp: now + 4*3600})
	signed := r.media + "/v/" + tok + "/pid_1/master.m3u8"

	out, err := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-protocol_whitelist", "http,tcp,crypto,data",
		"-i", signed, "-map", "0:v:0", "-map", "0:a:0", "-f", "framemd5", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg could not play the signed URL: %v\n%s", err, out)
	}
	frames := strings.Count(string(out), "\n0,") // video stream frame lines
	audio := strings.Count(string(out), "\n1,")
	if frames < 190 || audio == 0 { // 8 s at 25 fps
		t.Fatalf("decoded %d video frames and %d audio frames, want about 200 and some", frames, audio)
	}

	// Without the token, the same video is refused.
	res, err := http.Get(r.media + "/p/pid_1/master.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("unsigned request: %d, want 403", res.StatusCode)
	}
}
