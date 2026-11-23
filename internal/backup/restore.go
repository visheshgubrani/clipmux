package backup

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"filippo.io/age"
	"github.com/jackc/pgx/v5"

	"github.com/visheshgubrani/clipmux/internal/db"
)

// Source is what a restore brings back: a backup on the target, or a dump
// file on this host.
type Source struct {
	// Manifest names a backup on the target.
	Manifest Manifest
	// File is a dump on this host: an age-encrypted backup copied off the
	// target, which this install's secrets key opens, or a plain custom-format
	// pg_dump.
	File string
}

// Pick finds the backup to restore on the target: the one with the ID, or with
// no ID the newest, whatever its tier or kind. It reads manifests only, so it
// works on a host with no database.
func (s *Service) Pick(ctx context.Context, id string) (Manifest, error) {
	ms, err := s.Manifests(ctx)
	if err != nil {
		return Manifest{}, err
	}
	if len(ms) == 0 {
		return Manifest{}, fmt.Errorf("no backups on %s", s.Target.Desc)
	}
	if id == "" {
		return ms[0], nil
	}
	for _, m := range ms {
		if m.ID == id {
			return m, nil
		}
	}
	return Manifest{}, fmt.Errorf("no backup %s on %s (clipmux admin restore --list shows what is there)", id, s.Target.Desc)
}

// Fetch downloads a backup, checks it against its manifest and decrypts it to
// a file in dir, whose path it returns. The file is the plaintext dump: dir
// must be private and is the caller's to clear.
func (s *Service) Fetch(ctx context.Context, m Manifest, dir string) (string, error) {
	rc, err := s.Target.Get(ctx, m.Object)
	if err != nil {
		return "", fmt.Errorf("read the backup from %s: %w", s.Target.Desc, err)
	}
	defer rc.Close()
	sum := sha256.New()
	var size int64
	tee := io.TeeReader(rc, io.MultiWriter(sum, countWriter{&size}))
	path, err := s.decryptTo(dir, tee, m.SecretsKeyVersion)
	if err != nil {
		return "", err
	}
	// age has read to the end of the object to authenticate it; this is for
	// anything it left, so the hash covers every stored byte.
	if _, err := io.Copy(io.Discard, tee); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("read the backup from %s: %w", s.Target.Desc, err)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != m.SHA256 || size != m.SizeBytes {
		os.Remove(path)
		return "", fmt.Errorf("the backup %s on %s doesn't match its manifest (%d bytes, sha256 %s; the manifest says %d bytes, %s): it was damaged or replaced",
			m.ID, s.Target.Desc, size, got, m.SizeBytes, m.SHA256)
	}
	return path, nil
}

// decrypt opens an age stream with the secrets keys this install holds.
func (s *Service) decrypt(r io.Reader, keyVersion uint32) (io.Reader, error) {
	ids, err := identities(s.Box)
	if err != nil {
		return nil, err
	}
	plain, err := age.Decrypt(r, ids...)
	if err != nil {
		var noMatch *age.NoIdentityMatchError
		if errors.As(err, &noMatch) {
			which := "a secrets key this install doesn't hold"
			if keyVersion != 0 {
				which = fmt.Sprintf("secrets key v%d", keyVersion)
			}
			return nil, fmt.Errorf("none of this install's secrets keys opens the backup, which is encrypted to %s: set CLIPMUX_SECRETS_KEY to the key it was made with (and CLIPMUX_SECRETS_KEY_PREVIOUS for an older one), or open it with the age CLI and your own key", which)
		}
		return nil, fmt.Errorf("decrypt the backup: %w", err)
	}
	return plain, nil
}

// decryptTo decrypts r, an age-encrypted dump, to a new file in dir. keyVersion
// is the secrets key version the backup was encrypted to, for the message when
// no key this install holds opens it; 0 when unknown.
func (s *Service) decryptTo(dir string, r io.Reader, keyVersion uint32) (string, error) {
	plain, err := s.decrypt(r, keyVersion)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "dump-*") // 0600
	if err != nil {
		return "", fmt.Errorf("create the dump file: %w", err)
	}
	if _, err := io.Copy(f, plain); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("decrypt the backup: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("write the dump file: %w", err)
	}
	return f.Name(), nil
}

// openFile makes a dump file on this host ready for pg_restore: an encrypted
// one is decrypted into dir, a plain one is used where it is.
func (s *Service) openFile(path, dir string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open the dump: %w", err)
	}
	defer f.Close()
	head := make([]byte, 24)
	n, _ := io.ReadFull(f, head)
	switch head = head[:n]; {
	case bytes.HasPrefix(head, []byte("age-encryption.org/")):
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return "", fmt.Errorf("read the dump: %w", err)
		}
		return s.decryptTo(dir, f, 0)
	case bytes.HasPrefix(head, []byte("PGDMP")):
		return path, nil
	}
	return "", fmt.Errorf("%s is neither an age-encrypted backup nor a custom-format pg_dump (pg_dump -Fc)", path)
}

// dumpInfo reads what a dump holds: the schema it is of, and the migration
// version that schema was at, from its goose_db_version table.
func (s *Service) dumpInfo(ctx context.Context, file string) (schema string, version int64, err error) {
	restore := s.Config.pgRestoreBin()
	var list bytes.Buffer
	if err := runPG(ctx, restore, "", []string{"--list", file}, nil, &list); err != nil {
		return "", 0, fmt.Errorf("read the dump: %w", err)
	}
	if schema = tableSchema(list.String(), "goose_db_version"); schema == "" {
		return "", 0, errors.New("the dump has no goose_db_version table, so it isn't a ClipMux backup")
	}
	var data bytes.Buffer
	if err := runPG(ctx, restore, "", []string{"--data-only", "--table=goose_db_version", "--file=-", file}, nil, &data); err != nil {
		return "", 0, fmt.Errorf("read the dump's migration version: %w", err)
	}
	if version, err = gooseVersion(data.String()); err != nil {
		return "", 0, err
	}
	return schema, version, nil
}

// tableSchema finds table in the output of pg_restore --list, whose lines are
// "ID; OID OID TABLE schema name owner".
func tableSchema(list, table string) string {
	sc := bufio.NewScanner(strings.NewReader(list))
	for sc.Scan() {
		_, rest, ok := strings.Cut(sc.Text(), "; ")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) >= 5 && f[2] == "TABLE" && f[4] == table {
			return f[3]
		}
	}
	return ""
}

// gooseVersion replays goose_db_version as pg_restore prints it (a COPY block
// of id, version_id, is_applied, tstamp) the way goose does: an applied row
// adds a version, a rolled-back one removes it, and the newest left is the
// schema's.
func gooseVersion(out string) (int64, error) {
	type row struct {
		id, version int64
		applied     bool
	}
	var rows []row
	in := false
	for line := range strings.SplitSeq(out, "\n") {
		switch {
		case strings.HasPrefix(line, "COPY ") && strings.Contains(line, "goose_db_version"):
			in = true
		case line == `\.`:
			in = false
		case in:
			f := strings.Split(line, "\t")
			if len(f) < 3 {
				return 0, fmt.Errorf("the dump's goose_db_version has a row this version can't read: %q", line)
			}
			id, err1 := strconv.ParseInt(f[0], 10, 64)
			v, err2 := strconv.ParseInt(f[1], 10, 64)
			if err1 != nil || err2 != nil {
				return 0, fmt.Errorf("the dump's goose_db_version has a row this version can't read: %q", line)
			}
			rows = append(rows, row{id, v, f[2] == "t"})
		}
	}
	if len(rows) == 0 {
		return 0, errors.New("the dump's goose_db_version is empty, so it isn't a ClipMux backup")
	}
	slices.SortFunc(rows, func(a, b row) int { return cmp.Compare(a.id, b.id) })
	applied := map[int64]bool{}
	for _, r := range rows {
		if r.applied {
			applied[r.version] = true
		} else {
			delete(applied, r.version)
		}
	}
	var latest int64
	for v := range applied {
		latest = max(latest, v)
	}
	return latest, nil
}

// RestoreOptions are how a restore reports and where it works.
type RestoreOptions struct {
	// TempDir holds the decrypted dump while it is restored. It should be on
	// a disk with room for the whole dump; it is created private, and what the
	// restore leaves in it is removed.
	TempDir string
	// Say reports each step as it begins; may be nil.
	Say func(format string, args ...any)
}

// RestoreResult is what a restore did.
type RestoreResult struct {
	// SchemaVersion is the migration version of the restored database, before
	// this binary migrated it further.
	SchemaVersion int64
	// Safety is the backup taken of the database that was replaced, or nil if
	// it was empty.
	Safety *Manifest
}

// Restore replaces the database with a dump. It checks everything it can
// before it changes anything: the dump is downloaded, matched to its manifest
// and decrypted; it must be of this schema and no newer than this binary; and
// the database about to be replaced is backed up (kind pre_restore), so a
// restore is never one-way. Then it clears the schema, in one transaction,
// and loads the dump, in another (pg_restore --single-transaction). Clearing
// first means a schema newer than the dump's has its tables removed, so
// rolling back past a migration leaves nothing behind for the next migration
// to trip over. The two steps aren't one: a load that fails leaves the schema
// empty, the error says so, and the safety backup holds what was there.
//
// The caller makes sure nothing else uses the database while it runs: it
// holds the leader lock (sweeper.HoldLeaderLock), so no serve runs the loops,
// and the migration lock (db.HoldMigrationLock), so none migrates into the
// cleared schema. Afterwards it releases the migration lock, migrates the
// database to this binary, raises the restore fence and starts serve.
func (s *Service) Restore(ctx context.Context, src Source, opt RestoreOptions) (RestoreResult, error) {
	say := opt.Say
	if say == nil {
		say = func(string, ...any) {}
	}
	if err := os.MkdirAll(opt.TempDir, 0o700); err != nil {
		return RestoreResult{}, fmt.Errorf("create %s: %w", opt.TempDir, err)
	}
	work, err := os.MkdirTemp(opt.TempDir, "restore-")
	if err != nil {
		return RestoreResult{}, fmt.Errorf("create a working directory: %w", err)
	}
	defer os.RemoveAll(work)

	var file string
	if src.File != "" {
		say("reading %s", src.File)
		file, err = s.openFile(src.File, work)
	} else {
		say("downloading and decrypting backup %s (%d bytes)", src.Manifest.ID, src.Manifest.SizeBytes)
		file, err = s.Fetch(ctx, src.Manifest, work)
	}
	if err != nil {
		return RestoreResult{}, err
	}

	dumpSchema, version, err := s.dumpInfo(ctx, file)
	if err != nil {
		return RestoreResult{}, err
	}
	if err := db.CheckNotNewer(version); err != nil {
		return RestoreResult{}, fmt.Errorf("refusing to restore this dump: %w", err)
	}
	var schema string
	if err := s.Pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		return RestoreResult{}, fmt.Errorf("read the database's schema: %w", err)
	}
	if dumpSchema != schema {
		return RestoreResult{}, fmt.Errorf("the dump is of schema %q, but DATABASE_URL uses schema %q: restoring would load it where this install doesn't look", dumpSchema, schema)
	}

	res := RestoreResult{SchemaVersion: version}
	has, err := s.hasData(ctx, schema)
	if err != nil {
		return RestoreResult{}, err
	}
	if has {
		say("backing up the current database first (kind pre_restore)")
		b, err := s.Backup(ctx, PreRestore)
		if b.Manifest.ID == "" {
			return RestoreResult{}, fmt.Errorf("the current database couldn't be backed up, so nothing was restored: %w", err)
		}
		if err != nil {
			s.Logger.Warn("the safety backup was taken, but housekeeping after it failed", "err", err)
		}
		res.Safety = &b.Manifest
	} else {
		say("the database is empty: no safety backup needed")
	}

	say("restoring (schema at migration %d)", version)
	if err := s.wipe(ctx, schema); err != nil {
		return res, err
	}
	err = runPG(ctx, s.Config.pgRestoreBin(), s.DatabaseURL,
		[]string{"--clean", "--if-exists", "--single-transaction", "--no-owner", "--no-privileges", file}, nil, io.Discard)
	// The schema was dropped and rebuilt under the pool's connections.
	s.Pool.Reset()
	if err != nil {
		hint := ""
		if res.Safety != nil {
			hint = fmt.Sprintf(" (the database as it was is backup %s)", res.Safety.ID)
		}
		return res, fmt.Errorf("load the dump into the cleared schema; the database is empty%s; run the restore again: %w", hint, err)
	}
	return res, nil
}

// hasData reports whether the schema holds any table.
func (s *Service) hasData(ctx context.Context, schema string) (bool, error) {
	var has bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind IN ('r', 'p'))`, schema).Scan(&has)
	if err != nil {
		return false, fmt.Errorf("look for data in schema %s: %w", schema, err)
	}
	return has, nil
}

// wipe drops the schema and everything in it, and makes it again empty, in one
// transaction. pg_restore --clean drops only what the dump has, which would
// leave behind tables a later migration created.
func (s *Service) wipe(ctx context.Context, schema string) error {
	q := pgx.Identifier{schema}.Sanitize()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("clear schema %s: %w", schema, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) // after a commit it does nothing
	for _, stmt := range []string{"DROP SCHEMA IF EXISTS " + q + " CASCADE", "CREATE SCHEMA " + q} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("clear schema %s: %w", schema, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("clear schema %s: %w", schema, err)
	}
	return nil
}
