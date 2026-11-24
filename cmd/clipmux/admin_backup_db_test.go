package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/visheshgubrani/clipmux/internal/backup"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/sweeper"
)

func TestBackupCommandsTakeListCheckAndRevealTheIdentityThatOpensThem(t *testing.T) {
	pool := dbtest.New(t)
	dir := t.TempDir()
	// A pg_dump that writes a fixed body: the real one is the engine's test.
	fake := filepath.Join(t.TempDir(), "pg_dump")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf 'PGDMP the database'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	getenv := envFor(t, pool, map[string]string{
		"CLIPMUX_DATA_DIR":        dir,
		"CLIPMUX_SECRETS_KEY":     "v1:" + strings.Repeat("a", 64),
		"CLIPMUX_BACKUP_URL":      "file://" + filepath.Join(dir, "elsewhere"),
		"CLIPMUX_PG_DUMP":         fake,
		"CLIPMUX_BACKUP_INTERVAL": "15m",
	})
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := admin(append([]string{"backup"}, args...), getenv, strings.NewReader(""), &out, &errOut)
		return code, out.String(), errOut.String()
	}

	code, out, errOut := run("check")
	for _, want := range []string{"writes, reads and lists: ok", "deletes: allowed", "schedule: every 15m0s", "latest backup: none yet"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Fatalf("check before any backup: exit %d, missing %q\n%s\n%s", code, want, out, errOut)
		}
	}
	if strings.Contains(out, "warning") {
		t.Errorf("a target the owner chose got the built-in directory's warning:\n%s", out)
	}

	code, out, errOut = run("now")
	id := regexp.MustCompile(`backup (bak_\w+) written to file://\S+/elsewhere/?db/hourly/\S+\.dump\.age`).FindStringSubmatch(out)
	if code != 0 || id == nil {
		t.Fatalf("now: exit %d\n%s\n%s", code, out, errOut)
	}
	code, out, errOut = run("list")
	if code != 0 || !strings.Contains(out, id[1]) || !strings.Contains(out, "manual") {
		t.Fatalf("list: exit %d\n%s\n%s", code, out, errOut)
	}
	code, out, _ = run("check")
	if code != 0 || !strings.Contains(out, "latest backup: ") || strings.Contains(out, "none yet") {
		t.Fatalf("check after a backup: exit %d\n%s", code, out)
	}

	// The identity opens the stored backup with age alone, as the docs say.
	code, out, errOut = run("identity")
	if code != 0 || !strings.Contains(out, "# public key: age1") || !strings.Contains(errOut, "Keep them") {
		t.Fatalf("identity: exit %d\n%s\n%s", code, out, errOut)
	}
	ids, err := age.ParseIdentities(strings.NewReader(out))
	if err != nil {
		t.Fatalf("age can't read what identity printed: %v\n%s", err, out)
	}
	var stored string
	_ = filepath.WalkDir(filepath.Join(dir, "elsewhere", "objects", "db", "hourly"), func(p string, _ os.DirEntry, _ error) error {
		if strings.HasSuffix(p, ".dump.age") {
			stored = p
		}
		return nil
	})
	f, err := os.Open(stored)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, err := age.Decrypt(f, ids...)
	if err != nil {
		t.Fatalf("the printed identity doesn't open the backup: %v", err)
	}
	if plain, _ := io.ReadAll(r); string(plain) != "PGDMP the database" {
		t.Errorf("decrypted = %q", plain)
	}
}

func TestBackupCommandsRefuseWhatTheyCannotDo(t *testing.T) {
	pool := dbtest.New(t)
	dir := t.TempDir()
	base := map[string]string{"CLIPMUX_DATA_DIR": dir}
	run := func(extra map[string]string, args ...string) (int, string) {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		for k, v := range extra {
			env[k] = v
		}
		var out, errOut bytes.Buffer
		code := admin(append([]string{"backup"}, args...), envFor(t, pool, env), strings.NewReader(""), &out, &errOut)
		return code, out.String() + errOut.String()
	}
	if code, _ := run(nil); code != 2 {
		t.Errorf("no subcommand: exit %d, want 2", code)
	}
	if code, _ := run(nil, "restore"); code != 2 {
		t.Errorf("an unknown subcommand: exit %d, want 2", code)
	}
	if code, out := run(nil, "identity"); code != 1 || !strings.Contains(out, "no secrets key") {
		t.Errorf("identity without a key: exit %d\n%s", code, out)
	}
	if code, out := run(map[string]string{"CLIPMUX_SECRETS_KEY": "v1:" + strings.Repeat("a", 64), "CLIPMUX_BACKUP_URL": "s3://b/p?endpoint=http://127.0.0.1:1"}, "check"); code != 1 || !strings.Contains(out, "CLIPMUX_BACKUP_ACCESS_KEY_ID") {
		t.Errorf("an s3 target without credentials: exit %d\n%s", code, out)
	}
}

// pgDumpBin is the real pg_dump, with its pg_restore beside it or on the PATH;
// the test skips without them (CLIPMUX_REQUIRE_MEDIA_TOOLS=1 makes that fail).
func pgDumpBin(t *testing.T) string {
	t.Helper()
	pg := os.Getenv("CLIPMUX_PG_DUMP")
	if pg == "" {
		pg = "pg_dump"
	}
	restore := "pg_restore"
	if dir := filepath.Dir(pg); dir != "." {
		restore = filepath.Join(dir, "pg_restore")
	}
	for _, bin := range []string{pg, restore} {
		if _, err := exec.LookPath(bin); err != nil {
			if os.Getenv("CLIPMUX_REQUIRE_MEDIA_TOOLS") == "1" {
				t.Fatalf("%s is required (CLIPMUX_REQUIRE_MEDIA_TOOLS=1): %v", bin, err)
			}
			t.Skipf("%s not installed", bin)
		}
	}
	return pg
}

func TestRestoreReplacesTheDatabaseWithABackupAndRaisesTheFence(t *testing.T) {
	pg := pgDumpBin(t)
	pool := dbtest.New(t)
	ctx := context.Background()
	dir := t.TempDir()
	getenv := envFor(t, pool, map[string]string{
		"CLIPMUX_DATA_DIR":    dir,
		"CLIPMUX_SECRETS_KEY": "v1:" + strings.Repeat("a", 64),
		"CLIPMUX_BACKUP_URL":  "file://" + filepath.Join(dir, "elsewhere"),
		"CLIPMUX_PG_DUMP":     pg,
	})
	run := func(args ...string) (int, string) {
		var out, errOut bytes.Buffer
		code := admin(args, getenv, strings.NewReader(""), &out, &errOut)
		return code, out.String() + errOut.String()
	}
	generation := func() int64 {
		var g int64
		if err := pool.QueryRow(ctx, `SELECT generation FROM install_epoch`).Scan(&g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	setGeneration := func(g int64) {
		if _, err := pool.Exec(ctx, `UPDATE install_epoch SET generation = $1`, g); err != nil {
			t.Fatal(err)
		}
	}

	setGeneration(5)
	code, out := run("backup", "now")
	id := regexp.MustCompile(`backup (bak_\w+) written`).FindStringSubmatch(out)
	if code != 0 || id == nil {
		t.Fatalf("backup now: exit %d\n%s", code, out)
	}
	setGeneration(9) // the database moves on

	// What it refuses, each leaving the database as it is.
	for name, c := range map[string]struct {
		args []string
		want string
		code int
	}{
		"nothing chosen":        {[]string{"restore", "--yes"}, "Usage", 2},
		"two chosen":            {[]string{"restore", "--latest", "--id", id[1], "--yes"}, "Usage", 2},
		"--list with --yes":     {[]string{"restore", "--list", "--yes"}, "Usage", 2},
		"no --yes":              {[]string{"restore", "--latest"}, "Stop serve first", 2},
		"an unknown ID":         {[]string{"restore", "--id", "bak_nope", "--yes"}, "bak_nope", 1},
		"a file that isn't one": {[]string{"restore", "--file", filepath.Join(dir, "nothing"), "--yes"}, "open the dump", 1},
	} {
		if code, out := run(c.args...); code != c.code || !strings.Contains(out, c.want) {
			t.Errorf("%s: exit %d, want %d containing %q\n%s", name, code, c.code, c.want, out)
		}
		if g := generation(); g != 9 {
			t.Fatalf("%s: the database was changed (generation %d)", name, g)
		}
	}
	if code, out := run("restore", "--latest"); !strings.Contains(out, id[1]) || code != 2 {
		t.Errorf("without --yes it should name the backup it would restore:\n%s", out)
	}

	// A running serve holds the leader lock.
	release, held, err := sweeper.HoldLeaderLock(ctx, pool)
	if err != nil || !held {
		t.Fatalf("HoldLeaderLock = %v, %v", held, err)
	}
	if code, out := run("restore", "--latest", "--yes"); code != 1 || !strings.Contains(out, "a serve is running") || generation() != 9 {
		t.Errorf("with a serve running: exit %d, generation %d\n%s", code, generation(), out)
	}
	release()

	code, out = run("restore", "--latest", "--yes")
	if code != 0 {
		t.Fatalf("restore: exit %d\n%s", code, out)
	}
	if g := generation(); g != 5 {
		t.Errorf("generation = %d, want the backup's 5", g)
	}
	safety := regexp.MustCompile(`before is backup (bak_\w+)`).FindStringSubmatch(out)
	for _, want := range []string{"The restore fence is up", "clipmux admin restore report", "restore finish --yes"} {
		if !strings.Contains(out, want) {
			t.Errorf("the output doesn't say %q:\n%s", want, out)
		}
	}
	if safety == nil {
		t.Fatalf("the output names no safety backup:\n%s", out)
	}
	f, err := db.New(pool).GetActiveFence(ctx, project)
	if err != nil || f.Reason != "restored" || f.BackupID.String != id[1] || !f.BackupAt.Valid {
		t.Fatalf("fence = %+v, %v, want one raised by the restore, naming the backup", f, err)
	}
	if left, _ := os.ReadDir(filepath.Join(dir, "restore-tmp")); len(left) != 0 {
		t.Errorf("the restore left %d entries in its temp directory", len(left))
	}
	// The safety backup undoes it; it predates the fence, so the restore raises
	// one again.
	if code, out = run("restore", "--id", safety[1], "--yes"); code != 0 || generation() != 9 || !strings.Contains(out, "The restore fence is up") {
		t.Errorf("restoring the safety backup: exit %d, generation %d\n%s", code, generation(), out)
	}
	// A backup taken while the fence is up brings the fence back with it, and a
	// restore of it doesn't raise a second.
	if _, err := db.New(pool).LiftFence(ctx, project); err != nil {
		t.Fatal(err)
	}
	// The safety backups are now also on the target. Select the original
	// backup by ID so the next test starts from generation 5.
	if code, out = run("restore", "--id", id[1], "--yes"); code != 0 || generation() != 5 {
		t.Fatalf("restore: exit %d, generation %d\n%s", code, generation(), out)
	}
	code, out = run("backup", "now") // fenced now
	fenced := regexp.MustCompile(`backup (bak_\w+) written`).FindStringSubmatch(out)
	if code != 0 || fenced == nil {
		t.Fatalf("backup now: exit %d\n%s", code, out)
	}
	setGeneration(8)
	if code, out = run("restore", "--id", fenced[1], "--yes"); code != 0 || generation() != 5 || !strings.Contains(out, "already up") {
		t.Errorf("restoring a backup taken under a fence: exit %d, generation %d\n%s", code, generation(), out)
	}
	if code, out = run("restore", "--list"); code != 0 || !strings.Contains(out, "pre_restore") || !strings.Contains(out, id[1]) {
		t.Errorf("--list: exit %d\n%s", code, out)
	}
}

// Every admin path that migrates backs the database up first, or refuses.
func TestAdminCommandsBackUpBeforeMigratingOrRefuse(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	if err := db.MigrateDownTo(ctx, pool, db.LatestMigration()-1); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	fake := filepath.Join(t.TempDir(), "pg_dump")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf 'PGDMP the old database'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"CLIPMUX_DATA_DIR":   dir,
		"CLIPMUX_BACKUP_URL": "file://" + filepath.Join(dir, "elsewhere"),
		"CLIPMUX_PG_DUMP":    fake,
	}
	run := func(args ...string) (int, string) {
		var out, errOut bytes.Buffer
		code := admin(args, envFor(t, pool, env), strings.NewReader(""), &out, &errOut)
		return code, out.String() + errOut.String()
	}
	schema := func() int64 { v, _ := db.SchemaVersion(ctx, pool); return v }
	before := db.LatestMigration() - 1

	// No secrets key: the backup can't be encrypted, so nothing migrates, whichever
	// command it was.
	for _, args := range [][]string{{"migrate", "up"}, {"create-key", "--app", "X", "--allow-loopback"}, {"prune"}} {
		if code, out := run(args...); code != 1 || !strings.Contains(out, "not migrating") || !strings.Contains(out, "CLIPMUX_BACKUP_BEFORE_MIGRATE=false") {
			t.Errorf("%v with no key: exit %d, want a refusal\n%s", args, code, out)
		}
		if v := schema(); v != before {
			t.Fatalf("%v migrated the database to %d without a backup", args, v)
		}
	}
	// Reading what is pending changes nothing, so it needs no backup.
	if code, out := run("migrate", "status"); code != 0 || !strings.Contains(out, "1 pending") {
		t.Errorf("migrate status: exit %d\n%s", code, out)
	}

	env["CLIPMUX_SECRETS_KEY"] = "v1:" + strings.Repeat("a", 64)
	code, out := run("migrate", "up")
	if code != 0 || !strings.Contains(out, "backed up before migrating") || schema() != db.LatestMigration() {
		t.Fatalf("migrate up with a key: exit %d, schema %d\n%s", code, schema(), out)
	}
	row, err := db.New(pool).LastBackup(ctx, project)
	if err != nil || row.Kind != "pre_migrate" || row.SchemaVersion != before {
		t.Errorf("ledger = %+v, %v, want the pre_migrate backup at migration %d", row, err, before)
	}
	// Opted out.
	if err := db.MigrateDownTo(ctx, pool, before); err != nil {
		t.Fatal(err)
	}
	env["CLIPMUX_BACKUP_BEFORE_MIGRATE"] = "false"
	delete(env, "CLIPMUX_SECRETS_KEY")
	if code, out := run("migrate", "up"); code != 0 || strings.Contains(out, "backed up") || schema() != db.LatestMigration() {
		t.Errorf("migrate up with the backup turned off: exit %d, schema %d\n%s", code, schema(), out)
	}
}

func TestBackupVerifyRestoresTheNewestBackupAndRecordsTheResult(t *testing.T) {
	pg := pgDumpBin(t)
	pool := dbtest.New(t)
	ctx := context.Background()
	dir := t.TempDir()
	getenv := envFor(t, pool, map[string]string{
		"CLIPMUX_DATA_DIR":    dir,
		"CLIPMUX_SECRETS_KEY": "v1:" + strings.Repeat("a", 64),
		"CLIPMUX_BACKUP_URL":  "file://" + filepath.Join(dir, "elsewhere"),
		"CLIPMUX_PG_DUMP":     pg,
	})
	run := func(args ...string) (int, string) {
		var out, errOut bytes.Buffer
		code := admin(append([]string{"backup"}, args...), getenv, strings.NewReader(""), &out, &errOut)
		return code, out.String() + errOut.String()
	}
	if code, out := run("verify"); code != 1 || !strings.Contains(out, "no backups") {
		t.Fatalf("verify with nothing to verify: exit %d\n%s", code, out)
	}
	code, out := run("now")
	if code != 0 {
		t.Fatalf("backup now: exit %d\n%s", code, out)
	}
	code, out = run("verify")
	for _, want := range []string{"restored into a scratch database", "the secrets key opens every secret", "row counts match the manifest", "the backup can be restored"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Fatalf("verify: exit %d, missing %q\n%s", code, want, out)
		}
	}
	row, err := db.New(pool).LastBackupVerification(ctx, project)
	if err != nil || row.VerifyMode.String != "restore" || row.VerifyError.Valid {
		t.Fatalf("ledger = %+v, %v, want a clean restore verification", row, err)
	}

	// A scheduled backup may land between an installer restore and its
	// verification. The drill must verify the ID that the installer restored.
	if code, out := run("now"); code != 0 {
		t.Fatalf("take another backup: exit %d\n%s", code, out)
	}
	if code, out := run("verify", "--id", row.ID); code != 0 || !strings.Contains(out, "verifying backup "+row.ID+",") {
		t.Fatalf("verify the earlier backup: exit %d\n%s", code, out)
	}
	if code, out := run("verify", "--id", "bak_missing"); code != 1 || !strings.Contains(out, "no backup bak_missing") {
		t.Fatalf("verify an unknown backup: exit %d\n%s", code, out)
	}

	// A different secrets key can't even open it.
	wrong := envFor(t, pool, map[string]string{
		"CLIPMUX_DATA_DIR": dir, "CLIPMUX_SECRETS_KEY": "v1:" + strings.Repeat("b", 64),
		"CLIPMUX_BACKUP_URL": "file://" + filepath.Join(dir, "elsewhere"), "CLIPMUX_PG_DUMP": pg,
	})
	var o, e bytes.Buffer
	if code := admin([]string{"backup", "verify"}, wrong, strings.NewReader(""), &o, &e); code != 1 || !strings.Contains(e.String(), "failed verification") {
		t.Errorf("verify with the wrong key: exit %d\n%s%s", code, o.String(), e.String())
	}
	row, _ = db.New(pool).LastBackupVerification(ctx, project)
	if !row.VerifyError.Valid {
		t.Error("the failure isn't in the ledger")
	}
}

func TestRestoreSourcesCopiesALostSourceBackAndQueuesItsReencode(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	dir := t.TempDir()
	getenv := envFor(t, pool, map[string]string{
		"CLIPMUX_DATA_DIR":    dir,
		"CLIPMUX_SECRETS_KEY": "v1:" + strings.Repeat("a", 64),
		"CLIPMUX_BACKUP_URL":  "file://" + filepath.Join(dir, "elsewhere"),
	})
	run := func(args ...string) (int, string) {
		var out, errOut bytes.Buffer
		code := admin(append([]string{"backup"}, args...), getenv, strings.NewReader(""), &out, &errOut)
		return code, out.String() + errOut.String()
	}
	if code, _ := run("restore-sources"); code != 2 {
		t.Errorf("restore-sources with no --asset or --all: exit %d, want 2", code)
	}
	if code, _ := run("restore-sources", "--asset", "ast_a", "--all"); code != 2 {
		t.Errorf("restore-sources with both: exit %d, want 2", code)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO assets (id, project_id, app_id, status, storage_id, source_uri)
		VALUES ('ast_a', 'prj_default', 'app_dashboard', 'processing', 'sto_default', 'uploads/ast_a/source')`); err != nil {
		t.Fatal(err)
	}
	set, fsStore, err := openStores(pool, getenv)
	if err != nil {
		t.Fatal(err)
	}
	defer fsStore.Close()
	if err := fsStore.Put(ctx, "uploads/ast_a/source", strings.NewReader("the source")); err != nil {
		t.Fatal(err)
	}
	cfg, err := backup.FromEnv(getenv, dir)
	if err != nil {
		t.Fatal(err)
	}
	target, err := backup.OpenTarget(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	box, err := loadBox(getenv)
	if err != nil {
		t.Fatal(err)
	}
	svc := backup.New(pool, box, target, cfg, getenv("DATABASE_URL"), "v-test")
	svc.Stores = set
	if n, err := svc.RunSources(ctx); err != nil || n != 1 {
		t.Fatalf("mirror = %d, %v", n, err)
	}

	// The bucket is lost: the source goes with it.
	if err := fsStore.Delete(ctx, "uploads/ast_a/source"); err != nil {
		t.Fatal(err)
	}
	code, out := run("restore-sources", "--all")
	if code != 0 || !strings.Contains(out, "sources restored to their stores: 1") || !strings.Contains(out, "re-encodes queued:                1") {
		t.Fatalf("restore-sources: exit %d\n%s", code, out)
	}
	rc, err := fsStore.Get(ctx, "uploads/ast_a/source")
	if err != nil {
		t.Fatalf("the source is not back: %v", err)
	}
	if got, _ := io.ReadAll(rc); string(got) != "the source" {
		t.Errorf("source = %q", got)
	}
	rc.Close()
	var kind, status string
	if err := pool.QueryRow(ctx, `SELECT kind, status FROM jobs WHERE asset_id = 'ast_a'`).Scan(&kind, &status); err != nil || kind != "reencode" || status != "queued" {
		t.Fatalf("job = %s %s, %v, want a queued reencode", kind, status, err)
	}

	// Again: the source is there, and its job is running: nothing happens twice.
	code, out = run("restore-sources", "--all")
	if code != 0 || !strings.Contains(out, "sources restored to their stores: 0") || !strings.Contains(out, "re-encodes queued:                0") {
		t.Fatalf("second restore-sources: exit %d\n%s", code, out)
	}

	if code, out := run("prune-sources"); code != 0 || !strings.Contains(out, "pruned the copies of 0 deleted") {
		t.Errorf("prune-sources: exit %d\n%s", code, out)
	}
}
