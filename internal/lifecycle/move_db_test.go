package lifecycle

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/outputs"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/routes"
	"github.com/visheshgubrani/clipmux/internal/secrets"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/storage/s3test"
	"github.com/visheshgubrani/clipmux/internal/stores"
)

type moveRig struct {
	t        *testing.T
	pool     *pgxpool.Pool
	svc      *Service
	commit   *outputs.Committer
	fs, r2   storage.Storage
	r2ID     string
	source   []byte
	segments map[string]string
}

// newMoveRig puts ast_1 on the filesystem, with a committed and a retired
// output, under Worker delivery whose bucket is a fresh S3 bucket.
func newMoveRig(t *testing.T) *moveRig {
	t.Helper()
	b := s3test.New(t)
	pool := dbtest.New(t)
	fs, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: "http://unused"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fs.Close() })
	box, _ := secrets.NewBox(1, bytes.Repeat([]byte{9}, 32))
	set := stores.New(pool, box, fs)
	ctx := context.Background()
	row, err := set.AddS3(ctx, db.DefaultProjectID, stores.KindR2, stores.S3Config{Endpoint: b.Endpoint, Bucket: b.Name},
		stores.S3Secret{AccessKeyID: b.AccessKeyID, SecretAccessKey: b.SecretAccessKey})
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := set.Get(ctx, row.ID)
	commit := outputs.New(pool, queue.New(pool), set)
	r := &moveRig{t: t, pool: pool, svc: New(pool, set, commit), commit: commit, fs: fs, r2: r2, r2ID: row.ID,
		segments: map[string]string{}}
	r.svc.LateWrites = 0
	r.exec(`UPDATE projects SET delivery = 'worker', delivery_storage_id = $1`, row.ID)
	r.exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id, source_uri) VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default', 'uploads/ast_1/source')`)
	r.exec(`INSERT INTO outputs (id, project_id, asset_id, key_version, status, manifest) VALUES ('out_new', 'prj_default', 'ast_1', 1, 'committed', '{}')`)
	r.exec(`INSERT INTO outputs (id, project_id, asset_id, key_version, status, manifest, retired_at) VALUES ('out_old', 'prj_default', 'ast_1', 1, 'retired', '{}', now())`)
	r.exec(`INSERT INTO outputs (id, project_id, asset_id, key_version) VALUES ('out_lost', 'prj_default', 'ast_1', 1)`)
	r.exec(`UPDATE assets SET status = 'ready', current_output_id = 'out_new' WHERE id = 'ast_1'`)
	r.exec(`INSERT INTO playback_ids (id, project_id, asset_id, policy) VALUES ('pid_1', 'prj_default', 'ast_1', 'signed')`)
	// A source over one part goes up in parts.
	r.source = make([]byte, storage.PartSize+10)
	_, _ = rand.Read(r.source)
	r.put("uploads/ast_1/source", r.source)
	for _, k := range []string{"media/ast_1/out_new/master.m3u8", "media/ast_1/out_new/360p/seg_00001.m4s", "media/ast_1/out_old/360p/seg_00001.m4s"} {
		r.segments[k] = "bytes of " + k
		r.put(k, []byte(r.segments[k]))
	}
	r.put("media/ast_1/out_lost/360p/seg_00001.m4s", []byte("a lost attempt's file"))
	// Publish the route as it stands: on the filesystem, so "origin".
	r.refreshRoutes()
	return r
}

func (r *moveRig) exec(sql string, args ...any) {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(), sql, args...); err != nil {
		r.t.Fatal(err)
	}
}

func (r *moveRig) put(key string, b []byte) {
	r.t.Helper()
	if err := r.fs.Put(context.Background(), key, bytes.NewReader(b)); err != nil {
		r.t.Fatal(err)
	}
}

func (r *moveRig) refreshRoutes() {
	r.t.Helper()
	ctx := context.Background()
	q := db.New(r.pool)
	a, _ := q.GetAssetByID(ctx, "ast_1")
	if err := routes.QueueAssetRefresh(ctx, q, a.ProjectID, a.AppID, a.ID); err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.commit.SyncRoutes(ctx, 100); err != nil {
		r.t.Fatal(err)
	}
}

func (r *moveRig) read(st storage.Storage, key string) (string, bool) {
	r.t.Helper()
	rc, err := st.Get(context.Background(), key)
	if errors.Is(err, storage.ErrNotFound) {
		return "", false
	}
	if err != nil {
		r.t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return string(b), true
}

func (r *moveRig) run() {
	r.t.Helper()
	if _, err := r.svc.Run(context.Background()); err != nil {
		r.t.Fatal(err)
	}
}

func TestMovingAnAssetCopiesItSwitchesItsRoutesThenCleansUp(t *testing.T) {
	r := newMoveRig(t)
	ctx := context.Background()
	if got, _ := r.read(r.r2, "_routes/pid_1.json"); !strings.Contains(got, `"backend":"origin"`) {
		t.Fatalf("route before the move = %s, want origin", got)
	}
	n, err := r.svc.MoveLibrary(ctx, db.DefaultProjectID, r.r2ID)
	if err != nil || n != 1 {
		t.Fatalf("MoveLibrary = %d, %v; want 1 queued", n, err)
	}
	// A second request queues nothing: the asset already has its move.
	if n, _ := r.svc.MoveLibrary(ctx, db.DefaultProjectID, r.r2ID); n != 0 {
		t.Fatalf("second MoveLibrary queued %d", n)
	}
	r.run()

	// Copied: the source (in two parts) and both outputs a viewer may be pinned
	// to, but not the lost attempt's files.
	if got, ok := r.read(r.r2, "uploads/ast_1/source"); !ok || got != string(r.source) {
		t.Fatalf("source on R2: %d bytes, want %d", len(got), len(r.source))
	}
	for k, want := range r.segments {
		if got, _ := r.read(r.r2, k); got != want {
			t.Fatalf("%s on R2 = %q", k, got)
		}
	}
	if _, ok := r.read(r.r2, "media/ast_1/out_lost/360p/seg_00001.m4s"); ok {
		t.Fatal("a lost attempt's file was copied")
	}
	var storageID, phase string
	_ = r.pool.QueryRow(ctx, `SELECT storage_id FROM assets WHERE id = 'ast_1'`).Scan(&storageID)
	_ = r.pool.QueryRow(ctx, `SELECT coalesce(phase, '') FROM jobs WHERE kind = 'move'`).Scan(&phase)
	if storageID != r.r2ID || phase != "cleanup" {
		t.Fatalf("after the copy: storage %s, phase %q", storageID, phase)
	}
	if _, err := r.commit.SyncRoutes(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.read(r.r2, "_routes/pid_1.json"); !strings.Contains(got, `"backend":"r2"`) {
		t.Fatalf("route after the move = %s, want r2", got)
	}
	// The old copies stay until the Worker's route cache has expired.
	if _, ok := r.read(r.fs, "uploads/ast_1/source"); !ok {
		t.Fatal("the old source went before the cleanup pass")
	}
	r.exec(`UPDATE jobs SET available_at = now() WHERE kind = 'move'`)
	r.run()
	var status string
	_ = r.pool.QueryRow(ctx, `SELECT status FROM jobs WHERE kind = 'move'`).Scan(&status)
	if _, ok := r.read(r.fs, "uploads/ast_1/source"); ok || status != "succeeded" {
		t.Fatalf("after cleanup: old source still there = %v, job %s", ok, status)
	}
	var events int
	_ = r.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE kind = 'webhook' AND payload->>'type' = 'video.asset.updated' AND payload->'job'->>'kind' = 'move'`).Scan(&events)
	if events != 1 {
		t.Fatalf("updated events for the move = %d, want 1", events)
	}
}

func TestAMoveResumesWithoutCopyingTwice(t *testing.T) {
	r := newMoveRig(t)
	ctx := context.Background()
	if _, err := r.svc.MoveLibrary(ctx, db.DefaultProjectID, r.r2ID); err != nil {
		t.Fatal(err)
	}
	// A crash after copying one file: the next pass copies the rest only.
	if err := r.r2.Put(ctx, "media/ast_1/out_old/360p/seg_00001.m4s", strings.NewReader(r.segments["media/ast_1/out_old/360p/seg_00001.m4s"])); err != nil {
		t.Fatal(err)
	}
	before, _ := r.r2.Head(ctx, "media/ast_1/out_old/360p/seg_00001.m4s")
	r.run()
	after, _ := r.r2.Head(ctx, "media/ast_1/out_old/360p/seg_00001.m4s")
	if before.ETag != after.ETag {
		t.Fatal("an object already in the new store was copied again")
	}
	var storageID string
	_ = r.pool.QueryRow(ctx, `SELECT storage_id FROM assets WHERE id = 'ast_1'`).Scan(&storageID)
	if storageID != r.r2ID {
		t.Fatalf("storage = %s", storageID)
	}
}
