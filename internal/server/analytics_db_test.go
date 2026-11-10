package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/config"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/storage"
)

// A play answered just before SIGTERM is in Postgres once serve returns:
// the recorder flushes after the HTTP server has shut down (P7 exit 5).
func TestServeFlushesPlaybackCountsOnShutdown(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	store, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: "http://localhost"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, sql := range []string{
		`INSERT INTO apps (id, project_id, name) VALUES ('app_a', 'prj_default', 'a')`,
		`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_1', 'prj_default', 'app_a', 'processing', 'sto_default')`,
		`INSERT INTO outputs (id, project_id, asset_id, key_version, status, manifest) VALUES ('out_1', 'prj_default', 'ast_1', 1, 'committed', '{"audio": true}')`,
		`UPDATE assets SET current_output_id = 'out_1', status = 'ready' WHERE id = 'ast_1'`,
		`INSERT INTO playback_ids (id, project_id, asset_id, policy) VALUES ('pid_1', 'prj_default', 'ast_1', 'public')`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	if err := store.Put(ctx, "media/ast_1/out_1/master.m3u8", strings.NewReader("#EXTM3U\n360p/index.m3u8\n")); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- serve(sctx, config.Config{}, ln, slog.New(slog.NewTextHandler(io.Discard, nil)), testBoot(t, pool, store, testBox(t)))
	}()
	url := "http://" + ln.Addr().String() + "/p/pid_1/master.m3u8"
	var res *http.Response
	for i := 0; ; i++ {
		res, err = http.Get(url)
		if err == nil && res.StatusCode == http.StatusOK {
			break
		}
		if err == nil {
			res.Body.Close()
		}
		if i == 50 {
			t.Fatalf("the master never played: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return after shutdown")
	}
	var views, bytes int64
	if err := pool.QueryRow(ctx, `SELECT sum(views), sum(bytes) FROM playback_hourly WHERE source = 'origin' AND playback_id = 'pid_1'`).Scan(&views, &bytes); err != nil {
		t.Fatal(err)
	}
	if views != 1 || bytes != int64(len(body)) || len(body) != 30 {
		t.Fatalf("views %d, bytes %d (body %d), want 1 view of 30 bytes", views, bytes, len(body))
	}
}
