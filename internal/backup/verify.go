package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/secrets"
)

// How often the latest backup is proved restorable, and how soon a failed
// check is tried again.
const (
	VerifyInterval = 7 * 24 * time.Hour
	VerifyRetry    = 6 * time.Hour
)

// How far a verification went, as backups.verify_mode has it.
const (
	// ModeRestore loaded the dump into a scratch database and read it.
	ModeRestore = "restore"
	// ModeIntegrity proved the file only: what a role that can't create
	// databases allows.
	ModeIntegrity = "integrity"
)

// scratchPrefix begins the name of every database a verification restores into;
// an install's own are scratchPrefix, a token of its database and schema, and
// the backup, so a server shared by several installs, or by several verifying
// at once, never has one drop another's.
const scratchPrefix = "clipmux_verify_"

// VerifyResult is what a verification found.
type VerifyResult struct {
	Manifest Manifest
	// Mode is how far it went; on a failure, how far it got.
	Mode string
	// Checks lists what passed, in order.
	Checks []string
}

// Verify proves a backup can be restored, as a restore would: it downloads
// it, checks its SHA-256 against the manifest, decrypts it, and has pg_restore
// read it. Where the role may create databases (the compose superuser does) it
// then restores the dump into a scratch database, and checks that this install's
// secrets key opens the secrets in it, that every content key it needs loads,
// and that each counted table has exactly the rows the manifest recorded. The
// scratch database is dropped afterwards. Nothing in the live database changes.
func (s *Service) Verify(ctx context.Context, m Manifest) (VerifyResult, error) {
	res := VerifyResult{Manifest: m, Mode: ModeIntegrity}
	pass := func(format string, a ...any) { res.Checks = append(res.Checks, fmt.Sprintf(format, a...)) }

	tmp := s.TempDir
	if tmp == "" {
		tmp = os.TempDir()
	}
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return res, fmt.Errorf("create %s: %w", tmp, err)
	}
	work, err := os.MkdirTemp(tmp, "verify-")
	if err != nil {
		return res, fmt.Errorf("create a working directory: %w", err)
	}
	defer os.RemoveAll(work)

	file, err := s.Fetch(ctx, m, work)
	if err != nil {
		return res, err
	}
	pass("downloaded, SHA-256 matches the manifest (%d bytes), decrypted with this install's secrets key", m.SizeBytes)
	schema, version, err := s.dumpInfo(ctx, file)
	if err != nil {
		return res, err
	}
	if version != m.SchemaVersion {
		return res, fmt.Errorf("the manifest says migration %d, the dump holds migration %d", m.SchemaVersion, version)
	}
	if err := db.CheckNotNewer(version); err != nil {
		return res, fmt.Errorf("this binary couldn't restore the backup: %w", err)
	}
	pass("pg_restore reads it: schema %s at migration %d", schema, version)

	can, err := s.canCreateDB(ctx)
	if err != nil {
		return res, err
	}
	if !can {
		return res, nil
	}
	if err := s.verifyRestore(ctx, m, file, schema, work, &res, pass); err != nil {
		return res, err
	}
	return res, nil
}

func (s *Service) canCreateDB(ctx context.Context) (bool, error) {
	var can bool
	if err := s.Pool.QueryRow(ctx, `SELECT rolcreatedb OR rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&can); err != nil {
		return false, fmt.Errorf("read the role's privileges: %w", err)
	}
	return can, nil
}

// scratchURL is the database URL pointed at another database of the same
// server, with the schema the dump holds as the search path.
func scratchURL(databaseURL, dbname, schema string) (string, error) {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return "", errors.New("DATABASE_URL is not a URL")
	}
	u.Path = "/" + dbname
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (s *Service) verifyRestore(ctx context.Context, m Manifest, file, schema, work string, res *VerifyResult, pass func(string, ...any)) (err error) {
	res.Mode = ModeRestore
	var token string
	if err := s.Pool.QueryRow(ctx, `SELECT substr(md5(current_database() || '/' || current_schema()), 1, 12)`).Scan(&token); err != nil {
		return fmt.Errorf("name the scratch database: %w", err)
	}
	own := scratchPrefix + token + "_"
	id := strings.ToLower(strings.TrimPrefix(m.ID, "bak_"))
	name := own + id[max(0, len(id)-12):]
	dropAll := func(ctx context.Context) error {
		rows, err := s.Pool.Query(ctx, `SELECT datname FROM pg_database WHERE datname LIKE $1`, strings.ReplaceAll(own, "_", `\_`)+"%")
		if err != nil {
			return fmt.Errorf("list scratch databases: %w", err)
		}
		names, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("list scratch databases: %w", err)
		}
		var errs []error
		for _, n := range names {
			if _, err := s.Pool.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{n}.Sanitize()+" WITH (FORCE)"); err != nil {
				errs = append(errs, fmt.Errorf("drop scratch database %s: %w", n, err))
			}
		}
		return errors.Join(errs...)
	}
	// A verification that was killed leaves its database behind; this install
	// runs one at a time, so any of its own that is there is such a leftover.
	if err := dropAll(ctx); err != nil {
		return err
	}
	if _, err := s.Pool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return fmt.Errorf("create the scratch database: %w", err)
	}
	defer func() {
		if derr := dropAll(context.WithoutCancel(ctx)); derr != nil {
			err = errors.Join(err, derr)
		}
	}()

	conn, err := scratchURL(s.DatabaseURL, name, schema)
	if err != nil {
		return err
	}
	// --clean --if-exists as a real restore has them: a dump of the public
	// schema creates it, and a new database already has one.
	if err := runPG(ctx, s.Config.pgRestoreBin(), conn,
		[]string{"--clean", "--if-exists", "--no-owner", "--no-privileges", "--single-transaction", file}, nil, io.Discard); err != nil {
		return fmt.Errorf("restore into a scratch database: %w", err)
	}
	pass("restored into a scratch database")

	pool, err := db.Open(ctx, conn)
	if err != nil {
		return fmt.Errorf("open the scratch database: %w", err)
	}
	defer pool.Close()
	// Counted before anything else touches the scratch database: loading the
	// content keys makes one when there are none.
	if len(m.RowCounts) == 0 {
		pass("no row counts in the manifest to compare (a backup from before they were recorded)")
	}
	var wrong []string
	for _, table := range slices.Sorted(func(yield func(string) bool) {
		for t := range m.RowCounts {
			if !yield(t) {
				return
			}
		}
	}) {
		var n int64
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{table}.Sanitize()).Scan(&n); err != nil {
			return fmt.Errorf("count %s in the scratch database: %w", table, err)
		}
		if n != m.RowCounts[table] {
			wrong = append(wrong, fmt.Sprintf("%s has %d rows, the manifest recorded %d", table, n, m.RowCounts[table]))
		}
	}
	if len(wrong) > 0 {
		return fmt.Errorf("the restored rows don't match the manifest: %s", strings.Join(wrong, "; "))
	}
	if len(m.RowCounts) > 0 {
		pass("row counts match the manifest (%d tables)", len(m.RowCounts))
	}
	if err := secrets.Verify(ctx, db.New(pool), db.DefaultProjectID, s.Box); err != nil {
		return fmt.Errorf("this install's secrets key doesn't open the backup's secrets: %w", err)
	}
	if err := secrets.CheckVersions(ctx, pool, s.Box); err != nil {
		return fmt.Errorf("this install's secrets key doesn't open the backup's secrets: %w", err)
	}
	pass("the secrets key opens every secret in it")
	if _, err := contentkey.Load(ctx, pool, s.Box, db.DefaultProjectID, work); err != nil {
		return fmt.Errorf("the backup's content keys: %w", err)
	}
	pass("every content key version it needs loads")
	return nil
}

// VerifyAndRecord verifies a backup and writes the result to the ledger:
// verified_at and the mode, and verify_error when it failed. The ledger row is
// made first if the database has none, as after a restore of an older database.
// It returns the verification's own error.
func (s *Service) VerifyAndRecord(ctx context.Context, m Manifest) (VerifyResult, error) {
	res, verr := s.Verify(ctx, m)
	if ctx.Err() != nil {
		return res, verr // stopped, not failed: nothing to record
	}
	p := db.RecordBackupVerificationParams{ProjectID: db.DefaultProjectID, ID: m.ID, VerifiedAt: pgtype.Timestamptz{Time: s.Now(), Valid: true}, VerifyMode: pgtype.Text{String: res.Mode, Valid: true}}
	if verr != nil {
		msg := verr.Error()
		if len(msg) > 2000 {
			msg = msg[:2000]
		}
		p.VerifyError = pgtype.Text{String: msg, Valid: true}
	}
	q := db.New(s.Pool)
	n, err := q.RecordBackupVerification(ctx, p)
	if err == nil && n == 0 {
		if _, err = s.Record(ctx, m); err == nil {
			_, err = q.RecordBackupVerification(ctx, p)
		}
	}
	if err != nil {
		return res, errors.Join(verr, fmt.Errorf("record the verification: %w", err))
	}
	return res, verr
}

// RunVerify is the leader loop's pass: it verifies the newest backup when the
// last check is older than VerifyInterval, or than VerifyRetry after a failed
// one, and returns how many it verified. A failed verification is returned as
// the pass's error, and recorded.
func (s *Service) RunVerify(ctx context.Context) (int, error) {
	now := s.Now()
	if !s.verifyFailedAt.IsZero() && now.Sub(s.verifyFailedAt) < retryAfter {
		return 0, nil
	}
	last, err := db.New(s.Pool).LastBackupVerification(ctx, db.DefaultProjectID)
	switch {
	case err == nil:
		wait := VerifyInterval
		if last.VerifyError.Valid {
			wait = VerifyRetry
		}
		if now.Sub(last.VerifiedAt.Time) < wait {
			return 0, nil
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return 0, fmt.Errorf("last verification: %w", err)
	}
	m, ok, err := s.LatestManifest(ctx)
	if err != nil {
		s.verifyFailedAt = now
		return 0, fmt.Errorf("find the backup to verify: %w", err)
	}
	if !ok {
		return 0, nil
	}
	s.verifyFailedAt = time.Time{}
	res, err := s.VerifyAndRecord(ctx, m)
	if err != nil {
		return 1, fmt.Errorf("backup %s failed verification: %w", m.ID, err)
	}
	s.Logger.Info("backup verified", "id", m.ID, "mode", res.Mode, "checks", len(res.Checks))
	return 1, nil
}
