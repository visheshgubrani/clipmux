package worker

// End to end on one machine: real Postgres, filesystem storage and the
// worker API over HTTP, and the real ffmpeg and Shaka Packager. Skips without
// TEST_DATABASE_URL or the media tools.

import (
	"context"
	"encoding/json"
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
	"github.com/visheshgubrani/clipmux/internal/media"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/workerapi"
)

type rig struct {
	pool   *pgxpool.Pool
	store  *storage.Filesystem
	worker *Worker
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
	tl := tools(t)
	pool := dbtest.New(t)
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	store, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	keys, err := contentkey.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	api := workerapi.New(queue.New(pool), workerapi.BuiltinKey("k"), store, keys)
	mux.Handle("POST /worker/v1/", api.Handler())
	for _, m := range []string{"GET", "PUT"} {
		mux.Handle(m+" /_storage/", store.Handler())
	}
	return &rig{pool: pool, store: store, worker: &Worker{
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
		`INSERT INTO jobs (id, project_id, asset_id, kind) VALUES ('job_1', 'prj_default', 'ast_1', 'transcode')`,
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

func TestAnUnusableSourceFailsPermanently(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.store.Put(ctx, "uploads/ast_1/source", strings.NewReader(strings.Repeat("not a video ", 100))); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default')`,
		`INSERT INTO jobs (id, project_id, asset_id, kind) VALUES ('job_1', 'prj_default', 'ast_1', 'transcode')`,
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
