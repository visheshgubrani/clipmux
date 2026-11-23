package backup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
)

// pgTools returns the pg_dump these tests use, and skips when it, or the
// pg_restore beside it, isn't installed (a failure under
// CLIPMUX_REQUIRE_MEDIA_TOOLS=1).
func pgTools(t *testing.T) string {
	t.Helper()
	pg := os.Getenv("CLIPMUX_PG_DUMP")
	if pg == "" {
		pg = "pg_dump"
	}
	for _, bin := range []string{pg, (Config{PGDump: pg}).pgRestoreBin()} {
		if _, err := exec.LookPath(bin); err != nil {
			if os.Getenv("CLIPMUX_REQUIRE_MEDIA_TOOLS") == "1" {
				t.Fatalf("%s is required (CLIPMUX_REQUIRE_MEDIA_TOOLS=1): %v", bin, err)
			}
			t.Skipf("%s not installed; skipping the real-restore test", bin)
		}
	}
	return pg
}

func setGeneration(t *testing.T, pool *pgxpool.Pool, g int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE install_epoch SET generation = $1`, g); err != nil {
		t.Fatal(err)
	}
}

func generation(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var g int64
	if err := pool.QueryRow(context.Background(), `SELECT generation FROM install_epoch`).Scan(&g); err != nil {
		t.Fatal(err)
	}
	return g
}

func tableExists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(), `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// backupsTaken counts the backups on the target, one per backup whatever tiers
// it was copied to.
func backupsTaken(t *testing.T, s *Service) int {
	t.Helper()
	es, err := s.entries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range es {
		if e.Tier == TierBase {
			n++
		}
	}
	return n
}

func restoreOpts(t *testing.T) RestoreOptions {
	return RestoreOptions{TempDir: filepath.Join(t.TempDir(), "tmp")}
}

// leftovers lists what a restore left in its temp directory.
func leftovers(t *testing.T, o RestoreOptions) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(o.TempDir, func(p string, _ os.DirEntry, _ error) error {
		if p != o.TempDir {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func TestARestoreBringsBackTheBackedUpDatabaseAndClearsWhatAMigrationAddedSince(t *testing.T) {
	pg := pgTools(t)
	pool := dbtest.New(t)
	s, clk := newService(t, pool, Config{PGDump: pg, Interval: time.Hour}, time.Now())
	ctx := context.Background()

	setGeneration(t, pool, 5)
	res, err := s.Backup(ctx, Scheduled)
	if err != nil {
		t.Fatal(err)
	}
	// The database moves on: more data, and a table from a migration the
	// backup predates.
	setGeneration(t, pool, 9)
	if _, err := pool.Exec(ctx, `CREATE TABLE added_by_a_later_migration (x int)`); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Minute)

	opt := restoreOpts(t)
	var said []string
	opt.Say = func(f string, a ...any) { said = append(said, f) }
	m, err := s.Pick(ctx, "")
	if err != nil || m.ID != res.Manifest.ID {
		t.Fatalf("Pick(latest) = %+v, %v, want the backup just taken", m, err)
	}
	out, err := s.Restore(ctx, Source{Manifest: m}, opt)
	if err != nil {
		t.Fatal(err)
	}
	if g := generation(t, pool); g != 5 {
		t.Errorf("generation after the restore = %d, want the backup's 5", g)
	}
	if tableExists(t, pool, "added_by_a_later_migration") {
		t.Error("a table the backup doesn't have survived the restore, for the next migration to trip over")
	}
	if v, _ := db.SchemaVersion(ctx, pool); v != out.SchemaVersion || v != db.LatestMigration() {
		t.Errorf("schema version %d, result says %d, want this binary's %d", v, out.SchemaVersion, db.LatestMigration())
	}
	if out.Safety == nil || out.Safety.Kind != PreRestore {
		t.Fatalf("Safety = %+v, want a pre_restore backup of the database as it was", out.Safety)
	}
	if left := leftovers(t, opt); len(left) != 0 {
		t.Errorf("the restore left %v in its temp directory: a plaintext dump", left)
	}
	if len(said) < 3 {
		t.Errorf("the restore said only %q", said)
	}

	// The safety backup is a way back: restoring it undoes the restore.
	if _, err := s.Restore(ctx, Source{Manifest: *out.Safety}, restoreOpts(t)); err != nil {
		t.Fatal(err)
	}
	if g := generation(t, pool); g != 9 || !tableExists(t, pool, "added_by_a_later_migration") {
		t.Errorf("after restoring the safety backup: generation %d, later table %v; want the database as it was before the restore", g, tableExists(t, pool, "added_by_a_later_migration"))
	}
}

func TestARestoreIntoAnEmptyDatabaseNeedsNoSafetyBackup(t *testing.T) {
	pg := pgTools(t)
	pool := dbtest.New(t)
	s, _ := newService(t, pool, Config{PGDump: pg}, time.Now())
	ctx := context.Background()
	setGeneration(t, pool, 7)
	res, err := s.Backup(ctx, Manual)
	if err != nil {
		t.Fatal(err)
	}
	// A new host: the schema exists and holds nothing.
	var schema string
	_ = pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema)
	if _, err := pool.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE; CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	s.Pool.Reset()

	out, err := s.Restore(ctx, Source{Manifest: res.Manifest}, restoreOpts(t))
	if err != nil {
		t.Fatal(err)
	}
	if out.Safety != nil {
		t.Errorf("Safety = %+v, want none for an empty database", out.Safety)
	}
	if g := generation(t, pool); g != 7 {
		t.Errorf("generation = %d, want 7", g)
	}
	if n := backupsTaken(t, s); n != 1 {
		t.Errorf("the target holds %d backups, want only the one restored from", n)
	}
}

// The checks that come before anything is changed.
func TestARestoreRefusesWhatItCannotTrustAndChangesNothing(t *testing.T) {
	pg := pgTools(t)
	pool := dbtest.New(t)
	s, _ := newService(t, pool, Config{PGDump: pg}, time.Now())
	ctx := context.Background()
	setGeneration(t, pool, 5)
	res, err := s.Backup(ctx, Manual)
	if err != nil {
		t.Fatal(err)
	}
	m := res.Manifest
	setGeneration(t, pool, 9)
	unchanged := func(what string) {
		t.Helper()
		if g := generation(t, pool); g != 9 {
			t.Errorf("%s: the database was changed (generation %d, want 9)", what, g)
		}
		if n := backupsTaken(t, s); n != 1 {
			t.Errorf("%s: %d backups on the target, want 1: no safety backup before a refusal", what, n)
		}
	}

	t.Run("a stored dump that was changed", func(t *testing.T) {
		stored := filepath.Join(strings.TrimPrefix(s.Target.Desc, "file://"), "objects", filepath.FromSlash(m.Object))
		orig, err := os.ReadFile(stored)
		if err != nil {
			t.Fatal(err)
		}
		damaged := bytes.Clone(orig)
		damaged[len(damaged)/2] ^= 0xff
		if err := os.WriteFile(stored, damaged, 0o600); err != nil {
			t.Fatal(err)
		}
		defer os.WriteFile(stored, orig, 0o600)
		if _, err := s.Restore(ctx, Source{Manifest: m}, restoreOpts(t)); err == nil {
			t.Fatal("restored a damaged dump")
		}
		unchanged("damaged")
	})

	t.Run("a dump no key here opens", func(t *testing.T) {
		other := *s
		other.Box = box(t, 1, 7)
		_, err := other.Restore(ctx, Source{Manifest: m}, restoreOpts(t))
		if err == nil || !strings.Contains(err.Error(), "CLIPMUX_SECRETS_KEY") || !strings.Contains(err.Error(), "v1") {
			t.Fatalf("err = %v, want it to say which secrets key the backup needs", err)
		}
		unchanged("wrong key")
	})

	t.Run("a dump from a newer ClipMux", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `INSERT INTO goose_db_version (version_id, is_applied) VALUES ($1, true)`, db.LatestMigration()+1); err != nil {
			t.Fatal(err)
		}
		newer, err := s.Backup(ctx, Manual)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM goose_db_version WHERE version_id > $1`, db.LatestMigration()); err != nil {
			t.Fatal(err)
		}
		_, err = s.Restore(ctx, Source{Manifest: newer.Manifest}, restoreOpts(t))
		if !errors.Is(err, db.ErrSchemaNewer) {
			t.Fatalf("err = %v, want ErrSchemaNewer", err)
		}
		if g := generation(t, pool); g != 9 {
			t.Errorf("the database was changed (generation %d, want 9)", g)
		}
		if n := backupsTaken(t, s); n != 2 {
			t.Errorf("%d backups on the target, want the two taken: no safety backup before a refusal", n)
		}
	})

	t.Run("a dump of another schema", func(t *testing.T) {
		other := dbtest.New(t) // another schema of the same database
		o := *s
		o.Pool = other
		_, err := o.Restore(ctx, Source{Manifest: m}, restoreOpts(t))
		if err == nil || !strings.Contains(err.Error(), "schema") {
			t.Fatalf("err = %v, want a refusal that names the schema mismatch", err)
		}
		if g := generation(t, other); g == 5 {
			t.Error("the other schema was restored into")
		}
	})
}

func TestARestoreReadsADumpFileOffTheTargetOrAPlainPgDump(t *testing.T) {
	pg := pgTools(t)
	pool := dbtest.New(t)
	s, _ := newService(t, pool, Config{PGDump: pg}, time.Now())
	ctx := context.Background()
	setGeneration(t, pool, 5)
	res, err := s.Backup(ctx, Manual)
	if err != nil {
		t.Fatal(err)
	}
	encrypted := filepath.Join(t.TempDir(), "copied.dump.age")
	if err := os.WriteFile(encrypted, read(t, s.Target, res.Manifest.Object), 0o600); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(t.TempDir(), "plain.dump")
	r, err := age.Decrypt(bytes.NewReader(read(t, s.Target, res.Manifest.Object)), mustIdentities(t, s.Box)...)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	if err := os.WriteFile(plain, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	junk := filepath.Join(t.TempDir(), "notes.txt")
	_ = os.WriteFile(junk, []byte("not a dump"), 0o600)

	for name, file := range map[string]string{"encrypted": encrypted, "plain": plain} {
		setGeneration(t, pool, 9)
		out, err := s.Restore(ctx, Source{File: file}, restoreOpts(t))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if g := generation(t, pool); g != 5 || out.Safety == nil {
			t.Errorf("%s: generation %d, safety %v; want 5 and a safety backup", name, g, out.Safety)
		}
	}
	if _, err := os.Stat(plain); err != nil {
		t.Errorf("the plain dump file was removed after the restore: %v", err)
	}
	setGeneration(t, pool, 9)
	if _, err := s.Restore(ctx, Source{File: junk}, restoreOpts(t)); err == nil || generation(t, pool) != 9 {
		t.Errorf("a file that isn't a dump: err %v, generation %d; want a refusal and no change", err, generation(t, pool))
	}
}

func TestPickFindsABackupByIDAndSaysWhenThereIsNone(t *testing.T) {
	pool := dbtest.New(t)
	s, clk := newService(t, pool, Config{PGDump: fakeDump(t, "PGDMP x", 0)}, time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC))
	ctx := context.Background()
	if _, err := s.Pick(ctx, ""); err == nil || !strings.Contains(err.Error(), "no backups") {
		t.Errorf("Pick on an empty target: %v", err)
	}
	first, err := s.Backup(ctx, Manual)
	if err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Hour)
	second, err := s.Backup(ctx, Manual)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := s.Pick(ctx, first.Manifest.ID); err != nil || m.ID != first.Manifest.ID {
		t.Errorf("Pick(first) = %v, %v", m.ID, err)
	}
	if m, err := s.Pick(ctx, ""); err != nil || m.ID != second.Manifest.ID {
		t.Errorf("Pick(latest) = %v, %v, want the newer", m.ID, err)
	}
	if _, err := s.Pick(ctx, "bak_nope"); err == nil || !strings.Contains(err.Error(), "bak_nope") {
		t.Errorf("Pick(unknown): %v", err)
	}
}
