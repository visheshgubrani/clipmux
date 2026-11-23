package backup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/stores"
)

// sourceEnv is a service with a filesystem store for sources and a target.
type sourceEnv struct {
	*Service
	clk   *clock
	store *storage.Filesystem
	exec  func(sql string, args ...any)
}

func newSourceEnv(t *testing.T) sourceEnv {
	t.Helper()
	pool := dbtest.New(t)
	s, clk := newService(t, pool, Config{URL: "file://" + t.TempDir()}, time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC))
	fs, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: "http://localhost"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fs.Close() })
	s.Stores = stores.Fixed(fs)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	return sourceEnv{s, clk, fs, exec}
}

// asset makes an asset whose source holds body, created n minutes after the
// first, and returns its ID.
func (e sourceEnv) asset(t *testing.T, n int, status, body string) string {
	t.Helper()
	id := fmt.Sprintf("ast_%02d", n)
	e.exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id, source_uri, created_at)
		VALUES ($1, 'prj_default', 'app_dashboard', $2, 'sto_default', 'uploads/'||$1||'/source', '2026-10-01'::timestamptz + $3 * interval '1 minute')`, id, status, n)
	if body != "" {
		if err := e.store.Put(context.Background(), "uploads/"+id+"/source", strings.NewReader(body)); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func (e sourceEnv) backedUp(t *testing.T) []string {
	t.Helper()
	rows, err := e.Pool.Query(context.Background(), `SELECT id FROM assets WHERE source_backed_up_at IS NOT NULL ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

func (e sourceEnv) has(t *testing.T, key string) bool {
	t.Helper()
	rc, err := e.Target.Get(context.Background(), key)
	if err != nil {
		return false
	}
	rc.Close()
	return true
}

func TestTheSourceMirrorCopiesTwentyOldestFirstEncryptedAndSkipsWhatItShouldnt(t *testing.T) {
	e := newSourceEnv(t)
	ctx := context.Background()
	for n := 1; n <= 22; n++ {
		e.asset(t, n, "processing", fmt.Sprintf("source number %d", n))
	}
	e.asset(t, 30, "waiting_upload", "not committed yet")
	notKept := e.asset(t, 31, "processing", "dropped after encoding")
	e.exec(`UPDATE assets SET source_kept = false WHERE id = $1`, notKept)

	n, err := e.RunSources(ctx)
	if err != nil || n != 20 {
		t.Fatalf("first pass = %d, %v, want 20 copied", n, err)
	}
	if got := e.backedUp(t); len(got) != 20 || got[0] != "ast_01" || got[19] != "ast_20" {
		t.Fatalf("copied %v, want ast_01 to ast_20", got)
	}
	n, err = e.RunSources(ctx)
	if err != nil || n != 2 {
		t.Fatalf("second pass = %d, %v, want the last 2", n, err)
	}
	if n, err = e.RunSources(ctx); err != nil || n != 0 {
		t.Fatalf("third pass = %d, %v, want nothing left", n, err)
	}
	if e.has(t, "sources/ast_30.age") || e.has(t, "sources/ast_31.age") {
		t.Error("an uncommitted source, or one not kept, was copied")
	}

	// What is stored is age ciphertext that the install's own key opens.
	raw := read(t, e.Target, "sources/ast_07.age")
	if bytes.Contains(raw, []byte("source number 7")) {
		t.Fatal("the source is on the target in the clear")
	}
	ids, err := identities(e.Box)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := age.Decrypt(bytes.NewReader(raw), ids...)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(plain); string(got) != "source number 7" {
		t.Errorf("decrypted source = %q", got)
	}
}

func TestASourceMissingFromItsStoreIsSkippedWithoutHoldingUpTheOthers(t *testing.T) {
	e := newSourceEnv(t)
	ctx := context.Background()
	missing := e.asset(t, 1, "processing", "")
	ok := e.asset(t, 2, "processing", "fine")
	n, err := e.RunSources(ctx)
	if err != nil || n != 1 {
		t.Fatalf("pass = %d, %v, want the one that exists copied and no error", n, err)
	}
	if got := e.backedUp(t); !slices.Equal(got, []string{ok}) {
		t.Errorf("copied %v, want only %s", got, ok)
	}
	// It is left alone for an hour, then tried again.
	e.clk.t = e.clk.t.Add(SourceRetry - time.Minute)
	if _, ok := e.skippedSourcesSet()[missing]; !ok {
		t.Errorf("%s is not skipped within the hour", missing)
	}
	e.clk.t = e.clk.t.Add(2 * time.Minute)
	if _, ok := e.skippedSourcesSet()[missing]; ok {
		t.Errorf("%s is still skipped after the hour", missing)
	}
	// Once its source is back, it is copied.
	if err := e.store.Put(ctx, "uploads/"+missing+"/source", strings.NewReader("back")); err != nil {
		t.Fatal(err)
	}
	if n, err := e.RunSources(ctx); err != nil || n != 1 {
		t.Fatalf("pass = %d, %v, want it copied now", n, err)
	}
}

func (e sourceEnv) skippedSourcesSet() map[string]struct{} {
	m := map[string]struct{}{}
	for _, id := range e.skippedSources() {
		m[id] = struct{}{}
	}
	return m
}

func TestADeletedAssetThatWasCopiedGetsOneTombstoneAndIsPrunedAfterTheGrace(t *testing.T) {
	e := newSourceEnv(t)
	ctx := context.Background()
	kept := e.asset(t, 1, "processing", "keep me")
	gone := e.asset(t, 2, "processing", "delete me")
	never := e.asset(t, 3, "processing", "deleted before it was copied")
	e.exec(`UPDATE assets SET status = 'deleted', deleted_at = now() WHERE id = $1`, never)
	if n, err := e.RunSources(ctx); err != nil || n != 2 {
		t.Fatalf("pass = %d, %v, want the two live sources copied", n, err)
	}
	e.exec(`UPDATE assets SET status = 'deleted', deleted_at = now() WHERE id = $1`, gone)
	if n, err := e.RunSources(ctx); err != nil || n != 1 {
		t.Fatalf("pass = %d, %v, want one tombstone", n, err)
	}
	if !e.has(t, "sources/"+gone+".deleted") || e.has(t, "sources/"+kept+".deleted") || e.has(t, "sources/"+never+".deleted") || e.has(t, "sources/"+never+".age") {
		t.Fatal("tombstones are not exactly the deleted asset that had been copied")
	}
	if n, err := e.RunSources(ctx); err != nil || n != 0 {
		t.Fatalf("pass = %d, %v, want the tombstone written once", n, err)
	}

	// Inside the grace nothing goes; after it the copy and the tombstone do,
	// and a live asset's copy stays.
	e.clk.t = e.clk.t.Add(TombstoneGrace - time.Hour)
	if n, err := e.PruneSources(ctx, TombstoneGrace); err != nil || n != 0 || !e.has(t, "sources/"+gone+".age") {
		t.Fatalf("early prune = %d, %v", n, err)
	}
	e.clk.t = e.clk.t.Add(2 * time.Hour)
	if n, err := e.PruneSources(ctx, TombstoneGrace); err != nil || n != 1 {
		t.Fatalf("prune = %d, %v, want 1", n, err)
	}
	if e.has(t, "sources/"+gone+".age") || e.has(t, "sources/"+gone+".deleted") || !e.has(t, "sources/"+kept+".age") {
		t.Error("prune left the deleted asset's objects or took a live one")
	}
}

func TestSourcesAreNotPrunedWhereTheKeyCantDeleteAndTheLoopPrunesWhereItCan(t *testing.T) {
	e := newSourceEnv(t)
	ctx := context.Background()
	id := e.asset(t, 1, "processing", "x")
	if _, err := e.RunSources(ctx); err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE assets SET status = 'deleted', deleted_at = now() WHERE id = $1`, id)
	if _, err := e.RunSources(ctx); err != nil {
		t.Fatal(err)
	}
	e.clk.t = e.clk.t.Add(TombstoneGrace + 25*time.Hour)
	// The loop's daily prune takes it.
	if _, err := e.RunSources(ctx); err != nil {
		t.Fatal(err)
	}
	if e.has(t, "sources/"+id+".age") {
		t.Error("the loop didn't prune a long-deleted asset's copy on a target that may delete")
	}

	f := newSourceEnv(t)
	f.Target.store = noDelete{f.Target.store}
	id = f.asset(t, 1, "processing", "x")
	f.RunSources(ctx)
	f.exec(`UPDATE assets SET status = 'deleted', deleted_at = now() WHERE id = $1`, id)
	f.RunSources(ctx)
	f.clk.t = f.clk.t.Add(TombstoneGrace + 25*time.Hour)
	if _, err := f.RunSources(ctx); err != nil {
		t.Fatalf("the loop errored on a target that can't delete: %v", err)
	}
	if !f.has(t, "sources/"+id+".age") {
		t.Error("a copy was pruned from a target whose key can't delete")
	}
	if _, err := f.PruneSources(ctx, 0); err != ErrNotPrunable {
		t.Errorf("PruneSources on an immutable target = %v, want ErrNotPrunable", err)
	}
}

func TestRestoreSourcesBringsBackWhatAStoreLostAndQueuesReencodes(t *testing.T) {
	e := newSourceEnv(t)
	ctx := context.Background()
	lostBoth := e.asset(t, 1, "processing", "lost source and outputs")
	lostOutputs := e.asset(t, 2, "processing", "source survived, outputs lost")
	fine := e.asset(t, 3, "processing", "nothing lost")
	deleted := e.asset(t, 4, "processing", "deleted after the backup")
	unmirrored := e.asset(t, 5, "processing", "")
	for _, id := range []string{lostBoth, lostOutputs, fine} {
		e.exec(`INSERT INTO outputs (id, project_id, asset_id, key_version, status, manifest) VALUES ('out_'||$1::text, 'prj_default', $1, 1, 'committed', '{}')`, id)
		e.exec(`UPDATE assets SET current_output_id = 'out_'||$1::text, status = 'ready' WHERE id = $1`, id)
	}
	for _, id := range []string{lostBoth, fine} {
		if err := e.store.Put(ctx, "media/"+id+"/out_"+id+"/master.m3u8", strings.NewReader("#EXTM3U")); err != nil {
			t.Fatal(err)
		}
	}
	// lostOutputs has its source but no files; lostBoth loses its source; the
	// copies are made before the loss.
	if n, err := e.RunSources(ctx); err != nil || n != 4 {
		t.Fatalf("mirror = %d, %v, want 4", n, err)
	}
	e.exec(`UPDATE assets SET status = 'deleted', deleted_at = now() WHERE id = $1`, deleted)
	if _, err := e.RunSources(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Delete(ctx, "uploads/"+lostBoth+"/source"); err != nil {
		t.Fatal(err)
	}
	if err := e.store.DeletePrefix(ctx, "media/"+lostBoth+"/"); err != nil {
		t.Fatal(err)
	}
	// ...and the database was restored from before the delete.
	e.exec(`UPDATE assets SET status = 'processing', deleted_at = NULL WHERE id = $1`, deleted)

	var queued []string
	opt := RestoreSourcesOptions{Reencode: func(_ context.Context, project, app, id string) (bool, error) {
		if project != "prj_default" || app != "app_dashboard" {
			t.Errorf("re-encode of %s for %s/%s", id, project, app)
		}
		queued = append(queued, id)
		return true, nil
	}}
	res, err := e.RestoreSources(ctx, opt)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(queued)
	if want := []string{lostBoth, lostOutputs}; !slices.Equal(queued, want) || !slices.Equal(res.Queued, queued) {
		t.Errorf("queued %v (reported %v), want %v", queued, res.Queued, want)
	}
	if !slices.Equal(res.Restored, []string{lostBoth}) || !slices.Equal(res.Present, []string{fine}) ||
		!slices.Equal(res.Tombstoned, []string{deleted}) || !slices.Equal(res.Missing, []string{unmirrored}) || len(res.Problems) != 0 {
		t.Errorf("result = %+v", res)
	}
	rc, err := e.store.Get(ctx, "uploads/"+lostBoth+"/source")
	if err != nil {
		t.Fatalf("the source wasn't restored: %v", err)
	}
	if got, _ := io.ReadAll(rc); string(got) != "lost source and outputs" {
		t.Errorf("restored source = %q", got)
	}
	rc.Close()

	// A second run restores nothing it already did, and the store has no
	// leftover temporary files.
	queued = nil
	res, err = e.RestoreSources(ctx, opt)
	if err != nil || len(res.Restored) != 0 || !slices.Equal(queued, []string{lostBoth, lostOutputs}) {
		// The re-encodes haven't run, so their outputs are still gone: they
		// are queued again, and the job path answers job_active for them.
		t.Errorf("second run = %+v, %v, queued %v", res, err, queued)
	}

	one, err := e.RestoreSources(ctx, RestoreSourcesOptions{AssetID: fine})
	if err != nil || len(one.Present) != 1 {
		t.Errorf("one asset = %+v, %v", one, err)
	}
	if _, err := e.RestoreSources(ctx, RestoreSourcesOptions{AssetID: "ast_nope"}); err == nil {
		t.Error("an unknown asset was accepted")
	}
}
