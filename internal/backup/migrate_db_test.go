package backup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/secrets"
)

// olderDatabase is a populated database some migrations behind this binary:
// the state of a server about to be upgraded. Two behind is before migration
// 40, which made the backups table, so there is none, as on an upgrade from
// before T29; one behind has the table, without the columns a later migration
// gave it.
func olderDatabase(t *testing.T, behind int64) *pgxpool.Pool {
	t.Helper()
	pool := dbtest.New(t)
	if err := db.MigrateDownTo(context.Background(), pool, db.LatestMigration()-behind); err != nil {
		t.Fatal(err)
	}
	if v, _ := db.SchemaVersion(context.Background(), pool); v != db.LatestMigration()-behind {
		t.Fatalf("schema version %d after rolling back %d migrations", v, behind)
	}
	return pool
}

func dsn() string { return os.Getenv("TEST_DATABASE_URL") }

func migrateOpts(t *testing.T, cfg Config, boxCalls *int) MigrateOptions {
	t.Helper()
	if cfg.URL == "" {
		cfg.URL = "file://" + t.TempDir()
	}
	cfg.BeforeMigrate = true
	return MigrateOptions{
		Config: cfg, DatabaseURL: dsn(), Version: "v-test",
		Box: func() (*secrets.Box, error) {
			if boxCalls != nil {
				*boxCalls++
			}
			return box(t, 1, 9), nil
		},
	}
}

func TestPendingMigrationsAreAppliedAfterABackupOfTheDatabaseAsItIs(t *testing.T) {
	pg := pgTools(t)
	ctx := context.Background()
	// Behind by one the ledger table exists, so the row is written before the
	// migration; from before the migration that made it (40) it doesn't, so the
	// row is written after.
	const backupsMigration = 40
	for _, behind := range []int64{1, db.LatestMigration() - (backupsMigration - 1)} {
		pool := olderDatabase(t, behind)
		o := migrateOpts(t, Config{PGDump: pg}, nil)
		was := db.LatestMigration() - behind

		res, err := Migrate(ctx, pool, o)
		if err != nil {
			t.Fatalf("%d behind: %v", behind, err)
		}
		if v, _ := db.SchemaVersion(ctx, pool); v != db.LatestMigration() || int64(res.Pending) != behind {
			t.Errorf("%d behind: schema version %d, %d applied; want it migrated to %d", behind, v, res.Pending, db.LatestMigration())
		}
		if res.Backup == nil || res.Backup.Kind != PreMigrate || res.Backup.SchemaVersion != was {
			t.Fatalf("%d behind: Backup = %+v, want a pre_migrate backup at migration %d", behind, res.Backup, was)
		}
		row, err := db.New(pool).LastBackup(ctx, db.DefaultProjectID)
		if err != nil || row.ID != res.Backup.ID || row.Kind != PreMigrate || row.SchemaVersion != was {
			t.Errorf("%d behind: ledger = %+v, %v, want the pre_migrate backup at migration %d", behind, row, err, was)
		}

		// The dump is of the database before the migrations: it lacks the
		// tables they made.
		tg, err := OpenTarget(o.Config)
		if err != nil {
			t.Fatal(err)
		}
		r, err := age.Decrypt(bytes.NewReader(read(t, tg, res.Backup.Object)), mustIdentities(t, box(t, 1, 9))...)
		if err != nil {
			t.Fatal(err)
		}
		tg.Close()
		cmd := exec.Command((Config{PGDump: pg}).pgRestoreBin(), "--list")
		cmd.Stdin = r
		list, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("pg_restore --list: %v\n%s", err, list)
		}
		hasBackups := strings.Contains(string(list), " backups ")
		if !strings.Contains(string(list), " projects ") || hasBackups != (was >= backupsMigration) {
			t.Errorf("%d behind: the backup isn't of the database before the migrations (backups table in it: %v)\n%s", behind, hasBackups, list)
		}

		// Nothing pending now: no second backup, and no key needed.
		calls := 0
		o.Box = func() (*secrets.Box, error) { calls++; return nil, errors.New("no key") }
		if res, err := Migrate(ctx, pool, o); err != nil || res.Backup != nil || res.Pending != 0 || calls != 0 {
			t.Errorf("%d behind: a second Migrate = %+v, %v, %d key loads; want nothing to do", behind, res, err, calls)
		}
	}
}

func TestAFailedBackupStopsTheMigrationUnlessItIsTurnedOff(t *testing.T) {
	pool := olderDatabase(t, 1)
	ctx := context.Background()
	before := db.LatestMigration() - 1
	schema := func() int64 { v, _ := db.SchemaVersion(ctx, pool); return v }

	for name, o := range map[string]MigrateOptions{
		"the dump fails": migrateOpts(t, Config{PGDump: fakeDump(t, "x", 2)}, nil),
		"there is no key": func() MigrateOptions {
			o := migrateOpts(t, Config{PGDump: fakeDump(t, "PGDMP x", 0)}, nil)
			o.Box = func() (*secrets.Box, error) { return nil, secrets.ErrNoKey }
			return o
		}(),
		"the target is unusable": migrateOpts(t, Config{PGDump: fakeDump(t, "PGDMP x", 0), URL: "s3://b/p?endpoint=http://127.0.0.1:1"}, nil),
	} {
		_, err := Migrate(ctx, pool, o)
		if err == nil || !strings.Contains(err.Error(), "not migrating") || !strings.Contains(err.Error(), "CLIPMUX_BACKUP_BEFORE_MIGRATE=false") {
			t.Errorf("%s: err = %v, want a refusal that says how to go on without a backup", name, err)
		}
		if v := schema(); v != before {
			t.Fatalf("%s: the database was migrated to %d without a backup", name, v)
		}
	}

	o := migrateOpts(t, Config{PGDump: fakeDump(t, "x", 2)}, nil)
	o.Config.BeforeMigrate = false
	res, err := Migrate(ctx, pool, o)
	if err != nil || res.Backup != nil || schema() != db.LatestMigration() {
		t.Errorf("with the backup turned off: %+v, %v, schema %d; want it migrated without one", res, err, schema())
	}
}

func TestANewDatabaseIsMigratedWithoutABackup(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	if err := db.MigrateDownAll(ctx, pool); err != nil {
		t.Fatal(err)
	}
	calls := 0
	o := migrateOpts(t, Config{PGDump: fakeDump(t, "x", 2)}, &calls)
	res, err := Migrate(ctx, pool, o)
	if err != nil || res.Backup != nil || res.Pending != int(db.LatestMigration()) || calls != 0 {
		t.Fatalf("Migrate on a new database = %+v, %v, %d key loads; want it migrated with no backup", res, err, calls)
	}
}

func TestADatabaseNewerThanTheBinaryIsRefusedNotBackedUp(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO goose_db_version (version_id, is_applied) VALUES ($1, true)`, db.LatestMigration()+1); err != nil {
		t.Fatal(err)
	}
	calls := 0
	_, err := Migrate(ctx, pool, migrateOpts(t, Config{PGDump: fakeDump(t, "PGDMP x", 0)}, &calls))
	if !errors.Is(err, db.ErrSchemaNewer) || calls != 0 {
		t.Errorf("err = %v, %d key loads; want ErrSchemaNewer and no backup", err, calls)
	}
}
