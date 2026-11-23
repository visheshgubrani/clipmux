package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/backup"
	"github.com/visheshgubrani/clipmux/internal/config"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/storage"
)

// serve takes a backup when it starts, and not again until the interval has
// passed, and stops cleanly with its loops.
func TestServeBacksUpAtBootAndStopsCleanly(t *testing.T) {
	pool := dbtest.New(t)
	store, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: "http://localhost"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	dump := filepath.Join(t.TempDir(), "pg_dump")
	if err := os.WriteFile(dump, []byte("#!/bin/sh\nprintf 'PGDMP serve'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	cfg := config.Config{
		DatabaseURL: os.Getenv("TEST_DATABASE_URL"),
		Backup:      backup.Config{URL: "file://" + target, PGDump: dump, Interval: time.Hour},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, cfg, ln, slog.New(slog.NewTextHandler(io.Discard, nil)), testBoot(t, pool, store, testBox(t)))
	}()
	// A failed test mustn't leave serve running, or closing the pool waits for
	// its loops.
	stopped := false
	t.Cleanup(func() {
		cancel()
		if !stopped {
			<-done
		}
	})

	q := db.New(pool)
	deadline := time.Now().Add(15 * time.Second)
	for {
		if row, err := q.LastBackup(context.Background(), db.DefaultProjectID); err == nil {
			if row.Kind != backup.Scheduled {
				t.Errorf("kind = %s, want a scheduled backup", row.Kind)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("serve took no backup within 15 seconds of starting")
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		stopped = true
		if err != nil {
			t.Fatalf("serve returned %v, want nil on shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
}

// serve boots no further than the migration when the backup before it fails:
// the database is left as it was.
func TestServeRefusesToMigrateWhenTheBackupBeforeItFails(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	if err := db.MigrateDownTo(ctx, pool, db.LatestMigration()-1); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	failing := filepath.Join(t.TempDir(), "pg_dump")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\necho 'pg_dump: error: boom' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		DatabaseURL: u.String(),
		DataDir:     t.TempDir(),
		SecretsKey:  "v1:" + strings.Repeat("a", 64),
		Backup:      backup.Config{URL: "file://" + t.TempDir(), PGDump: failing, BeforeMigrate: true, Interval: time.Hour},
	}
	err = Run(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "not migrating") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Run = %v, want a refusal to migrate that says why", err)
	}
	if v, _ := db.SchemaVersion(ctx, pool); v != db.LatestMigration()-1 {
		t.Errorf("the schema is at %d, want it left at %d", v, db.LatestMigration()-1)
	}
}

// serve checks the newest backup weekly, and at once when it never has.
func TestServeVerifiesTheNewestBackupAtBoot(t *testing.T) {
	pool := dbtest.New(t)
	store, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: "http://localhost"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	dump := filepath.Join(t.TempDir(), "pg_dump")
	if err := os.WriteFile(dump, []byte("#!/bin/sh\nprintf 'PGDMP serve'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		DatabaseURL: os.Getenv("TEST_DATABASE_URL"),
		DataDir:     t.TempDir(),
		Backup:      backup.Config{URL: "file://" + t.TempDir(), PGDump: dump, Interval: time.Hour},
	}
	box := testBox(t)
	target, err := backup.OpenTarget(cfg.Backup)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if _, err := backup.New(pool, box, target, cfg.Backup, cfg.DatabaseURL, "v-test").Backup(context.Background(), backup.Manual); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, cfg, ln, slog.New(slog.NewTextHandler(io.Discard, nil)), testBoot(t, pool, store, box))
	}()
	defer func() {
		cancel()
		<-done
	}()
	q := db.New(pool)
	deadline := time.Now().Add(20 * time.Second)
	for {
		// The fake dump is no archive pg_restore reads, so what is recorded is
		// a failure, and that it was recorded shows the loop ran.
		if row, err := q.LastBackupVerification(context.Background(), db.DefaultProjectID); err == nil {
			if !row.VerifyError.Valid || row.VerifyMode.String != backup.ModeIntegrity {
				t.Errorf("verification = %+v, want the fake dump's failure recorded", row)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("serve verified no backup within 20 seconds of starting")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// serve copies committed sources to the backup target.
func TestServeMirrorsACommittedSourceToTheBackupTarget(t *testing.T) {
	pool := dbtest.New(t)
	store, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: "http://localhost"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO assets (id, project_id, app_id, status, storage_id, source_uri)
		VALUES ('ast_m', 'prj_default', 'app_dashboard', 'processing', 'sto_default', 'uploads/ast_m/source')`); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, "uploads/ast_m/source", strings.NewReader("mirror me")); err != nil {
		t.Fatal(err)
	}
	dump := filepath.Join(t.TempDir(), "pg_dump")
	if err := os.WriteFile(dump, []byte("#!/bin/sh\nprintf 'PGDMP serve'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		DatabaseURL: os.Getenv("TEST_DATABASE_URL"),
		DataDir:     t.TempDir(),
		Backup:      backup.Config{URL: "file://" + t.TempDir(), PGDump: dump, Interval: time.Hour},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- serve(sctx, cfg, ln, slog.New(slog.NewTextHandler(io.Discard, nil)), testBoot(t, pool, store, testBox(t)))
	}()
	defer func() {
		cancel()
		<-done
	}()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var at *time.Time
		if err := pool.QueryRow(ctx, `SELECT source_backed_up_at FROM assets WHERE id = 'ast_m'`).Scan(&at); err != nil {
			t.Fatal(err)
		}
		if at != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("serve copied no source within 20 seconds of starting")
		}
		time.Sleep(50 * time.Millisecond)
	}
	target, err := backup.OpenTarget(cfg.Backup)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	rc, err := target.Get(ctx, "sources/ast_m.age")
	if err != nil {
		t.Fatalf("the encrypted copy isn't on the target: %v", err)
	}
	defer rc.Close()
	if raw, _ := io.ReadAll(rc); len(raw) == 0 || strings.Contains(string(raw), "mirror me") {
		t.Errorf("the copy is empty or in the clear: %d bytes", len(raw))
	}
}
