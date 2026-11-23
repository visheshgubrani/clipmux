package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/secrets"
)

// scratchDatabases lists the scratch databases of this test's install: other
// tests share the server, and their verifications are theirs.
func scratchDatabases(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT datname FROM pg_database
		WHERE datname LIKE 'clipmux\_verify\_' || substr(md5(current_database() || '/' || current_schema()), 1, 12) || '\_%'`)
	if err != nil {
		t.Fatal(err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func verifyService(t *testing.T) (*Service, *clock) {
	t.Helper()
	pool := dbtest.New(t)
	s, clk := newService(t, pool, Config{PGDump: pgTools(t)}, time.Now())
	s.TempDir = filepath.Join(t.TempDir(), "tmp")
	return s, clk
}

func TestABackupIsProvedRestorableInAScratchDatabaseThatIsThenDropped(t *testing.T) {
	s, _ := verifyService(t)
	ctx := context.Background()
	// A database a killed verification left behind goes too; another install's
	// on the same server stays.
	var token string
	if err := s.Pool.QueryRow(ctx, `SELECT substr(md5(current_database() || '/' || current_schema()), 1, 12)`).Scan(&token); err != nil {
		t.Fatal(err)
	}
	mine, theirs := "clipmux_verify_"+token+"_leftover", "clipmux_verify_000000000000_another"
	for _, name := range []string{mine, theirs} {
		if _, err := s.Pool.Exec(ctx, `CREATE DATABASE `+name); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _, _ = s.Pool.Exec(context.Background(), `DROP DATABASE IF EXISTS `+theirs) })
	// Something for the counts to count.
	if _, err := s.Pool.Exec(ctx, `INSERT INTO apps (id, project_id, name) VALUES ('app_a', $1, 'A'), ('app_b', $1, 'B')`, db.DefaultProjectID); err != nil {
		t.Fatal(err)
	}
	var apps int64
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM apps`).Scan(&apps); err != nil {
		t.Fatal(err)
	}
	res, err := s.Backup(ctx, Scheduled)
	if err != nil {
		t.Fatal(err)
	}
	m := res.Manifest
	if m.RowCounts["apps"] != apps || apps < 2 || m.RowCounts["projects"] != 1 || len(m.RowCounts) != len(CountedTables) {
		t.Fatalf("RowCounts = %v, want the rows the dump holds", m.RowCounts)
	}
	// More rows after the backup don't change what it must hold.
	if _, err := s.Pool.Exec(ctx, `INSERT INTO apps (id, project_id, name) VALUES ('app_c', $1, 'C')`, db.DefaultProjectID); err != nil {
		t.Fatal(err)
	}

	vr, err := s.Verify(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	if vr.Mode != ModeRestore || len(vr.Checks) < 6 {
		t.Errorf("result = %+v, want a restore into a scratch database with every check passed", vr)
	}
	if left := scratchDatabases(t, s.Pool); len(left) != 0 {
		t.Errorf("scratch databases left: %v", left)
	}
	var other int
	_ = s.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_database WHERE datname = $1`, theirs).Scan(&other)
	if other != 1 {
		t.Error("another install's scratch database was dropped")
	}
	if entries, _ := os.ReadDir(s.TempDir); len(entries) != 0 {
		t.Errorf("a plaintext dump was left in %s", s.TempDir)
	}
}

func TestAVerificationFailsOnWhatARestoreWouldGetWrong(t *testing.T) {
	s, _ := verifyService(t)
	ctx := context.Background()
	res, err := s.Backup(ctx, Manual)
	if err != nil {
		t.Fatal(err)
	}
	good := res.Manifest

	t.Run("rows that aren't what the manifest recorded", func(t *testing.T) {
		m := good
		m.RowCounts = map[string]int64{"projects": 1, "apps": 5}
		_, err := s.Verify(ctx, m)
		if err == nil || !strings.Contains(err.Error(), "the manifest recorded 5") {
			t.Fatalf("err = %v, want the mismatch named", err)
		}
	})
	t.Run("a stored dump that was changed", func(t *testing.T) {
		stored := filepath.Join(strings.TrimPrefix(s.Target.Desc, "file://"), "objects", filepath.FromSlash(good.Object))
		orig, err := os.ReadFile(stored)
		if err != nil {
			t.Fatal(err)
		}
		damaged := append([]byte(nil), orig...)
		damaged[len(damaged)/2] ^= 0xff
		if err := os.WriteFile(stored, damaged, 0o600); err != nil {
			t.Fatal(err)
		}
		defer os.WriteFile(stored, orig, 0o600)
		vr, err := s.Verify(ctx, good)
		if err == nil || vr.Mode != ModeIntegrity {
			t.Fatalf("Verify = %+v, %v, want a failure at the file", vr, err)
		}
	})
	t.Run("a manifest that disagrees with the dump", func(t *testing.T) {
		m := good
		m.SchemaVersion--
		if _, err := s.Verify(ctx, m); err == nil || !strings.Contains(err.Error(), "the dump holds migration") {
			t.Fatalf("err = %v", err)
		}
	})
	if left := scratchDatabases(t, s.Pool); len(left) != 0 {
		t.Errorf("failures left scratch databases: %v", left)
	}
}

// A backup whose secrets this install's key can't open is no backup.
func TestAVerificationFailsWhenTheSecretsKeyDoesNotOpenTheBackupsSecrets(t *testing.T) {
	s, _ := verifyService(t)
	ctx := context.Background()
	// The database's secrets are sealed under one key; the install holds
	// another of the same version, so it can encrypt a backup but not open
	// what is in it.
	if _, err := secrets.LoadOrCreate(ctx, db.New(s.Pool), db.DefaultProjectID, t.TempDir(), "v1:"+strings.Repeat("c", 64), ""); err != nil {
		t.Fatal(err)
	}
	res, err := s.Backup(ctx, Manual)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Verify(ctx, res.Manifest)
	if err == nil || !strings.Contains(err.Error(), "secrets key doesn't open") {
		t.Fatalf("err = %v, want the key named as what fails", err)
	}
}

func TestTheResultIsRecordedInTheLedgerEvenForABackupItHasNoRowFor(t *testing.T) {
	s, _ := verifyService(t)
	ctx := context.Background()
	res, err := s.Backup(ctx, Manual)
	if err != nil {
		t.Fatal(err)
	}
	q := db.New(s.Pool)
	if _, err := s.VerifyAndRecord(ctx, res.Manifest); err != nil {
		t.Fatal(err)
	}
	row, err := q.LastBackupVerification(ctx, db.DefaultProjectID)
	if err != nil || row.ID != res.Manifest.ID || row.VerifyMode.String != ModeRestore || row.VerifyError.Valid {
		t.Fatalf("ledger = %+v, %v, want a clean restore verification", row, err)
	}

	// After restoring an older database the ledger lacks the newest backup.
	if _, err := s.Pool.Exec(ctx, `DELETE FROM backups`); err != nil {
		t.Fatal(err)
	}
	bad := res.Manifest
	bad.RowCounts = map[string]int64{"projects": 9}
	if _, err := s.VerifyAndRecord(ctx, bad); err == nil {
		t.Fatal("a mismatch passed")
	}
	row, err = q.LastBackupVerification(ctx, db.DefaultProjectID)
	if err != nil || row.ID != bad.ID || !strings.Contains(row.VerifyError.String, "projects has 1 rows, the manifest recorded 9") {
		t.Fatalf("ledger = %+v, %v, want the failure recorded on a row made for the backup", row, err)
	}
}

func TestTheLoopVerifiesTheNewestBackupWeeklyAndRetriesAFailureSooner(t *testing.T) {
	s, clk := verifyService(t)
	start := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	clk.t = start
	ctx := context.Background()
	pass := func(after time.Duration) (int, error) {
		clk.t = start.Add(after)
		return s.RunVerify(ctx)
	}
	if n, err := pass(0); n != 0 || err != nil {
		t.Fatalf("with no backup: %d, %v, want nothing to verify", n, err)
	}
	res, err := s.Backup(ctx, Scheduled)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := pass(time.Minute); n != 1 || err != nil {
		t.Fatalf("a backup never verified: %d, %v, want it verified now", n, err)
	}
	if n, _ := pass(6 * 24 * time.Hour); n != 0 {
		t.Errorf("6 days later verified again (%d)", n)
	}

	// A week on, the dump is damaged: the check fails, and is tried again in
	// hours, not a week.
	stored := filepath.Join(strings.TrimPrefix(s.Target.Desc, "file://"), "objects", filepath.FromSlash(res.Manifest.Object))
	orig, _ := os.ReadFile(stored)
	bad := append([]byte(nil), orig...)
	bad[len(bad)/2] ^= 0xff
	_ = os.WriteFile(stored, bad, 0o600)
	if n, err := pass(7*24*time.Hour + time.Minute); n != 1 || err == nil || !strings.Contains(err.Error(), res.Manifest.ID) {
		t.Fatalf("a week later: %d, %v, want a recorded failure naming the backup", n, err)
	}
	row, _ := db.New(s.Pool).LastBackupVerification(ctx, db.DefaultProjectID)
	if !row.VerifyError.Valid {
		t.Fatal("the failure isn't in the ledger")
	}
	if n, _ := pass(7*24*time.Hour + 5*time.Hour); n != 0 {
		t.Errorf("5 hours after a failure retried (%d)", n)
	}
	_ = os.WriteFile(stored, orig, 0o600)
	if n, err := pass(7*24*time.Hour + 7*time.Hour); n != 1 || err != nil {
		t.Errorf("7 hours after a failure: %d, %v, want a retry that passes", n, err)
	}
	row, _ = db.New(s.Pool).LastBackupVerification(ctx, db.DefaultProjectID)
	if row.VerifyError.Valid {
		t.Errorf("the ledger still shows the failure: %q", row.VerifyError.String)
	}
}

// Without CREATEDB the file is proved, and that is said.
func TestARoleThatCannotCreateDatabasesGetsAnIntegrityCheck(t *testing.T) {
	s, _ := verifyService(t)
	ctx := context.Background()
	var schema string
	_ = s.Pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema)
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	role := "cmx_nocdb_" + hex.EncodeToString(suffix)
	for _, q := range []string{
		"CREATE ROLE " + role + " LOGIN PASSWORD 'pw' NOCREATEDB",
		"GRANT USAGE ON SCHEMA " + schema + " TO " + role,
		"GRANT SELECT ON ALL TABLES IN SCHEMA " + schema + " TO " + role,
		"GRANT SELECT ON ALL SEQUENCES IN SCHEMA " + schema + " TO " + role,
		"GRANT INSERT, UPDATE ON backups TO " + role,
	} {
		if _, err := s.Pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	t.Cleanup(func() {
		_, _ = s.Pool.Exec(context.Background(), "DROP OWNED BY "+role)
		_, _ = s.Pool.Exec(context.Background(), "DROP ROLE "+role)
	})
	res, err := s.Backup(ctx, Manual)
	if err != nil {
		t.Fatal(err)
	}

	u, _ := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	u.User = url.UserPassword(role, "pw")
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	pool, err := db.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	limited := *s
	limited.Pool, limited.DatabaseURL = pool, u.String()

	vr, err := limited.Verify(ctx, res.Manifest)
	if err != nil || vr.Mode != ModeIntegrity || len(vr.Checks) != 2 {
		t.Fatalf("Verify = %+v, %v, want the file proved and nothing restored", vr, err)
	}
	if left := scratchDatabases(t, s.Pool); len(left) != 0 {
		t.Errorf("scratch databases left: %v", left)
	}
}
