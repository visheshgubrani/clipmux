package retention_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/retention"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/stores"
)

func TestFilesGoOnlyOnceNothingCanPlayOrWriteThem(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	dir := t.TempDir()
	fs, err := storage.OpenFilesystem(dir, storage.FilesystemOptions{BaseURL: "http://unused"})
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	put := func(key string) {
		t.Helper()
		if err := fs.Put(ctx, key, strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default')`)
	exec(`INSERT INTO playback_ids (id, project_id, asset_id, policy) VALUES ('pid_1', 'prj_default', 'ast_1', 'public')`)
	// Outputs: current, retired a day ago, retired 8 days ago, and the
	// writing outputs of attempts replaced 31 and 29 minutes ago.
	for _, o := range []struct{ id, status, retired string }{
		{"out_current", "committed", ""}, {"out_retired1d", "retired", "1 day"}, {"out_retired8d", "retired", "8 days"},
		{"out_lost31", "writing", ""}, {"out_lost29", "writing", ""}, {"out_next31", "writing", ""}, {"out_next29", "writing", ""},
	} {
		manifest := "NULL"
		if o.status != "writing" {
			manifest = "'{}'"
		}
		retired := "NULL"
		if o.retired != "" {
			retired = "now() - interval '" + o.retired + "'"
		}
		exec(`INSERT INTO outputs (id, project_id, asset_id, key_version, status, manifest, retired_at) VALUES ($1, 'prj_default', 'ast_1', 1, $2, `+manifest+`, `+retired+`)`, o.id, o.status)
		put("media/ast_1/" + o.id + "/master.m3u8")
	}
	exec(`UPDATE assets SET current_output_id = 'out_current', status = 'ready' WHERE id = 'ast_1'`)
	for job, outs := range map[string][2]string{"job_31": {"out_lost31", "out_next31"}, "job_29": {"out_lost29", "out_next29"}} {
		ago := strings.TrimPrefix(job, "job_")
		// One job per asset may be active: the 29-minute one was cancelled
		// 29 minutes ago, which keeps its files as long.
		status, finished := "running", "NULL"
		if ago == "29" {
			status, finished = "cancelled", "now() - interval '29 minutes'"
		}
		exec(`INSERT INTO jobs (id, project_id, asset_id, kind, status, routed_to, route_reason, finished_at) VALUES ($1, 'prj_default', 'ast_1', 'reencode', $2, 'workers', 'primary', `+finished+`)`, job, status)
		exec(`INSERT INTO attempts (id, project_id, job_id, asset_id, attempt_number, output_id, executor, status, token_hash, lease_expires_at, started_at)
		      VALUES ($1, 'prj_default', $2, 'ast_1', 1, $3, 'builtin', 'lost', $4, now(), now() - interval '1 hour')`, "att_a"+ago, job, outs[0], []byte("a"+ago))
		exec(`INSERT INTO attempts (id, project_id, job_id, asset_id, attempt_number, output_id, executor, status, token_hash, lease_expires_at, started_at)
		      VALUES ($1, 'prj_default', $2, 'ast_1', 2, $3, 'builtin', 'running', $4, now(), now() - make_interval(mins => $5))`, "att_b"+ago, job, outs[1], []byte("b"+ago), map[string]int{"31": 31, "29": 29}[ago])
	}
	// An orphan output prefix, and an asset's tracks, which aren't outputs.
	put("media/ast_1/out_orphan/master.m3u8")
	put("media/ast_1/tracks/trk_1.vtt")
	// Filesystem leftovers: a staged write from yesterday, and one from now.
	for name, age := range map[string]time.Duration{"old": 25 * time.Hour, "new": time.Hour} {
		p := filepath.Join(dir, "tmp", name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}

	files := retention.NewFiles(pool, stores.Fixed(fs), fs)
	if _, err := files.Run(ctx); err != nil {
		t.Fatal(err)
	}
	stored := map[string]bool{}
	objs, _, err := fs.List(ctx, "media/ast_1/", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		stored[strings.Split(strings.TrimPrefix(o.Key, "media/ast_1/"), "/")[0]] = true
	}
	for id, want := range map[string]bool{
		"out_current": true, "out_retired1d": true, "out_retired8d": false,
		"out_lost31": false, "out_lost29": true, "out_next31": true, "out_next29": true,
		"out_orphan": false, "tracks": true,
	} {
		if stored[id] != want {
			t.Errorf("%s stored = %v, want %v", id, stored[id], want)
		}
	}
	var purged []string
	rows, _ := pool.Query(ctx, `SELECT id FROM outputs WHERE purged_at IS NOT NULL ORDER BY id`)
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		purged = append(purged, id)
	}
	rows.Close()
	if strings.Join(purged, ",") != "out_lost31,out_retired8d" {
		t.Errorf("purged = %v, want out_lost31 and out_retired8d", purged)
	}
	var refreshes int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE kind = 'route_sync' AND payload->>'op' = 'refresh'`).Scan(&refreshes); err != nil || refreshes != 1 {
		t.Errorf("route refreshes = %d (%v), want 1 for the purged retired output", refreshes, err)
	}
	var servable []string
	rows, _ = pool.Query(ctx, `SELECT id FROM outputs WHERE asset_id = 'ast_1' AND status IN ('committed', 'retired') AND purged_at IS NULL ORDER BY id`)
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		servable = append(servable, id)
	}
	rows.Close()
	if strings.Join(servable, ",") != "out_current,out_retired1d" {
		t.Errorf("servable = %v", servable)
	}
	for name, want := range map[string]bool{"old": false, "new": true} {
		if _, err := os.Stat(filepath.Join(dir, "tmp", name)); (err == nil) != want {
			t.Errorf("tmp/%s kept = %v, want %v", name, err == nil, want)
		}
	}
}

// Retired tracks go with the output window (P5-T15): one retired 8 days ago
// stops being served and loses its file; one retired a day ago, and the
// ready one, stay.
func TestRetiredTracksGoWithTheOutputWindow(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	fs, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: "http://unused"})
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default')`)
	exec(`INSERT INTO playback_ids (id, project_id, asset_id, policy) VALUES ('pid_1', 'prj_default', 'ast_1', 'public')`)
	for _, tr := range []struct{ id, status, retired string }{{"trk_ready", "ready", ""}, {"trk_r1", "retired", "1 day"}, {"trk_r8", "retired", "8 days"}} {
		retired := "NULL"
		if tr.retired != "" {
			retired = "now() - interval '" + tr.retired + "'"
		}
		exec(`INSERT INTO tracks (id, project_id, asset_id, kind, status, retired_at) VALUES ($1, 'prj_default', 'ast_1', 'subtitles', $2, `+retired+`)`, tr.id, tr.status)
		if err := fs.Put(ctx, "media/ast_1/tracks/"+tr.id+".vtt", strings.NewReader("sealed")); err != nil {
			t.Fatal(err)
		}
	}
	n, err := retention.NewFiles(pool, stores.Fixed(fs), fs).PurgeRetiredTracks(ctx)
	if err != nil || n != 1 {
		t.Fatalf("purged %d, %v; want 1", n, err)
	}
	for id, want := range map[string]string{"trk_ready": "ready", "trk_r1": "retired", "trk_r8": "deleted"} {
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM tracks WHERE id = $1`, id).Scan(&status); err != nil || status != want {
			t.Errorf("%s = %s (%v), want %s", id, status, err, want)
		}
		_, err := fs.Head(ctx, "media/ast_1/tracks/"+id+".vtt")
		if (err == nil) != (want != "deleted") {
			t.Errorf("%s file kept = %v", id, err == nil)
		}
	}
	var refreshes int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE kind = 'route_sync' AND payload->>'op' = 'refresh'`).Scan(&refreshes); err != nil || refreshes != 1 {
		t.Errorf("route refreshes = %d (%v), want 1", refreshes, err)
	}
}
