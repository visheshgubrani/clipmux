package fence_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/fence"
	"github.com/visheshgubrani/clipmux/internal/retention"
	"github.com/visheshgubrani/clipmux/internal/routes"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/stores"
	"github.com/visheshgubrani/clipmux/internal/tokens"
)

// install is one ClipMux database over a store, which two installs can share.
type install struct {
	t     *testing.T
	pool  *pgxpool.Pool
	store *storage.Filesystem
	epoch *fence.Epoch
}

func newStore(t *testing.T) *storage.Filesystem {
	t.Helper()
	fs, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: "http://unused"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fs.Close() })
	return fs
}

func newInstall(t *testing.T, store *storage.Filesystem) *install {
	t.Helper()
	pool := dbtest.New(t)
	return &install{t: t, pool: pool, store: store, epoch: fence.New(pool, stores.New(pool, nil, store))}
}

func (i *install) exec(sql string, args ...any) {
	i.t.Helper()
	if _, err := i.pool.Exec(context.Background(), sql, args...); err != nil {
		i.t.Fatalf("%s: %v", sql, err)
	}
}

func (i *install) pass() int {
	i.t.Helper()
	n, err := i.epoch.Pass(context.Background())
	if err != nil {
		i.t.Fatalf("epoch pass: %v", err)
	}
	return n
}

func (i *install) row() db.InstallEpoch {
	i.t.Helper()
	e, err := db.New(i.pool).EnsureInstallEpoch(context.Background(), db.DefaultProjectID)
	if err != nil {
		i.t.Fatal(err)
	}
	return e
}

func (i *install) fenced() bool {
	i.t.Helper()
	f, err := db.New(i.pool).FenceActive(context.Background(), db.DefaultProjectID)
	if err != nil {
		i.t.Fatal(err)
	}
	return f
}

func (i *install) activeFence() db.RestoreFence {
	i.t.Helper()
	f, err := db.New(i.pool).GetActiveFence(context.Background(), db.DefaultProjectID)
	if err != nil {
		i.t.Fatalf("no active fence: %v", err)
	}
	return f
}

// marker is what the store says about its owner.
func marker(t *testing.T, st storage.Storage) (installID string, generation int64) {
	t.Helper()
	rc, err := st.Get(context.Background(), fence.MarkerKey)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	var m struct {
		InstallID  string `json:"install_id"`
		Generation int64  `json:"generation"`
	}
	b, _ := io.ReadAll(rc)
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("marker %q: %v", b, err)
	}
	return m.InstallID, m.Generation
}

func put(t *testing.T, st storage.Storage, key, body string) {
	t.Helper()
	if err := st.Put(context.Background(), key, strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
}

func exists(t *testing.T, st storage.Storage, key string) bool {
	t.Helper()
	_, err := st.Head(context.Background(), key)
	if errors.Is(err, storage.ErrNotFound) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

func TestEachPassStampsTheStoreWithAGenerationTheDatabaseHoldsFirst(t *testing.T) {
	a := newInstall(t, newStore(t))
	id := a.row().InstallID
	if !strings.HasPrefix(id, "ins_") {
		t.Fatalf("install ID %q", id)
	}

	if n := a.pass(); n != 1 {
		t.Fatalf("first pass stamped %d stores, want 1", n)
	}
	if got, gen := marker(t, a.store); got != id || gen != 1 {
		t.Fatalf("marker = %s/%d, want %s/1", got, gen, id)
	}
	a.pass()
	if _, gen := marker(t, a.store); gen != 2 {
		t.Fatalf("second pass: marker generation %d, want 2", gen)
	}
	if e := a.row(); e.Generation != 2 || e.InstallID != id {
		t.Fatalf("database says %s/%d, want %s/2", e.InstallID, e.Generation, id)
	}
	if a.fenced() {
		t.Fatal("an install that wrote every marker itself is fenced")
	}
}

// What a restore of an older dump does: the install ID comes back, and the
// generation goes back to where it was.
func rewind(a *install, to int64) {
	a.exec(`UPDATE install_epoch SET generation = $1`, to)
}

func TestAnOlderDatabaseNextToANewerStoreDeletesNothing(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	a := newInstall(t, st)
	// The database knows one asset, with one output and one playback ID.
	a.exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_old', 'prj_default', 'app_dashboard', 'processing', 'sto_default')`)
	a.exec(`INSERT INTO outputs (id, project_id, asset_id, key_version, status, manifest) VALUES ('out_old', 'prj_default', 'ast_old', 1, 'committed', '{"files":[{"path":"master.m3u8"}]}')`)
	a.exec(`UPDATE assets SET current_output_id = 'out_old', status = 'ready' WHERE id = 'ast_old'`)
	a.exec(`INSERT INTO playback_ids (id, project_id, asset_id, policy) VALUES ('pid_old', 'prj_default', 'ast_old', 'public')`)
	a.pass()
	a.pass()
	a.pass() // the store is at generation 3; the backup was taken at 1
	rewind(a, 1)

	// Everything uploaded after the backup, which only storage has.
	put(t, st, "_routes/pid_new.json", `{"v":1,"app_id":"app_dashboard","asset_id":"ast_new","output_id":"out_new","policy":"public","backend":"r2","outputs":{"out_new":1}}`)
	put(t, st, "media/ast_new/out_new/master.m3u8", "x")
	put(t, st, "media/ast_new/out_new/manifest.json", "{}")
	put(t, st, "media/ast_old/out_reencoded/master.m3u8", "x")
	put(t, st, "uploads/ast_new/source", "source bytes")
	put(t, st, routes.KeysKey, `{"v":1,"max_token_lifetime_seconds":604800,"keys":[{"kid":"kid_newapp","app_id":"app_new","jwk":{"kty":"EC","crv":"P-256","x":"x","y":"y"},"not_after":null}]}`)

	if n := a.pass(); n != 0 {
		t.Fatalf("a pass over a store that is ahead stamped %d stores", n)
	}
	if !a.fenced() {
		t.Fatal("a database older than its store isn't fenced")
	}
	if f := a.activeFence(); f.Reason != "database_behind" || !strings.Contains(f.Detail, "generation 3") {
		t.Fatalf("fence = %s %q", f.Reason, f.Detail)
	}
	if _, gen := marker(t, st); gen != 3 {
		t.Fatalf("a fenced pass rewrote the marker: generation %d, want 3", gen)
	}
	if e := a.row(); e.Generation != 1 {
		t.Fatalf("a fenced pass moved the database's generation to %d", e.Generation)
	}

	// Every deleting loop runs, as serve would run them.
	files := retention.NewFiles(a.pool, stores.New(a.pool, nil, st), st)
	if _, err := files.Run(ctx); err != nil {
		t.Fatal(err)
	}
	q := db.New(a.pool)
	if _, err := routes.Reconcile(ctx, q, routes.Worker{Store: st}, "sto_default", db.DefaultProjectID); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"_routes/pid_new.json", "media/ast_new/out_new/master.m3u8", "media/ast_new/out_new/manifest.json",
		"media/ast_old/out_reencoded/master.m3u8", "uploads/ast_new/source",
	} {
		if !exists(t, st, key) {
			t.Errorf("%s was deleted while fenced", key)
		}
	}
	// What the database does know still reaches the edge.
	if !exists(t, st, "_routes/pid_old.json") {
		t.Error("a route the database knows wasn't written while fenced")
	}
	// A key the database never heard of survives the rewrite of _keys.json.
	rc, err := st.Get(ctx, routes.KeysKey)
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := io.ReadAll(rc)
	rc.Close()
	if !strings.Contains(string(keys), "kid_newapp") {
		t.Errorf("_keys.json lost the newer app's key: %s", keys)
	}

	// The report says what is newer.
	rep, err := a.epoch.Report(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Routes) != 1 || rep.Routes[0].PlaybackID != "pid_new" || rep.Routes[0].AssetID != "ast_new" {
		t.Errorf("routes = %+v", rep.Routes)
	}
	outs := map[string]fence.OutputFinding{}
	for _, o := range rep.Outputs {
		outs[o.AssetID+"/"+o.OutputID] = o
	}
	if len(outs) != 2 || !outs["ast_new/out_new"].HasManifest || outs["ast_new/out_new"].AssetKnown ||
		outs["ast_old/out_reencoded"].HasManifest || !outs["ast_old/out_reencoded"].AssetKnown {
		t.Errorf("outputs = %+v", rep.Outputs)
	}
	if len(rep.Sources) != 1 || rep.Sources[0].AssetID != "ast_new" || rep.Sources[0].Bytes != int64(len("source bytes")) {
		t.Errorf("sources = %+v", rep.Sources)
	}

	// Finishing is the owner's decision: it adopts the store's generation,
	// stamps every store, and only then lets deletes run again.
	lifted, err := a.epoch.Finish(ctx)
	if err != nil || !lifted {
		t.Fatalf("finish = %v, %v", lifted, err)
	}
	if a.fenced() {
		t.Fatal("finish left the fence up")
	}
	if got, gen := marker(t, st); got != a.row().InstallID || gen <= 3 || gen != a.row().Generation {
		t.Fatalf("after finish: marker %s/%d, database %s/%d", got, gen, a.row().InstallID, a.row().Generation)
	}
	a.pass()
	if a.fenced() {
		t.Fatal("the next pass fenced again after finish")
	}
}

func TestAFreshDatabaseOnAUsedStoreIsFenced(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	used := newInstall(t, st)
	used.pass()
	used.pass()
	put(t, st, "_routes/pid_theirs.json", `{"v":1}`)

	fresh := newInstall(t, st) // a new database: its own install ID
	if fresh.row().InstallID == used.row().InstallID {
		t.Fatal("two databases got the same install ID")
	}
	if n := fresh.pass(); n != 0 {
		t.Fatalf("a fresh install stamped %d stores that another install owns", n)
	}
	if !fresh.fenced() {
		t.Fatal("a fresh install on a used store isn't fenced")
	}
	if f := fresh.activeFence(); f.Reason != "foreign_install" || !strings.Contains(f.Detail, used.row().InstallID) {
		t.Fatalf("fence = %s %q", f.Reason, f.Detail)
	}
	if got, _ := marker(t, st); got != used.row().InstallID {
		t.Fatalf("the fresh install overwrote the marker with %s", got)
	}

	// Its first reconcile would have wiped _routes/.
	if _, err := routes.Reconcile(ctx, db.New(fresh.pool), routes.Worker{Store: st}, "sto_default", db.DefaultProjectID); err != nil {
		t.Fatal(err)
	}
	if !exists(t, st, "_routes/pid_theirs.json") {
		t.Fatal("a fenced install deleted another install's route record")
	}

	// The owner decides the bucket is theirs now.
	if lifted, err := fresh.epoch.Finish(ctx); err != nil || !lifted {
		t.Fatalf("finish = %v, %v", lifted, err)
	}
	if got, gen := marker(t, st); got != fresh.row().InstallID || gen <= 2 {
		t.Fatalf("after finish: marker %s/%d", got, gen)
	}
	fresh.pass()
	if fresh.fenced() {
		t.Fatal("fenced again after finish")
	}
}

func TestAFenceRaisedByARestoreCommandStaysUntilFinished(t *testing.T) {
	ctx := context.Background()
	a := newInstall(t, newStore(t))
	a.pass()
	// `clipmux admin restore` raises it directly, with the backup it used.
	raised, err := fence.Engage(ctx, db.New(a.pool), db.DefaultProjectID, fence.Found{Reason: fence.Restored, Detail: "restored bak_1", BackupID: "bak_1"})
	if err != nil || !raised {
		t.Fatalf("engage = %v, %v", raised, err)
	}
	if again, err := fence.Engage(ctx, db.New(a.pool), db.DefaultProjectID, fence.Found{Reason: fence.Restored}); err != nil || again {
		t.Fatalf("a second engage = %v, %v; one fence is active at a time", again, err)
	}
	// Nothing is wrong with the markers, but the passes write nothing.
	before := a.row().Generation
	if n := a.pass(); n != 0 || a.row().Generation != before {
		t.Fatalf("a pass under a fence stamped %d stores and moved the generation to %d", n, a.row().Generation)
	}
	if lifted, err := a.epoch.Finish(ctx); err != nil || !lifted {
		t.Fatalf("finish = %v, %v", lifted, err)
	}
	if lifted, err := a.epoch.Finish(ctx); err != nil || lifted {
		t.Fatalf("finishing with no fence = %v, %v", lifted, err)
	}
}

func TestWhileFencedTheKeyFileKeepsUnknownKeysAndNeverARevokedOne(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	a := newInstall(t, st)
	_, pub, err := tokens.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	// The database knows a live key and one revoked since the backup.
	a.exec(`INSERT INTO signing_keys (id, project_id, app_id, public_key_pem) VALUES ('kid_live', 'prj_default', 'app_dashboard', $1)`, pub)
	a.exec(`INSERT INTO signing_keys (id, project_id, app_id, public_key_pem, revoked_at) VALUES ('kid_revoked', 'prj_default', 'app_dashboard', $1, now())`, pub)
	// The edge has both, and a key of an app created after the backup.
	put(t, st, routes.KeysKey, `{"v":1,"max_token_lifetime_seconds":1000000,"keys":[`+
		`{"kid":"kid_live","app_id":"app_dashboard","jwk":{"kty":"EC","crv":"P-256","x":"x","y":"y"},"not_after":null},`+
		`{"kid":"kid_revoked","app_id":"app_dashboard","jwk":{"kty":"EC","crv":"P-256","x":"x","y":"y"},"not_after":null},`+
		`{"kid":"kid_newapp","app_id":"app_new","jwk":{"kty":"EC","crv":"P-256","x":"x","y":"y"},"not_after":null}]}`)
	q := db.New(a.pool)
	if _, err := fence.Engage(ctx, q, db.DefaultProjectID, fence.Found{Reason: fence.Restored}); err != nil {
		t.Fatal(err)
	}

	if err := routes.PublishKeys(ctx, q, routes.Worker{Store: st}, db.DefaultProjectID); err != nil {
		t.Fatal(err)
	}
	rc, err := st.Get(ctx, routes.KeysKey)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	var k routes.Keys
	if err := json.NewDecoder(rc).Decode(&k); err != nil {
		t.Fatal(err)
	}
	var kids []string
	for _, key := range k.Keys {
		kids = append(kids, key.Kid)
	}
	if got := strings.Join(kids, ","); got != "kid_live,kid_newapp" {
		t.Fatalf("keys = %s, want kid_live,kid_newapp (a revoked key must not come back)", got)
	}
	if k.MaxTokenLifetimeSeconds != 1000000 {
		t.Fatalf("max token lifetime = %d, want the larger of the two", k.MaxTokenLifetimeSeconds)
	}

	// Without the fence the file is the database's alone.
	if _, err := q.LiftFence(ctx, db.DefaultProjectID); err != nil {
		t.Fatal(err)
	}
	if err := routes.PublishKeys(ctx, q, routes.Worker{Store: st}, db.DefaultProjectID); err != nil {
		t.Fatal(err)
	}
	rc2, _ := st.Get(ctx, routes.KeysKey)
	defer rc2.Close()
	b, _ := io.ReadAll(rc2)
	if strings.Contains(string(b), "kid_newapp") {
		t.Fatalf("an unfenced install kept a key it doesn't know: %s", b)
	}
}
