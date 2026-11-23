package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/ids"
	"github.com/visheshgubrani/clipmux/internal/secrets"
	"github.com/visheshgubrani/clipmux/internal/stores"
)

// Kinds of backup, as backups.kind has them.
const (
	Scheduled  = "scheduled"
	Manual     = "manual"
	PreMigrate = "pre_migrate"
	PreRestore = "pre_restore"
)

// Tiers on the target: db/{tier}/{stamp}-{id}.dump.age beside its .json
// manifest. A backup is written to the base tier ("hourly", whatever the
// interval) and copied to the others when it is the first of its day, week or
// month.
const (
	TierBase    = "hourly"
	TierDaily   = "daily"
	TierWeekly  = "weekly"
	TierMonthly = "monthly"
)

var tiers = []string{TierBase, TierDaily, TierWeekly, TierMonthly}

const stampLayout = "20060102T150405Z"

// retryAfter is how long a failed scheduled backup waits before the next try,
// so a broken target isn't dumped against every minute.
const retryAfter = 5 * time.Minute

// Manifest describes one backup. It is written after the dump, so a dump with
// no manifest is an unfinished one.
type Manifest struct {
	V         int       `json:"v"`
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
	// Object is the dump's key on the target, relative to its prefix.
	Object string `json:"object"`
	// SizeBytes and SHA256 are of the stored (encrypted) object.
	SizeBytes       int64  `json:"size_bytes"`
	SHA256          string `json:"sha256"`
	ClipmuxVersion  string `json:"clipmux_version"`
	SchemaVersion   int64  `json:"schema_version"`
	PostgresVersion string `json:"postgres_version"`
	// Schema is the Postgres schema that was dumped.
	Schema string `json:"schema"`
	// SecretsKeyVersion is the version of the secrets key whose identity the
	// dump is encrypted to.
	SecretsKeyVersion uint32 `json:"secrets_key_version"`
	// RowCounts are the rows of each of CountedTables in the dump, counted in
	// the very snapshot pg_dump reads, so a verification can require them
	// exactly. Absent in a backup taken before they were recorded, and when
	// the snapshot couldn't be taken.
	RowCounts map[string]int64 `json:"row_counts,omitempty"`
}

// CountedTables are the tables whose rows a backup counts and a verification
// compares: the ones whose loss would be a loss of the library.
var CountedTables = []string{
	"projects", "apps", "api_keys", "signing_keys", "storage_configs",
	"assets", "outputs", "playback_ids", "content_key_secrets", "owners",
}

// Service takes backups of one install's database.
type Service struct {
	Pool        *pgxpool.Pool
	Box         *secrets.Box
	Target      *Target
	Config      Config
	DatabaseURL string
	// Version is this build's version, recorded in each manifest.
	Version string
	Logger  *slog.Logger
	Now     func() time.Time
	HTTP    *http.Client
	// TempDir holds the decrypted dump while a verification reads it; it
	// should be on a disk with room for it (default: the system's).
	TempDir string
	// Stores holds the asset sources the mirror copies and restores; nil
	// leaves the source mirror off.
	Stores *stores.Set

	failedAt        time.Time
	verifyFailedAt  time.Time
	sourceFailed    map[string]time.Time
	sourcesPrunedAt time.Time
	// canDelete is what the target's probe found: 0 not yet, 1 ClipMux
	// prunes it (Probe.Prunes), 2 it is immutable to this server: its key
	// can't delete or its bucket is locked. The dashboard reads it while the
	// loop writes.
	canDelete *atomic.Int32
}

// New returns a Service for the default project.
func New(pool *pgxpool.Pool, box *secrets.Box, target *Target, cfg Config, databaseURL, version string) *Service {
	return &Service{
		Pool: pool, Box: box, Target: target, Config: cfg, DatabaseURL: databaseURL, Version: version,
		Logger: slog.New(slog.DiscardHandler), Now: time.Now, canDelete: new(atomic.Int32),
		HTTP: &http.Client{Timeout: 15 * time.Second},
	}
}

// Result is what one backup did.
type Result struct {
	Manifest Manifest
	// Promoted names the tiers the backup was also copied to.
	Promoted []string
	// Pruned counts the backups deleted by the retention rules.
	Pruned int
	// CanDelete is whether ClipMux prunes this target itself (Probe.Prunes):
	// false when its key can't delete or its bucket is locked.
	CanDelete bool
	// Recorded is false when the database has no backups table yet.
	Recorded bool
}

// Backup dumps the database to the target. It encrypts as it streams, so no
// plaintext reaches the target or the disk, records the backup, copies it to
// the tiers it begins, prunes what retention allows and, for a scheduled or
// manual backup, pings the dead man's switch.
func (s *Service) Backup(ctx context.Context, kind string) (Result, error) {
	probe, err := s.Target.Probe(ctx)
	if err != nil {
		return Result{}, err
	}
	s.noteProbe(probe)
	recips, err := recipients(s.Box, s.Config.Recipients)
	if err != nil {
		return Result{}, err
	}
	now := s.Now().UTC().Truncate(time.Second)
	m := Manifest{V: 1, ID: ids.New(ids.Backup), Kind: kind, CreatedAt: now, ClipmuxVersion: s.Version, SecretsKeyVersion: s.Box.Version()}
	m.Object = objectKey(TierBase, now, m.ID)
	if err := s.Pool.QueryRow(ctx, `SELECT current_schema(), current_setting('server_version')`).Scan(&m.Schema, &m.PostgresVersion); err != nil {
		return Result{}, fmt.Errorf("read the database's schema and version: %w", err)
	}
	if m.SchemaVersion, err = db.SchemaVersion(ctx, s.Pool); err != nil {
		return Result{}, err
	}
	snapshot, counts, release := s.snapshot(ctx)
	defer release()
	m.RowCounts = counts
	if err := s.writeDump(ctx, &m, recips, snapshot); err != nil {
		return Result{}, err
	}
	if err := s.writeManifest(ctx, m); err != nil {
		if probe.CanDelete {
			_ = s.Target.Delete(context.WithoutCancel(ctx), m.Object)
		}
		return Result{}, err
	}
	recorded, err := s.Record(ctx, m)
	if err != nil {
		return Result{}, err
	}
	res := Result{Manifest: m, CanDelete: probe.Prunes(), Recorded: recorded}
	// The backup is safe on the target; what follows is housekeeping, whose
	// failure is reported but doesn't unmake it.
	var errs []error
	if kind == Scheduled || kind == Manual {
		if res.Promoted, err = s.promote(ctx, m); err != nil {
			errs = append(errs, err)
		}
	}
	if probe.Prunes() {
		if res.Pruned, err = s.Prune(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if kind == Scheduled || kind == Manual {
		s.ping(ctx)
	}
	return res, errors.Join(errs...)
}

// Record writes the backup's row in the ledger. It reports false, with no
// error, when the database has no backups table yet: a database older than this
// binary, backed up before it is migrated. The target's manifests are the record
// that matters, and Migrate records the row once the table exists.
func (s *Service) Record(ctx context.Context, m Manifest) (bool, error) {
	err := db.New(s.Pool).InsertBackup(ctx, db.InsertBackupParams{
		ID: m.ID, ProjectID: db.DefaultProjectID, Kind: m.Kind, ObjectKey: m.Object, SizeBytes: m.SizeBytes, Sha256: m.SHA256,
		ClipmuxVersion: m.ClipmuxVersion, SchemaVersion: m.SchemaVersion, PostgresVersion: m.PostgresVersion,
		SecretsKeyVersion: int32(m.SecretsKeyVersion), CreatedAt: pgtype.Timestamptz{Time: m.CreatedAt, Valid: true},
	})
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &pgErr) && pgErr.Code == "42P01": // undefined_table
		return false, nil
	}
	return false, fmt.Errorf("record the backup: %w", err)
}

func objectKey(tier string, at time.Time, id string) string {
	return "db/" + tier + "/" + at.UTC().Format(stampLayout) + "-" + id + ".dump.age"
}

func manifestKey(object string) string { return strings.TrimSuffix(object, ".dump.age") + ".json" }

// writeDump streams pg_dump through age into the target, hashing what is
// stored. A dump that fails part way aborts the upload, so no truncated
// object is ever left looking whole.
func (s *Service) writeDump(ctx context.Context, m *Manifest, recips []age.Recipient, snapshot string) error {
	pr, pw := io.Pipe()
	dumpCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		enc, err := age.Encrypt(pw, recips...)
		if err == nil {
			err = s.runDump(dumpCtx, enc, s.DatabaseURL, m.Schema, snapshot)
		}
		if err == nil {
			err = enc.Close()
		}
		pw.CloseWithError(err) // nil closes it cleanly
		done <- err
	}()
	sum := sha256.New()
	var size int64
	putErr := s.Target.Put(ctx, m.Object, io.TeeReader(pr, io.MultiWriter(sum, countWriter{&size})))
	cancel()
	pr.CloseWithError(putErr) // stop a dump nobody is reading
	dumpErr := <-done
	switch {
	case dumpErr != nil && (putErr == nil || errors.Is(putErr, dumpErr)):
		// The dump failed on its own; the upload only saw its error.
		return fmt.Errorf("dump the database: %w", dumpErr)
	case putErr != nil:
		return fmt.Errorf("write the dump to %s: %w", s.Target.Desc, putErr)
	}
	m.SizeBytes, m.SHA256 = size, hex.EncodeToString(sum.Sum(nil))
	return nil
}

// snapshot exports a snapshot of the database and counts CountedTables in it,
// for pg_dump to read the same one (--snapshot), so the counts are exactly
// what the dump holds. release ends it. If the snapshot can't be taken the
// backup goes ahead without counts: they are a check on the backup, not the
// backup.
func (s *Service) snapshot(ctx context.Context) (id string, counts map[string]int64, release func()) {
	none := func() {}
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		s.Logger.Warn("no snapshot for the backup's row counts", "err", err)
		return "", nil, none
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		conn.Release()
		s.Logger.Warn("no snapshot for the backup's row counts", "err", err)
		return "", nil, none
	}
	release = func() {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		conn.Release()
	}
	counts = map[string]int64{}
	// The session's idle-in-transaction timeout (D068) would end this
	// transaction while pg_dump, which holds nothing of ours, is still reading
	// its snapshot.
	if _, err = tx.Exec(ctx, `SET LOCAL idle_in_transaction_session_timeout = 0`); err == nil {
		err = tx.QueryRow(ctx, `SELECT pg_export_snapshot()`).Scan(&id)
	}
	for _, table := range CountedTables {
		if err != nil {
			break
		}
		var n int64
		if err = tx.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{table}.Sanitize()).Scan(&n); err == nil {
			counts[table] = n
		}
	}
	if err != nil {
		release()
		s.Logger.Warn("no snapshot for the backup's row counts", "err", err)
		return "", nil, none
	}
	return id, counts, release
}

type countWriter struct{ n *int64 }

func (c countWriter) Write(p []byte) (int, error) { *c.n += int64(len(p)); return len(p), nil }

func (s *Service) writeManifest(ctx context.Context, m Manifest) error {
	body, _ := json.MarshalIndent(m, "", "  ") // plain struct; can't fail
	if err := s.Target.Put(ctx, manifestKey(m.Object), bytes.NewReader(body)); err != nil {
		return fmt.Errorf("write the manifest to %s: %w", s.Target.Desc, err)
	}
	return nil
}

// Entry is one object on the target: a dump of one backup in one tier.
type Entry struct {
	Tier  string
	Key   string
	Stamp time.Time
	Size  int64
	// Manifest is set when the entry's manifest is on the target.
	HasManifest bool
}

// entries lists the dumps on the target, newest first.
func (s *Service) entries(ctx context.Context) ([]Entry, error) {
	var out []Entry
	for _, tier := range tiers {
		objs, err := s.Target.list(ctx, "db/"+tier+"/")
		if err != nil {
			return nil, err
		}
		have := map[string]bool{}
		for _, o := range objs {
			have[o.Key] = true
		}
		for _, o := range objs {
			if !strings.HasSuffix(o.Key, ".dump.age") {
				continue
			}
			name := o.Key[strings.LastIndex(o.Key, "/")+1:]
			stamp, _, _ := strings.Cut(name, "-")
			at, err := time.Parse(stampLayout, stamp)
			if err != nil {
				continue
			}
			out = append(out, Entry{Tier: tier, Key: o.Key, Stamp: at, Size: o.Size, HasManifest: have[manifestKey(o.Key)]})
		}
	}
	slices.SortFunc(out, func(a, b Entry) int { return b.Stamp.Compare(a.Stamp) })
	return out, nil
}

// Latest is the newest backup on the target, whatever its tier; it reads
// listings only, not manifests.
func (s *Service) Latest(ctx context.Context) (Entry, bool, error) {
	es, err := s.entries(ctx)
	if err != nil || len(es) == 0 {
		return Entry{}, false, err
	}
	return es[0], true, nil
}

// promote copies the backup to each tier it is the first of: the day, the ISO
// week or the month, for the tiers the policy keeps.
func (s *Service) promote(ctx context.Context, m Manifest) ([]string, error) {
	pol := s.Config.Policy()
	es, err := s.entries(ctx)
	if err != nil {
		return nil, err
	}
	same := map[string]func(a, b time.Time) bool{
		TierDaily: func(a, b time.Time) bool { return a.Format("20060102") == b.Format("20060102") },
		TierWeekly: func(a, b time.Time) bool {
			ay, aw := a.ISOWeek()
			by, bw := b.ISOWeek()
			return ay == by && aw == bw
		},
		TierMonthly: func(a, b time.Time) bool { return a.Format("200601") == b.Format("200601") },
	}
	keep := map[string]time.Duration{TierDaily: pol.Daily, TierWeekly: pol.Weekly, TierMonthly: pol.Monthly}
	var done []string
	var errs []error
	for _, tier := range []string{TierDaily, TierWeekly, TierMonthly} {
		if keep[tier] == 0 || slices.ContainsFunc(es, func(e Entry) bool { return e.Tier == tier && same[tier](e.Stamp, m.CreatedAt) }) {
			continue
		}
		if err := s.copyTo(ctx, m, tier); err != nil {
			errs = append(errs, fmt.Errorf("copy the backup to %s: %w", tier, err))
			continue
		}
		done = append(done, tier)
	}
	return done, errors.Join(errs...)
}

// copyTo writes the dump and a manifest naming the copy to another tier.
func (s *Service) copyTo(ctx context.Context, m Manifest, tier string) error {
	rc, err := s.Target.Get(ctx, m.Object)
	if err != nil {
		return err
	}
	defer rc.Close()
	c := m
	c.Object = objectKey(tier, m.CreatedAt, m.ID)
	if err := s.Target.Put(ctx, c.Object, rc); err != nil {
		return err
	}
	return s.writeManifest(ctx, c)
}

// Prune deletes the backups past their tier's age, and returns how many. It
// needs a credential that may delete, which Backup checks first; the newest
// backup is never deleted.
func (s *Service) Prune(ctx context.Context) (int, error) {
	pol := s.Config.Policy()
	es, err := s.entries(ctx)
	if err != nil || len(es) == 0 {
		return 0, err
	}
	age := map[string]time.Duration{TierBase: pol.Base, TierDaily: pol.Daily, TierWeekly: pol.Weekly, TierMonthly: pol.Monthly}
	now := s.Now()
	n := 0
	var errs []error
	for _, e := range es[1:] { // newest first: es[0] stays
		if age[e.Tier] == 0 || now.Sub(e.Stamp) <= age[e.Tier] {
			continue
		}
		// The manifest goes first: a dump without one is an unfinished backup,
		// so a failure half way never leaves a manifest naming nothing.
		if err := s.Target.Delete(ctx, manifestKey(e.Key)); err != nil {
			errs = append(errs, fmt.Errorf("prune %s: %w", e.Key, err))
			continue
		}
		if err := s.Target.Delete(ctx, e.Key); err != nil {
			errs = append(errs, fmt.Errorf("prune %s: %w", e.Key, err))
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// ping fetches the dead man's switch URL. A failure is logged, without the
// URL, which is a secret of sorts.
func (s *Service) ping(ctx context.Context) {
	if s.Config.PingURL == "" {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Config.PingURL, nil)
	if err == nil {
		var resp *http.Response
		if resp, err = s.HTTP.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if resp.StatusCode >= 300 {
				err = fmt.Errorf("status %d", resp.StatusCode)
			}
		}
	}
	if err != nil {
		var ue interface{ Unwrap() error }
		if errors.As(err, &ue) && ue.Unwrap() != nil {
			err = ue.Unwrap() // the net error, without the URL url.Error quotes
		}
		s.Logger.Warn("the backup ping failed", "err", err)
	}
}

// Run is the leader loop's pass: it takes a scheduled backup when the last
// one is older than the interval, or a manual one when the owner asked for it
// from the dashboard since the last, and returns how many it took. A failure
// waits retryAfter before the next try, and is kept for the dashboard until a
// backup succeeds.
func (s *Service) Run(ctx context.Context) (int, error) {
	if s.Config.Interval == 0 {
		return 0, nil
	}
	now := s.Now()
	if !s.failedAt.IsZero() && now.Sub(s.failedAt) < retryAfter {
		return 0, nil
	}
	q := db.New(s.Pool)
	last, err := q.LastBackup(ctx, db.DefaultProjectID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("last backup: %w", err)
	}
	kind := Scheduled
	switch state, err := q.GetBackupState(ctx, db.DefaultProjectID); {
	case err != nil:
		return 0, fmt.Errorf("read the backup state: %w", err)
	case state.BackupRequestedAt.Valid && (last.CreatedAt.Time.IsZero() || state.BackupRequestedAt.Time.After(last.CreatedAt.Time)):
		kind = Manual
	case last.CreatedAt.Time.IsZero() || now.Sub(last.CreatedAt.Time) >= s.Config.Interval-30*time.Second:
	default:
		return 0, nil
	}
	res, err := s.Backup(ctx, kind)
	if res.Manifest.ID == "" {
		s.failedAt = now
		s.recordFailure(ctx, err)
		return 0, err
	}
	s.failedAt = time.Time{}
	if cerr := q.SetBackupError(ctx, db.SetBackupErrorParams{ID: db.DefaultProjectID}); cerr != nil {
		s.Logger.Warn("clear the backup error", "err", cerr)
	}
	s.Logger.Info("backup taken", "id", res.Manifest.ID, "kind", kind, "bytes", res.Manifest.SizeBytes, "target", s.Target.Desc,
		"promoted", res.Promoted, "pruned", res.Pruned, "can_delete", res.CanDelete)
	return 1, err
}

// recordFailure keeps why the newest backup failed, for the dashboard.
func (s *Service) recordFailure(ctx context.Context, err error) {
	if err == nil || ctx.Err() != nil {
		return
	}
	msg := err.Error()
	if len(msg) > 1000 {
		msg = msg[:1000]
	}
	if rerr := db.New(s.Pool).SetBackupError(context.WithoutCancel(ctx), db.SetBackupErrorParams{ID: db.DefaultProjectID, Message: pgtype.Text{String: msg, Valid: true}}); rerr != nil {
		s.Logger.Warn("record the backup error", "err", rerr)
	}
}

// Immutable reports whether the target is known to be immutable to this
// server (its key can't delete, or its bucket has Object Lock), and false,
// false when it hasn't been probed yet in this process.
func (s *Service) Immutable() (immutable, known bool) {
	switch s.canDelete.Load() {
	case 1:
		return false, true
	case 2:
		return true, true
	}
	return false, false
}

func (s *Service) noteProbe(p Probe) {
	if p.Prunes() {
		s.canDelete.Store(1)
	} else {
		s.canDelete.Store(2)
	}
}

// Manifests reads every backup's manifest from the target, newest first, one
// per copy: a backup copied to the daily tier is listed there too. It is what
// a restore chooses from on a host with no database.
func (s *Service) Manifests(ctx context.Context) ([]Manifest, error) {
	es, err := s.entries(ctx)
	if err != nil {
		return nil, err
	}
	var out []Manifest
	for _, e := range es {
		if !e.HasManifest {
			continue
		}
		m, err := s.readManifest(ctx, e.Key)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// readManifest reads the manifest beside the dump at key.
func (s *Service) readManifest(ctx context.Context, key string) (Manifest, error) {
	rc, err := s.Target.Get(ctx, manifestKey(key))
	if err != nil {
		return Manifest{}, fmt.Errorf("read the manifest of %s: %w", key, err)
	}
	var m Manifest
	err = json.NewDecoder(rc).Decode(&m)
	rc.Close()
	if err != nil || m.V != 1 || m.ID == "" {
		return Manifest{}, fmt.Errorf("the manifest of %s isn't one this version reads", key)
	}
	return m, nil
}

// LatestManifest is the manifest of the newest finished backup on the target,
// and false when there is none. It reads one manifest, not all of them.
func (s *Service) LatestManifest(ctx context.Context) (Manifest, bool, error) {
	es, err := s.entries(ctx)
	if err != nil {
		return Manifest{}, false, err
	}
	for _, e := range es {
		if e.HasManifest {
			m, err := s.readManifest(ctx, e.Key)
			return m, err == nil, err
		}
	}
	return Manifest{}, false, nil
}
