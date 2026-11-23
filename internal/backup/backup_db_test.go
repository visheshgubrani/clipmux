package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
)

type clock struct{ t time.Time }

func (c *clock) Now() time.Time { return c.t }

// fakeDump is a pg_dump that writes a fixed body (or fails), so the layout,
// tiers and retention are tested without a PostgreSQL client.
func fakeDump(t *testing.T, body string, exit int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pg_dump")
	script := "#!/bin/sh\nprintf '%s' '" + body + "'\n"
	if exit != 0 {
		script += "echo 'pg_dump: error: connection refused' >&2\nexit " + string(rune('0'+exit)) + "\n"
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func newService(t *testing.T, pool *pgxpool.Pool, cfg Config, start time.Time) (*Service, *clock) {
	t.Helper()
	if cfg.URL == "" {
		cfg.URL = "file://" + t.TempDir()
	}
	tg, err := OpenTarget(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tg.Close() })
	clk := &clock{start}
	s := New(pool, box(t, 1, 9), tg, cfg, os.Getenv("TEST_DATABASE_URL"), "v-test")
	s.Now = clk.Now
	return s, clk
}

func read(t *testing.T, tg *Target, key string) []byte {
	t.Helper()
	rc, err := tg.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestABackupIsEncryptedHashedRecordedAndListed(t *testing.T) {
	pg := os.Getenv("CLIPMUX_PG_DUMP")
	if pg == "" {
		pg = "pg_dump"
	}
	if _, err := exec.LookPath(pg); err != nil {
		if os.Getenv("CLIPMUX_REQUIRE_MEDIA_TOOLS") == "1" {
			t.Fatalf("pg_dump is required (CLIPMUX_REQUIRE_MEDIA_TOOLS=1): %v", err)
		}
		t.Skip("pg_dump not installed; skipping the real-dump test")
	}
	pool := dbtest.New(t)
	own, _ := age.GenerateX25519Identity()
	s, _ := newService(t, pool, Config{PGDump: pg, Recipients: []string{own.Recipient().String()}, Interval: time.Hour}, time.Now())
	ctx := context.Background()

	res, err := s.Backup(ctx, Manual)
	if err != nil {
		t.Fatal(err)
	}
	m := res.Manifest
	sealed := read(t, s.Target, m.Object)
	if sum := sha256.Sum256(sealed); hex.EncodeToString(sum[:]) != m.SHA256 || int64(len(sealed)) != m.SizeBytes {
		t.Errorf("manifest says %d bytes %s; the object is %d bytes %x", m.SizeBytes, m.SHA256, len(sealed), sum)
	}
	if bytes.Contains(sealed, []byte("PGDMP")) || bytes.Contains(sealed, []byte("install_epoch")) {
		t.Fatal("the dump is on the target in the clear")
	}
	for name, ids := range map[string][]age.Identity{"the install's identity": mustIdentities(t, s.Box), "the owner's own key": {own}} {
		r, err := age.Decrypt(bytes.NewReader(sealed), ids...)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		plain, _ := io.ReadAll(r)
		if !bytes.HasPrefix(plain, []byte("PGDMP")) || !bytes.Contains(plain, []byte("install_epoch")) {
			t.Errorf("%s opened something that isn't a custom-format dump of this schema", name)
		}
	}
	// pg_restore reads what was dumped: the schema's tables, and no other
	// schema's.
	restore := filepath.Join(filepath.Dir(pg), "pg_restore") // the same version as the dump's
	if _, err := os.Stat(restore); err != nil {
		restore = "pg_restore"
	}
	if bin, err := exec.LookPath(restore); err == nil {
		r, _ := age.Decrypt(bytes.NewReader(sealed), mustIdentities(t, s.Box)...)
		cmd := exec.Command(bin, "--list")
		cmd.Stdin = r
		list, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(list), "TABLE "+m.Schema+" install_epoch") {
			t.Errorf("pg_restore --list = %v\n%s", err, list)
		}
	}
	if v, _ := db.SchemaVersion(ctx, pool); m.SchemaVersion != v || m.SchemaVersion == 0 || m.Kind != Manual || m.SecretsKeyVersion != 1 ||
		m.ClipmuxVersion != "v-test" || m.PostgresVersion == "" || !strings.HasPrefix(m.Schema, "t_") {
		t.Errorf("manifest = %+v, want this database's schema version, kind, key version and build", m)
	}
	row, err := db.New(pool).LastBackup(ctx, db.DefaultProjectID)
	if err != nil || row.ID != m.ID || row.Sha256 != m.SHA256 || row.ObjectKey != m.Object {
		t.Errorf("ledger = %+v, %v, want the backup just taken", row, err)
	}
	ms, err := s.Manifests(ctx)
	if err != nil || len(ms) == 0 || ms[0].ID != m.ID {
		t.Fatalf("Manifests = %+v, %v", ms, err)
	}
}

func TestADumpThatFailsPartWayLeavesNothingOnTheTargetOrInTheLedger(t *testing.T) {
	pool := dbtest.New(t)
	s, _ := newService(t, pool, Config{PGDump: fakeDump(t, "PGDMP half a dump", 3)}, time.Now())
	ctx := context.Background()

	_, err := s.Backup(ctx, Manual)
	if err == nil || !strings.Contains(err.Error(), "pg_dump") || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err = %v, want pg_dump's own complaint", err)
	}
	es, lerr := s.entries(ctx)
	if lerr != nil || len(es) != 0 {
		t.Errorf("entries after a failed dump = %+v, %v, want none", es, lerr)
	}
	if _, err := db.New(pool).LastBackup(ctx, db.DefaultProjectID); err == nil {
		t.Error("a failed dump was recorded in the ledger")
	}
}

// Walks the tiers over a month and a half.
func TestBackupsAreCopiedToTheDailyWeeklyAndMonthlyTiersTheyBeginAndPrunedByAge(t *testing.T) {
	pool := dbtest.New(t)
	// Monday 23 November 2026, ISO week 48.
	start := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	s, clk := newService(t, pool, Config{URL: "file://" + t.TempDir(), PGDump: fakeDump(t, "PGDMP x", 0), Interval: time.Hour}, start)
	ctx := context.Background()

	take := func(at time.Time) Result {
		t.Helper()
		clk.t = at
		res, err := s.Backup(ctx, Scheduled)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	for _, c := range []struct {
		at     time.Time
		want   []string
		pruned int
	}{
		{start, []string{"daily", "weekly", "monthly"}, 0},               // first of everything
		{start.Add(time.Hour), nil, 0},                                   // same day
		{start.Add(24 * time.Hour), []string{"daily"}, 0},                // Tuesday: same week, same month
		{start.Add(7 * 24 * time.Hour), []string{"daily", "weekly"}, 3},  // Monday 30 Nov: a new week; the first three are over 3 days old
		{start.Add(8 * 24 * time.Hour), []string{"daily", "monthly"}, 0}, // Tuesday 1 Dec: a new month, the same week
		{start.Add(18 * 24 * time.Hour), []string{"daily", "weekly"}, 2}, // Friday 11 Dec: the 30 Nov and 1 Dec hourlies are over 3 days old
	} {
		res := take(c.at)
		if !slices.Equal(res.Promoted, c.want) || res.Pruned != c.pruned || !res.CanDelete {
			t.Errorf("backup at %s: promoted %v, pruned %d; want %v, %d", c.at.Format(time.RFC3339), res.Promoted, res.Pruned, c.want, c.pruned)
		}
	}

	// A promoted copy has a manifest of its own, naming itself.
	ms, err := s.Manifests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if _, err := s.Target.Get(ctx, m.Object); err != nil {
			t.Errorf("manifest of %s names %s, which isn't there: %v", m.ID, m.Object, err)
		}
	}
	// Left on the target: the daily tier keeps 35 days, so every day, and
	// each week's and month's first.
	count := map[string]int{}
	es, _ := s.entries(ctx)
	for _, e := range es {
		count[e.Tier]++
	}
	if count["hourly"] != 1 || count["daily"] != 5 || count["weekly"] != 3 || count["monthly"] != 2 {
		t.Errorf("tiers = %v, want hourly 1, daily 5, weekly 3, monthly 2", count)
	}

	// Years later, with nothing newer, the newest backup is kept whatever its age.
	clk.t = start.Add(3 * 365 * 24 * time.Hour)
	if _, err := s.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if es, _ := s.entries(ctx); len(es) == 0 {
		t.Fatal("pruning emptied the target")
	}
}

func TestATargetThatMayNotDeleteIsNeverPruned(t *testing.T) {
	pool := dbtest.New(t)
	start := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	s, clk := newService(t, pool, Config{URL: "file://" + t.TempDir(), PGDump: fakeDump(t, "PGDMP x", 0)}, start)
	s.Target.store = noDelete{s.Target.store}
	ctx := context.Background()
	for _, at := range []time.Time{start, start.Add(30 * 24 * time.Hour)} {
		clk.t = at
		res, err := s.Backup(ctx, Scheduled)
		if err != nil || res.CanDelete || res.Pruned != 0 {
			t.Fatalf("Backup = %+v, %v, want one that prunes nothing", res, err)
		}
	}
	hourly := 0
	es, _ := s.entries(ctx)
	for _, e := range es {
		if e.Tier == TierBase {
			hourly++
		}
	}
	if hourly != 2 {
		t.Errorf("hourly backups = %d, want both kept, though one is 30 days old", hourly)
	}
}

func TestTheLoopTakesABackupOnlyWhenOneIsDueAndBacksOffAfterAFailure(t *testing.T) {
	pool := dbtest.New(t)
	start := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	good := fakeDump(t, "PGDMP x", 0)
	s, clk := newService(t, pool, Config{PGDump: good, Interval: time.Hour, URL: "file://" + t.TempDir()}, start)
	ctx := context.Background()
	run := func(after time.Duration) int {
		t.Helper()
		clk.t = start.Add(after)
		n, err := s.Run(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := run(0); n != 1 {
		t.Fatalf("first pass took %d backups, want 1: a new install has none", n)
	}
	if n := run(59 * time.Minute); n != 0 {
		t.Errorf("59 minutes later took %d, want 0", n)
	}
	if n := run(time.Hour); n != 1 {
		t.Errorf("an hour later took %d, want 1 (a pass a minute late doesn't skip a cycle)", n)
	}

	s.Config.PGDump = fakeDump(t, "x", 2)
	clk.t = start.Add(3 * time.Hour)
	if _, err := s.Run(ctx); err == nil {
		t.Fatal("a failing dump was not reported")
	}
	clk.t = start.Add(3*time.Hour + 4*time.Minute)
	if n, err := s.Run(ctx); n != 0 || err != nil {
		t.Errorf("4 minutes after a failure: %d, %v, want it to wait", n, err)
	}
	s.Config.PGDump = good
	clk.t = start.Add(3*time.Hour + 6*time.Minute)
	if n, err := s.Run(ctx); n != 1 || err != nil {
		t.Errorf("6 minutes after a failure: %d, %v, want it to try again", n, err)
	}
	s.Config.Interval = 0
	clk.t = start.Add(30 * time.Hour)
	if n, err := s.Run(ctx); n != 0 || err != nil {
		t.Errorf("with the interval at 0: %d, %v, want no backups", n, err)
	}
}

func TestTheDeadMansSwitchIsPingedAfterASuccessAndItsURLNeverLogged(t *testing.T) {
	pool := dbtest.New(t)
	var hits atomic.Int32
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(status)
	}))
	defer srv.Close()
	var logs bytes.Buffer
	secret := "/ping/0f8fad5b-d9cb-469f-a165-70867728950e"
	s, _ := newService(t, pool, Config{PGDump: fakeDump(t, "PGDMP x", 0), PingURL: srv.URL + secret}, time.Now())
	s.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	ctx := context.Background()
	if _, err := s.Backup(ctx, Scheduled); err != nil || hits.Load() != 1 {
		t.Fatalf("after a backup: err %v, %d pings, want 1", err, hits.Load())
	}
	if _, err := s.Backup(ctx, PreMigrate); err != nil || hits.Load() != 1 {
		t.Fatalf("a safety backup isn't the schedule: err %v, %d pings, want still 1", err, hits.Load())
	}
	status = http.StatusInternalServerError
	if _, err := s.Backup(ctx, Scheduled); err != nil {
		t.Fatalf("a failing ping must not fail the backup: %v", err)
	}
	if !strings.Contains(logs.String(), "the backup ping failed") || strings.Contains(logs.String(), "0f8fad5b") {
		t.Errorf("log = %q, want the failure without the URL", logs.String())
	}
}

func TestTheOwnersRequestTakesAManualBackupAtOnceAndAFailureIsKeptUntilTheNextSuccess(t *testing.T) {
	pool := dbtest.New(t)
	start := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	good := fakeDump(t, "PGDMP x", 0)
	s, clk := newService(t, pool, Config{PGDump: good, Interval: 24 * time.Hour, URL: "file://" + t.TempDir()}, start)
	ctx := context.Background()
	q := db.New(pool)
	state := func() db.GetBackupStateRow {
		t.Helper()
		row, err := q.GetBackupState(ctx, db.DefaultProjectID)
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	if n, err := s.Run(ctx); n != 1 || err != nil {
		t.Fatalf("first pass = %d, %v", n, err)
	}
	// Not due for a day; the owner asks for one.
	clk.t = start.Add(10 * time.Minute)
	if n, _ := s.Run(ctx); n != 0 {
		t.Fatalf("took a backup 10 minutes after the last with nothing asked")
	}
	if err := q.RequestBackup(ctx, db.RequestBackupParams{ID: db.DefaultProjectID, RequestedAt: pgtype.Timestamptz{Time: clk.t, Valid: true}}); err != nil {
		t.Fatal(err)
	}
	clk.t = start.Add(11 * time.Minute)
	if n, err := s.Run(ctx); n != 1 || err != nil {
		t.Fatalf("pass after the request = %d, %v, want a backup", n, err)
	}
	if last, _ := q.LastBackup(ctx, db.DefaultProjectID); last.Kind != Manual {
		t.Errorf("the requested backup is kind %q, want manual", last.Kind)
	}
	// A request is answered once.
	clk.t = start.Add(12 * time.Minute)
	if n, _ := s.Run(ctx); n != 0 {
		t.Error("the same request took a second backup")
	}

	// A request that fails is reported on the project, and a success clears it.
	s.Config.PGDump = fakeDump(t, "x", 2)
	if err := q.RequestBackup(ctx, db.RequestBackupParams{ID: db.DefaultProjectID, RequestedAt: pgtype.Timestamptz{Time: clk.t, Valid: true}}); err != nil {
		t.Fatal(err)
	}
	clk.t = start.Add(13 * time.Minute)
	if _, err := s.Run(ctx); err == nil {
		t.Fatal("a failing dump was not reported")
	}
	if st := state(); !st.BackupError.Valid || !strings.Contains(st.BackupError.String, "dump") || !st.BackupErrorAt.Valid {
		t.Fatalf("state after a failure = %+v, want the error kept", st)
	}
	s.Config.PGDump = good
	clk.t = start.Add(20 * time.Minute)
	if n, err := s.Run(ctx); n != 1 || err != nil {
		t.Fatalf("retry = %d, %v", n, err)
	}
	if st := state(); st.BackupError.Valid || st.BackupErrorAt.Valid {
		t.Errorf("state after a success = %+v, want the error cleared", st)
	}
}
