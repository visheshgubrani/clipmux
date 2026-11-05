package routes_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
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
	"github.com/visheshgubrani/clipmux/internal/tokens"
)

type rig struct {
	t      *testing.T
	pool   *pgxpool.Pool
	set    *stores.Set
	bucket storage.Storage // what the Worker reads
	commit *outputs.Committer
}

// newRig switches the project to Worker delivery on a fresh S3 bucket and
// stores ast_r2 there, with ast_fs left on the filesystem.
func newRig(t *testing.T) *rig {
	t.Helper()
	b := s3test.New(t)
	pool := dbtest.New(t)
	fs, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: "http://localhost:8080"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fs.Close() })
	box, _ := secrets.NewBox(1, bytes.Repeat([]byte{6}, 32))
	set := stores.New(pool, box, fs)
	ctx := context.Background()
	row, err := set.AddS3(ctx, db.DefaultProjectID, stores.KindR2,
		stores.S3Config{Endpoint: b.Endpoint, Bucket: b.Name}, stores.S3Secret{AccessKeyID: b.AccessKeyID, SecretAccessKey: b.SecretAccessKey})
	if err != nil {
		t.Fatal(err)
	}
	bucket, _ := set.Get(ctx, row.ID)
	r := &rig{t: t, pool: pool, set: set, bucket: bucket, commit: outputs.New(pool, queue.New(pool), set)}
	r.exec(`UPDATE projects SET delivery = 'worker', delivery_storage_id = $1`, row.ID)
	for _, a := range []struct{ id, storage string }{{"ast_r2", row.ID}, {"ast_fs", "sto_default"}} {
		r.exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ($1, 'prj_default', 'app_dashboard', 'processing', $2)`, a.id, a.storage)
		r.exec(`INSERT INTO outputs (id, project_id, asset_id, key_version, status, manifest) VALUES ($1, 'prj_default', $2, 1, 'committed', '{}')`, "out_"+a.id, a.id)
		r.exec(`UPDATE assets SET current_output_id = $1, status = 'ready' WHERE id = $2`, "out_"+a.id, a.id)
		r.exec(`INSERT INTO playback_ids (id, project_id, asset_id, policy) VALUES ($1, 'prj_default', $2, 'signed')`, "pid_"+a.id, a.id)
	}
	return r
}

func (r *rig) exec(sql string, args ...any) {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(), sql, args...); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) tx(fn func(q *db.Queries) error) {
	r.t.Helper()
	if err := pgx.BeginFunc(context.Background(), r.pool, func(tx pgx.Tx) error { return fn(db.New(tx)) }); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) asset(id string) db.Asset {
	r.t.Helper()
	a, err := db.New(r.pool).GetAssetByID(context.Background(), id)
	if err != nil {
		r.t.Fatal(err)
	}
	return a
}

func (r *rig) queueRoute(assetID string) {
	r.t.Helper()
	a := r.asset(assetID)
	r.tx(func(q *db.Queries) error {
		return outputs.QueueRoute(context.Background(), q, a, db.PlaybackID{ID: "pid_" + assetID, Policy: "signed"}, a.CurrentOutputID.String)
	})
}

func (r *rig) sync() int {
	r.t.Helper()
	n, err := r.commit.SyncRoutes(context.Background(), 100)
	if err != nil {
		r.t.Fatal(err)
	}
	return n
}

func (r *rig) object(key string) (string, bool) {
	r.t.Helper()
	rc, err := r.bucket.Get(context.Background(), key)
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

func TestRoutesAreWrittenForTheWorkerWithTheirBackend(t *testing.T) {
	r := newRig(t)
	r.queueRoute("ast_r2")
	r.queueRoute("ast_fs")
	if n := r.sync(); n != 2 {
		t.Fatalf("confirmed %d rows, want 2", n)
	}
	got, ok := r.object("_routes/pid_ast_r2.json")
	want := `{"v":1,"app_id":"app_dashboard","asset_id":"ast_r2","output_id":"out_ast_r2","policy":"signed","backend":"r2","outputs":{"out_ast_r2":1}}`
	if !ok || got != want {
		t.Fatalf("route = %s, want %s", got, want)
	}
	got, _ = r.object("_routes/pid_ast_fs.json")
	if !strings.Contains(got, `"backend":"origin"`) {
		t.Fatalf("a filesystem asset's route = %s, want backend origin", got)
	}
	// A retired output stays reachable for sessions pinned to it; a writing
	// one never is.
	r.exec(`INSERT INTO outputs (id, project_id, asset_id, key_version, status, manifest, retired_at) VALUES ('out_old', 'prj_default', 'ast_r2', 1, 'retired', '{}', now())`)
	r.exec(`INSERT INTO outputs (id, project_id, asset_id, key_version) VALUES ('out_new', 'prj_default', 'ast_r2', 2)`)
	r.queueRoute("ast_r2")
	r.sync()
	got, _ = r.object("_routes/pid_ast_r2.json")
	var rec routes.Record
	_ = json.Unmarshal([]byte(got), &rec)
	if len(rec.Outputs) != 2 || rec.Outputs["out_old"] != 1 || rec.Outputs["out_ast_r2"] != 1 {
		t.Fatalf("outputs = %v, want the committed and the retired output only", rec.Outputs)
	}
}

func TestRevocationIsConfirmedOnlyAfterTheDeleteLands(t *testing.T) {
	r := newRig(t)
	r.queueRoute("ast_r2")
	r.sync()
	ctx := context.Background()
	// Point Worker delivery at a bucket that can't be reached.
	box, _ := secrets.NewBox(1, bytes.Repeat([]byte{6}, 32))
	sealed := box.Seal("storage_configs", "sto_dead", []byte(`{"access_key_id":"a","secret_access_key":"b"}`))
	r.exec(`INSERT INTO storage_configs (id, project_id, kind, config, secrets_encrypted) VALUES ('sto_dead', 'prj_default', 'r2', '{"endpoint":"http://127.0.0.1:1","bucket":"x"}', $1)`, sealed)
	r.exec(`UPDATE projects SET delivery_storage_id = 'sto_dead'`)
	revoke := func() {
		a := r.asset("ast_r2")
		r.exec(`UPDATE playback_ids SET status = 'revoking' WHERE id = 'pid_ast_r2'`)
		r.tx(func(q *db.Queries) error { return outputs.QueueUnpublish(ctx, q, a, "pid_ast_r2") })
	}
	revoke()
	if n := r.sync(); n != 0 {
		t.Fatalf("confirmed %d rows against an unreachable bucket", n)
	}
	var status, lastError string
	var attempts int
	_ = r.pool.QueryRow(ctx, `SELECT p.status FROM playback_ids p WHERE id = 'pid_ast_r2'`).Scan(&status)
	_ = r.pool.QueryRow(ctx, `SELECT attempts, last_error FROM outbox WHERE kind = 'route_sync' AND done_at IS NULL`).Scan(&attempts, &lastError)
	if status != "revoking" || attempts != 1 || lastError != "publish_failed" {
		t.Fatalf("after a failed delete: status %s, attempts %d, error %q; want revoking, 1, publish_failed", status, attempts, lastError)
	}
	// The bucket comes back: the retry deletes the route, then revokes.
	r.exec(`UPDATE projects SET delivery_storage_id = (SELECT id FROM storage_configs WHERE kind = 'r2' AND id <> 'sto_dead')`)
	r.exec(`UPDATE outbox SET available_at = now() WHERE done_at IS NULL`)
	if n := r.sync(); n != 1 {
		t.Fatalf("confirmed %d rows on retry, want 1", n)
	}
	if _, ok := r.object("_routes/pid_ast_r2.json"); ok {
		t.Fatal("the revoked route is still in the bucket")
	}
	_ = r.pool.QueryRow(ctx, `SELECT status FROM playback_ids WHERE id = 'pid_ast_r2'`).Scan(&status)
	if status != "revoked" {
		t.Fatalf("status = %s, want revoked", status)
	}
}

func TestDisablingAnAppUnpublishesItsRoutesWithoutEvents(t *testing.T) {
	r := newRig(t)
	r.queueRoute("ast_r2")
	r.sync()
	ctx := context.Background()
	var before int
	_ = r.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE kind = 'webhook'`).Scan(&before)
	r.exec(`INSERT INTO apps (id, project_id, name) VALUES ('app_lms', 'prj_default', 'LMS')`)
	r.exec(`UPDATE assets SET app_id = 'app_lms' WHERE id = 'ast_r2'`)
	r.exec(`UPDATE apps SET disabled_at = now() WHERE id = 'app_lms'`)
	r.tx(func(q *db.Queries) error { return routes.QueueAppRefresh(ctx, q, db.DefaultProjectID, "app_lms") })
	r.sync()
	if _, ok := r.object("_routes/pid_ast_r2.json"); ok {
		t.Fatal("a disabled app's route is still published")
	}
	r.exec(`UPDATE apps SET disabled_at = NULL WHERE id = 'app_lms'`)
	r.tx(func(q *db.Queries) error { return routes.QueueAppRefresh(ctx, q, db.DefaultProjectID, "app_lms") })
	r.sync()
	if _, ok := r.object("_routes/pid_ast_r2.json"); !ok {
		t.Fatal("re-enabling the app didn't republish its route")
	}
	var after int
	_ = r.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE kind = 'webhook'`).Scan(&after)
	if after != before {
		t.Fatalf("refreshing routes queued %d webhook events, want none", after-before)
	}
}

func TestKeysArePublishedAsJWKsAndRevocationRemovesThem(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	_, pubPEM, err := tokens.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	r.exec(`INSERT INTO signing_keys (id, project_id, app_id, public_key_pem) VALUES ('kid_app', 'prj_default', 'app_dashboard', $1)`, pubPEM)
	r.exec(`INSERT INTO signing_keys (id, project_id, app_id, public_key_pem, retired_at) VALUES ('kid_old', 'prj_default', 'app_dashboard', $1, now() - interval '1 second')`, pubPEM)
	r.tx(func(q *db.Queries) error { return routes.QueueKeys(ctx, q, db.DefaultProjectID) })
	r.sync()
	got, ok := r.object("_keys.json")
	var k routes.Keys
	if err := json.Unmarshal([]byte(got), &k); !ok || err != nil {
		t.Fatalf("_keys.json = %q, %v", got, err)
	}
	if k.MaxTokenLifetimeSeconds != 604800 || len(k.Keys) != 1 || k.Keys[0].Kid != "kid_app" || *k.Keys[0].AppID != "app_dashboard" {
		t.Fatalf("keys = %+v; want only the active app key, and the 7-day maximum", k)
	}
	if j := k.Keys[0].JWK; j.Kty != "EC" || j.Crv != "P-256" || len(j.X) != 43 || len(j.Y) != 43 {
		t.Fatalf("jwk = %+v", j)
	}
	r.exec(`UPDATE signing_keys SET revoked_at = now() WHERE id = 'kid_app'`)
	r.tx(func(q *db.Queries) error { return routes.QueueKeys(ctx, q, db.DefaultProjectID) })
	r.sync()
	got, _ = r.object("_keys.json")
	if strings.Contains(got, "kid_app") {
		t.Fatalf("a revoked key is still published: %s", got)
	}
}

func TestTheReconcilerRepairsDriftAndThenChangesNothing(t *testing.T) {
	r := newRig(t)
	r.queueRoute("ast_r2")
	r.queueRoute("ast_fs")
	r.sync()
	ctx := context.Background()
	// Drift: a stale record, a route for an ID that was never live, and a
	// missing one.
	_ = r.bucket.Put(ctx, "_routes/pid_ast_r2.json", strings.NewReader(`{"v":1,"stale":true}`))
	_ = r.bucket.Put(ctx, "_routes/pid_ghost.json", strings.NewReader(`{}`))
	_ = r.bucket.Delete(ctx, "_routes/pid_ast_fs.json")
	d, bucketID, err := routes.ForProject(ctx, db.New(r.pool), r.set, db.DefaultProjectID)
	if err != nil {
		t.Fatal(err)
	}
	res, err := routes.Reconcile(ctx, db.New(r.pool), d, bucketID, db.DefaultProjectID)
	if err != nil || res.Written != 2 || res.Deleted != 1 {
		t.Fatalf("Reconcile = %+v, %v; want 2 written, 1 deleted", res, err)
	}
	if got, _ := r.object("_routes/pid_ast_r2.json"); strings.Contains(got, "stale") {
		t.Fatal("the stale record survived")
	}
	if _, ok := r.object("_routes/pid_ghost.json"); ok {
		t.Fatal("the unknown ID's route survived")
	}
	if _, ok := r.object("_keys.json"); !ok {
		t.Fatal("the reconciler didn't write _keys.json")
	}
	res, err = routes.Reconcile(ctx, db.New(r.pool), d, bucketID, db.DefaultProjectID)
	if err != nil || res.Written != 0 || res.Deleted != 0 {
		t.Fatalf("second Reconcile = %+v, %v; want no changes", res, err)
	}
}
