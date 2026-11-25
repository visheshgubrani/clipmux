package fence_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/fence"
	"github.com/visheshgubrani/clipmux/internal/retention"
	"github.com/visheshgubrani/clipmux/internal/routes"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/stores"
)

// knowsOneVideo gives the install's database one ready asset (ast_old, on
// out_old) with one playback ID (pid_old).
func (i *install) knowsOneVideo() {
	i.t.Helper()
	i.exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_old', 'prj_default', 'app_dashboard', 'processing', 'sto_default')`)
	i.exec(`INSERT INTO outputs (id, project_id, asset_id, key_version, status, manifest) VALUES ('out_old', 'prj_default', 'ast_old', 1, 'committed', '{"files":[{"path":"master.m3u8"}]}')`)
	i.exec(`UPDATE assets SET current_output_id = 'out_old', status = 'ready' WHERE id = 'ast_old'`)
	i.exec(`INSERT INTO playback_ids (id, project_id, asset_id, policy) VALUES ('pid_old', 'prj_default', 'ast_old', 'public')`)
}

// playing is the output the published record of pid plays.
func playing(t *testing.T, st storage.Storage, pid string) string {
	t.Helper()
	rc, err := st.Get(context.Background(), routes.RouteKey(pid))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	var r routes.Record
	if err := json.NewDecoder(rc).Decode(&r); err != nil {
		t.Fatal(err)
	}
	return r.OutputID
}

// The fence row is raised by the epoch loop, but a reconcile may run before
// it: a boot that didn't win leadership in time, or a store the loop couldn't
// read. The reconcile reads the bucket's marker itself.
func TestAReconcileBeforeAnyEpochPassDeletesNothingFromAnotherInstallsBucket(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	used := newInstall(t, st)
	used.pass()
	put(t, st, "_routes/pid_theirs.json", `{"v":1}`)

	fresh := newInstall(t, st) // no epoch pass yet
	res, err := routes.Reconcile(ctx, db.New(fresh.pool), routes.Worker{Store: st}, "sto_default", db.DefaultProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if !exists(t, st, "_routes/pid_theirs.json") || res.Deleted != 0 || res.Kept != 1 {
		t.Fatalf("reconcile = %+v; it deleted another install's route record", res)
	}
	if f := fresh.activeFence(); f.Reason != "foreign_install" {
		t.Fatalf("fence = %s %q, want foreign_install raised by the reconcile", f.Reason, f.Detail)
	}
}

func TestASweepBeforeAnyEpochPassDeletesNothingFromAStoreAheadOfTheDatabase(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	a := newInstall(t, st)
	a.knowsOneVideo()
	a.pass()
	a.pass()
	a.pass()
	rewind(a, 1) // a restore, with serve not yet through an epoch pass
	put(t, st, "media/ast_old/out_reencoded/master.m3u8", "x")

	files := retention.NewFiles(a.pool, stores.New(a.pool, nil, st), st)
	if _, err := files.SweepOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	if !exists(t, st, "media/ast_old/out_reencoded/master.m3u8") {
		t.Fatal("the orphan sweep deleted a re-encode newer than the database")
	}
	if f := a.activeFence(); f.Reason != "database_behind" {
		t.Fatalf("fence = %s %q, want database_behind raised by the sweep", f.Reason, f.Detail)
	}
}

// A marker that can't be read stops the delete: it fails closed.
func TestAStoreWhoseMarkerCantBeReadIsNotDeletedFrom(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	a := newInstall(t, st)
	put(t, st, fence.MarkerKey, "not json")
	put(t, st, "_routes/pid_unknown.json", `{"v":1}`)

	if _, err := routes.Reconcile(ctx, db.New(a.pool), routes.Worker{Store: st}, "sto_default", db.DefaultProjectID); err == nil {
		t.Fatal("a reconcile went ahead on a store whose marker can't be read")
	}
	if !exists(t, st, "_routes/pid_unknown.json") {
		t.Fatal("a store whose marker can't be read was deleted from")
	}
}

// Connect Cloudflare adds a bucket and reconciles it at once. A bucket with no
// marker but with route records this database doesn't know was published into
// by someone else, or by this install before markers existed.
func TestABucketWithUnknownRoutesAndNoMarkerIsFencedBeforeItsFirstReconcile(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	a := newInstall(t, st)
	a.knowsOneVideo()
	put(t, st, "_routes/pid_old.json", `{"v":1}`)
	put(t, st, "_routes/pid_theirs.json", `{"v":1}`)

	q := db.New(a.pool)
	g, err := fence.NewGuard(ctx, q, db.DefaultProjectID, nil)
	if err != nil {
		t.Fatal(err)
	}
	fenced, err := g.CheckBucket(ctx, "sto_default", st)
	if err != nil || !fenced {
		t.Fatalf("CheckBucket = %v, %v; want fenced", fenced, err)
	}
	if f := a.activeFence(); f.Reason != "foreign_install" || !strings.Contains(f.Detail, "1 route records") {
		t.Fatalf("fence = %s %q", f.Reason, f.Detail)
	}
	if _, err := routes.Reconcile(ctx, q, routes.Worker{Store: st}, "sto_default", db.DefaultProjectID); err != nil {
		t.Fatal(err)
	}
	if !exists(t, st, "_routes/pid_theirs.json") {
		t.Fatal("the first reconcile deleted a route record this database doesn't know")
	}

	// A bucket holding only what this database published is its own.
	own := newStore(t)
	b := newInstall(t, own)
	b.knowsOneVideo()
	put(t, own, "_routes/pid_old.json", `{"v":1}`)
	g, err = fence.NewGuard(ctx, db.New(b.pool), db.DefaultProjectID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fenced, err := g.CheckBucket(ctx, "sto_default", own); err != nil || fenced {
		t.Fatalf("CheckBucket on the install's own bucket = %v, %v; want not fenced", fenced, err)
	}
}

// A record the database knows, but that plays a re-encode made after the
// backup, is kept while fenced: pointing it back at the backup's output could
// point it at files purged since. Once the owner finishes, the database
// decides again.
func TestWhileFencedARecordPlayingANewerReencodeIsKept(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	a := newInstall(t, st)
	a.knowsOneVideo()
	a.pass()
	a.pass()
	rewind(a, 1)
	put(t, st, "_routes/pid_old.json", `{"v":1,"app_id":"app_dashboard","asset_id":"ast_old","output_id":"out_reencoded","policy":"public","backend":"r2","outputs":{"out_reencoded":1}}`)
	a.pass()
	if !a.fenced() {
		t.Fatal("not fenced")
	}

	q := db.New(a.pool)
	res, err := routes.Reconcile(ctx, q, routes.Worker{Store: st}, "sto_default", db.DefaultProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if got := playing(t, st, "pid_old"); got != "out_reencoded" || res.Kept != 1 {
		t.Fatalf("after a fenced reconcile pid_old plays %s (%+v), want out_reencoded kept", got, res)
	}
	if err := routes.Apply(ctx, q, routes.Worker{Store: st}, "sto_default", "pid_old"); err != nil {
		t.Fatal(err)
	}
	if got := playing(t, st, "pid_old"); got != "out_reencoded" {
		t.Fatalf("after a fenced publication pid_old plays %s, want out_reencoded kept", got)
	}

	if lifted, err := a.epoch.Finish(ctx); err != nil || !lifted {
		t.Fatalf("finish = %v, %v", lifted, err)
	}
	if _, err := routes.Reconcile(ctx, q, routes.Worker{Store: st}, "sto_default", db.DefaultProjectID); err != nil {
		t.Fatal(err)
	}
	if got := playing(t, st, "pid_old"); got != "out_old" {
		t.Fatalf("after finish pid_old plays %s, want the database's out_old", got)
	}
}

func TestCheckForeignBucketIdentifiesAnotherInstallWithoutRaisingFence(t *testing.T) {
	ctx := context.Background()

	// Bucket stamped by another install -> refused, no fence raised.
	stForeign := newStore(t)
	other := newInstall(t, stForeign)
	other.pass()
	fresh := newInstall(t, stForeign)
	err := fence.CheckForeignBucket(ctx, db.New(fresh.pool), db.DefaultProjectID, stForeign)
	if err == nil || !strings.Contains(err.Error(), other.row().InstallID) {
		t.Fatalf("foreign marker: err = %v, want other install ID %q", err, other.row().InstallID)
	}
	if fresh.fenced() {
		t.Fatal("CheckForeignBucket must not raise restore_fence")
	}

	// Markerless bucket holding unknown _routes/ records -> refused, no fence raised.
	stUnmarked := newStore(t)
	put(t, stUnmarked, "_routes/pid_theirs.json", `{"v":1}`)
	a := newInstall(t, stUnmarked)
	err = fence.CheckForeignBucket(ctx, db.New(a.pool), db.DefaultProjectID, stUnmarked)
	if err == nil || !strings.Contains(err.Error(), "1 route records") {
		t.Fatalf("unknown routes without marker: err = %v, want route count", err)
	}
	if a.fenced() {
		t.Fatal("CheckForeignBucket must not raise restore_fence")
	}

	// Bucket stamped by this install (even ahead of this database after a restore) -> allowed.
	stOwn := newStore(t)
	b := newInstall(t, stOwn)
	b.knowsOneVideo()
	b.pass()
	b.pass()
	rewind(b, 1)
	if err := fence.CheckForeignBucket(ctx, db.New(b.pool), db.DefaultProjectID, stOwn); err != nil {
		t.Fatalf("same-install ahead bucket = %v, want nil", err)
	}

	// Markerless bucket with only known routes -> allowed.
	stKnown := newStore(t)
	c := newInstall(t, stKnown)
	c.knowsOneVideo()
	put(t, stKnown, "_routes/pid_old.json", `{"v":1}`)
	if err := fence.CheckForeignBucket(ctx, db.New(c.pool), db.DefaultProjectID, stKnown); err != nil {
		t.Fatalf("known-only unmarked bucket = %v, want nil", err)
	}
}
