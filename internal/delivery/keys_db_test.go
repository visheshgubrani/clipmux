package delivery

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/stores"
)

// A rotated content key secret applies to new outputs; a viewer mid-session
// on an older output keeps getting its key (docs/architecture.md, "Content
// protection", Rotation).
func TestDeliveryServesEveryVersionAnOutputUses(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	if _, err := pool.Exec(ctx, `
		INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default');
		INSERT INTO outputs (id, project_id, asset_id, key_version, status, manifest) VALUES
		  ('out_old', 'prj_default', 'ast_1', 1, 'retired', '{}'),
		  ('out_new', 'prj_default', 'ast_1', 2, 'committed', '{}');
		UPDATE assets SET status = 'ready', current_output_id = 'out_new' WHERE id = 'ast_1';
		INSERT INTO playback_ids (id, project_id, asset_id, policy) VALUES ('pid_1', 'prj_default', 'ast_1', 'public');`); err != nil {
		t.Fatal(err)
	}
	store, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: "http://unused"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	v1, _ := contentkey.FromHex(1, "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	v2, _ := contentkey.FromHex(2, "202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f")
	mux := http.NewServeMux()
	New(pool, stores.Fixed(store), contentkey.NewRing(v1, v2)).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	get := func(path string) (int, string) {
		t.Helper()
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	old, cur := v1.Derive("ast_1", "out_old"), v2.Derive("ast_1", "out_new")
	if status, body := get("/p/pid_1/out_old/key/1"); status != 200 || body != string(old[:]) {
		t.Fatalf("retired output's v1 key = %d, want 200 with its key", status)
	}
	if status, body := get("/p/pid_1/out_new/key/2"); status != 200 || body != string(cur[:]) {
		t.Fatalf("current output's v2 key = %d, want 200 with its key", status)
	}
	if status, _ := get("/p/pid_1/out_new/key/1"); status != 404 {
		t.Fatalf("an output's key under another version = %d, want 404", status)
	}
}

func TestARevocationIsSeenAtTheNextRequestOnceInvalidated(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	exec := func(sql string) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default');
	      INSERT INTO outputs (id, project_id, asset_id, key_version, status, manifest) VALUES ('out_1', 'prj_default', 'ast_1', 1, 'committed', '{}');
	      UPDATE assets SET status = 'ready', current_output_id = 'out_1' WHERE id = 'ast_1';
	      INSERT INTO playback_ids (id, project_id, asset_id, policy) VALUES ('pid_1', 'prj_default', 'ast_1', 'public');`)
	store, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: "http://unused"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	v1, _ := contentkey.FromHex(1, "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	h := New(pool, stores.Fixed(store), contentkey.NewRing(v1))
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	status := func() int {
		res, err := http.Get(srv.URL + "/p/pid_1/out_1/key/1")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if s := status(); s != 200 {
		t.Fatalf("before: %d", s)
	}
	exec(`UPDATE playback_ids SET status = 'revoked', revoked_at = now() WHERE id = 'pid_1'`)
	if s := status(); s != 200 {
		t.Fatalf("the route wasn't cached: %d", s) // published changes invalidate; this one didn't publish
	}
	h.Invalidate("pid_1") // what confirming its unpublish does
	if s := status(); s != 404 {
		t.Fatalf("after the unpublish was confirmed: %d, want 404 at once", s)
	}
}
