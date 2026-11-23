package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/backup"
	"github.com/visheshgubrani/clipmux/internal/config"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/fence"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
	"github.com/visheshgubrani/clipmux/internal/lifecycle"
	"github.com/visheshgubrani/clipmux/internal/secrets"
	"github.com/visheshgubrani/clipmux/internal/sweeper"
)

// backupCmd runs clipmux admin backup now, list, check, verify, identity,
// restore-sources and prune-sources (D143, D146).
func backupCmd(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	usage := func() int { fmt.Fprint(stderr, adminUsage); return 2 }
	if len(args) == 0 || !strings.Contains(" now list check verify identity restore-sources prune-sources ", " "+args[0]+" ") {
		return usage()
	}
	if len(args) > 1 && args[0] != "verify" && args[0] != "restore-sources" && args[0] != "prune-sources" {
		return usage()
	}
	var verifyID string
	if args[0] == "verify" {
		fs := flag.NewFlagSet("backup verify", flag.ContinueOnError)
		fs.SetOutput(stderr)
		fs.StringVar(&verifyID, "id", "", "verify this backup instead of the newest one")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() > 0 {
			return usage()
		}
	}
	fail := func(err error) int { fmt.Fprintf(stderr, "clipmux admin: %v\n", err); return 1 }
	cfg, err := backup.FromEnv(getenv, orDefault(getenv("CLIPMUX_DATA_DIR"), config.DefaultDataDir))
	if err != nil {
		return fail(err)
	}
	// Ctrl-C stops a dump and aborts its upload.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if args[0] == "identity" {
		box, err := loadBox(getenv)
		if err != nil {
			return fail(err)
		}
		return printIdentities(box, stdout, stderr)
	}
	target, err := backup.OpenTarget(cfg)
	if err != nil {
		return fail(err)
	}
	defer target.Close()
	svc := backup.New(nil, nil, target, cfg, getenv("DATABASE_URL"), version)

	switch args[0] {
	case "list":
		ms, err := svc.Manifests(ctx)
		if err != nil {
			return fail(err)
		}
		writeBackupList(stdout, ms)
		return 0
	case "check":
		if svc.Box, err = loadBox(getenv); err != nil {
			return fail(err)
		}
		return checkTarget(ctx, svc, stdout, stderr)
	case "verify":
		return verifyBackup(ctx, svc, verifyID, getenv, stdout, stderr)
	case "restore-sources":
		return restoreSourcesCmd(ctx, svc, args[1:], getenv, stdout, stderr)
	case "prune-sources":
		return pruneSourcesCmd(ctx, svc, args[1:], getenv, stdout, stderr)
	}

	// now: no migration first. A backup made before an upgrade should be of
	// the database as it is.
	box, err := loadBox(getenv)
	if err != nil {
		return fail(err)
	}
	pool, err := db.Open(ctx, getenv("DATABASE_URL"))
	if err != nil {
		return fail(err)
	}
	defer pool.Close()
	svc.Pool, svc.Box = pool, box
	res, err := svc.Backup(ctx, backup.Manual)
	if res.Manifest.ID == "" {
		return fail(err)
	}
	m := res.Manifest
	fmt.Fprintf(stdout, "backup %s written to %s%s\n  %s, sha256 %s\n", m.ID, target.Desc, m.Object, formatBytes(m.SizeBytes), m.SHA256)
	if len(res.Promoted) > 0 {
		fmt.Fprintf(stdout, "  also copied to: %s\n", strings.Join(res.Promoted, ", "))
	}
	switch {
	case res.CanDelete && res.Pruned > 0:
		fmt.Fprintf(stdout, "  pruned %d backups past their retention\n", res.Pruned)
	case !res.CanDelete:
		fmt.Fprintln(stdout, "  this target is immutable to this server (its key can't delete, or its bucket has Object Lock), so nothing is pruned: expire old backups with the bucket's lifecycle rules")
	}
	if !res.Recorded {
		fmt.Fprintln(stdout, "  not in the database's backups table yet: it has no such table until it is migrated")
	}
	if err != nil {
		fmt.Fprintf(stderr, "clipmux admin: the backup was taken, but: %v\n", err)
		return 1
	}
	return 0
}

// printIdentities writes the age identities that open this install's
// backups, in the form age-keygen does, so `age -d -i` takes them as a file.
// They are secrets: whoever holds one can read every backup it opens.
func printIdentities(box *secrets.Box, stdout, stderr io.Writer) int {
	ids, err := backup.Identities(box)
	if err != nil {
		fmt.Fprintf(stderr, "clipmux admin: %v\n", err)
		return 1
	}
	for _, id := range ids {
		fmt.Fprintf(stdout, "# secrets key version: %d\n# public key: %s\n%s\n", id.Version, id.Recipient, id.Secret)
	}
	fmt.Fprintln(stderr, "These identities open every backup. Keep them as you keep the secrets key.")
	return 0
}

func writeBackupList(w io.Writer, ms []backup.Manifest) {
	if len(ms) == 0 {
		fmt.Fprintln(w, "no backups on the target")
		return
	}
	fmt.Fprintf(w, "%-30s %-20s %-12s %10s %7s  %s\n", "ID", "TAKEN (UTC)", "KIND", "SIZE", "SCHEMA", "TIERS")
	seen := map[string]int{}
	var order []backup.Manifest
	tiers := map[string][]string{}
	for _, m := range ms {
		if seen[m.ID]++; seen[m.ID] == 1 {
			order = append(order, m)
		}
		parts := strings.Split(m.Object, "/")
		tiers[m.ID] = append(tiers[m.ID], parts[1])
	}
	for _, m := range order {
		fmt.Fprintf(w, "%-30s %-20s %-12s %10s %7d  %s\n", m.ID, m.CreatedAt.UTC().Format("2006-01-02 15:04:05"), m.Kind, formatBytes(m.SizeBytes), m.SchemaVersion, strings.Join(tiers[m.ID], ", "))
	}
}

// checkTarget probes the target and reports what the owner needs to know to
// trust it. It exits 1 when the target doesn't work.
func checkTarget(ctx context.Context, svc *backup.Service, stdout, stderr io.Writer) int {
	probe, err := svc.Target.Probe(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "clipmux admin: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "target: %s\n  writes, reads and lists: ok\n", svc.Target.Desc)
	switch {
	case probe.Locked:
		fmt.Fprintln(stdout, "  object lock: on, with a default retention; no key can remove a backup before it expires. ClipMux prunes nothing: the bucket's lifecycle rules expire old backups")
	case !probe.CanDelete:
		fmt.Fprintln(stdout, "  deletes: refused; the target is immutable to this server. Expire old backups with the bucket's lifecycle rules")
	default:
		fmt.Fprintln(stdout, "  deletes: allowed, and the bucket has no object lock; ClipMux prunes old backups itself, and so could anyone holding this key. With object lock and lifecycle rules, a compromised server can't erase its backups")
	}
	if probe.LockErr != nil {
		fmt.Fprintf(stdout, "  object lock: couldn't be read (%v); give the key permission to read the bucket's retention settings (B2: readBucketRetentions) so ClipMux can see it\n", probe.LockErr)
	}
	if svc.Config.Local {
		fmt.Fprintln(stdout, "  warning: this is the built-in directory on the server's own disk, which a lost disk takes with it. Set CLIPMUX_BACKUP_URL to a bucket elsewhere")
	}
	n := 1 + len(svc.Config.Recipients)
	fmt.Fprintf(stdout, "encryption: age, to this install's identity and %d other recipient(s) (CLIPMUX_BACKUP_RECIPIENTS)\n", n-1)
	if svc.Config.Interval == 0 {
		fmt.Fprintln(stdout, "schedule: off (CLIPMUX_BACKUP_INTERVAL=0)")
	} else {
		fmt.Fprintf(stdout, "schedule: every %s\n", svc.Config.Interval)
	}
	latest, ok, err := svc.Latest(ctx)
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "clipmux admin: %v\n", err)
		return 1
	case !ok:
		fmt.Fprintln(stdout, "latest backup: none yet")
	default:
		fmt.Fprintf(stdout, "latest backup: %s ago (%s, %s)\n", time.Since(latest.Stamp).Round(time.Second), latest.Key, formatBytes(latest.Size))
	}
	return 0
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// restoreBackupCmd runs clipmux admin restore with a backup to restore, or
// --list (D143, D144). The order is the point: everything that can refuse
// does so before the database is touched, the database about to be replaced is
// backed up, and the restore fence goes up before serve can run again.
func restoreBackupCmd(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	usage := func() int { fmt.Fprint(stderr, adminUsage); return 2 }
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	list := fs.Bool("list", false, "list the backups on the target")
	latest := fs.Bool("latest", false, "restore the newest backup")
	id := fs.String("id", "", "restore the backup with this ID")
	file := fs.String("file", "", "restore a dump file on this host")
	yes := fs.Bool("yes", false, "replace the database")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return usage()
	}
	chosen := 0
	for _, set := range []bool{*latest, *id != "", *file != ""} {
		if set {
			chosen++
		}
	}
	if (*list && (chosen > 0 || *yes)) || (!*list && chosen != 1) {
		return usage()
	}
	fail := func(err error) int { fmt.Fprintf(stderr, "clipmux admin: %v\n", err); return 1 }
	dataDir := orDefault(getenv("CLIPMUX_DATA_DIR"), config.DefaultDataDir)
	cfg, err := backup.FromEnv(getenv, dataDir)
	if err != nil {
		return fail(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	target, err := backup.OpenTarget(cfg)
	if err != nil {
		return fail(err)
	}
	defer target.Close()
	svc := backup.New(nil, nil, target, cfg, getenv("DATABASE_URL"), version)

	if *list {
		ms, err := svc.Manifests(ctx)
		if err != nil {
			return fail(err)
		}
		writeBackupList(stdout, ms)
		return 0
	}
	if svc.Box, err = loadBox(getenv); err != nil {
		return fail(err)
	}
	src := backup.Source{File: *file}
	if *file == "" {
		if src.Manifest, err = svc.Pick(ctx, *id); err != nil {
			return fail(err)
		}
	}
	m := src.Manifest
	if *file == "" {
		fmt.Fprintf(stdout, "backup %s: %s, taken %s, schema at migration %d, ClipMux %s\n", m.ID, m.Kind, m.CreatedAt.UTC().Format(time.RFC3339), m.SchemaVersion, m.ClipmuxVersion)
	} else {
		fmt.Fprintf(stdout, "dump file %s\n", *file)
	}
	if !*yes {
		fmt.Fprintln(stderr, "This replaces the database with that backup, and everything written since is lost from it.\nThe current database is backed up first, and the restore fence goes up after, so storage keeps\nwhat the backup doesn't know. Stop serve first, then pass --yes.")
		return 2
	}

	pool, err := db.Open(ctx, getenv("DATABASE_URL"))
	if err != nil {
		return fail(err)
	}
	defer pool.Close()
	// No migration first: the database may be newer than this binary (a
	// rollback after a bad upgrade), or empty (a new host).
	release, held, err := sweeper.HoldLeaderLock(ctx, pool)
	switch {
	case err != nil:
		return fail(err)
	case !held:
		return fail(fmt.Errorf("a serve is running against this database (it holds the leader lock); stop it first, for example docker compose stop app"))
	}
	defer release()
	svc.Pool = pool
	// And the migration lock, while the schema is replaced: a serve that boots
	// meanwhile (a restart policy, a stray `up`) waits to migrate rather than
	// creating a fresh schema in the moment between the old one going and the
	// dump loading. It is released before migrating, which takes it again.
	unlockMigrations, held, err := db.HoldMigrationLock(ctx, pool)
	switch {
	case err != nil:
		return fail(err)
	case !held:
		return fail(fmt.Errorf("something is migrating this database right now (a serve starting, or another admin command); wait for it, stop it, and run this again"))
	}
	res, err := svc.Restore(ctx, src, backup.RestoreOptions{
		TempDir: filepath.Join(dataDir, "restore-tmp"),
		Say:     func(format string, a ...any) { fmt.Fprintf(stdout, "  "+format+"\n", a...) },
	})
	unlockMigrations()
	if err != nil {
		return fail(err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		return fail(fmt.Errorf("the database is restored, but migrating it to this binary failed: %w", err))
	}
	found := fence.Found{Reason: fence.Restored, Detail: "`clipmux admin restore` replaced the database with an older one"}
	if *file == "" {
		found.BackupID, found.BackupAt = m.ID, m.CreatedAt
	} else {
		found.Detail += " (from " + filepath.Base(*file) + ")"
	}
	raised, err := fence.Engage(ctx, db.New(pool), project, found)
	if err != nil {
		return fail(fmt.Errorf("the database is restored, but the restore fence couldn't be raised; don't start serve until it is (run `clipmux admin restore report` to raise it): %w", err))
	}

	fmt.Fprintf(stdout, "\nrestored: the database is the backup, at migration %d", res.SchemaVersion)
	if newest := db.LatestMigration(); newest != res.SchemaVersion {
		fmt.Fprintf(stdout, ", migrated to %d", newest)
	}
	fmt.Fprintln(stdout, ".")
	if res.Safety != nil {
		fmt.Fprintf(stdout, "The database as it was before is backup %s (clipmux admin restore --id %s undoes this).\n", res.Safety.ID, res.Safety.ID)
	}
	if raised {
		fmt.Fprintln(stdout, "The restore fence is up: serve deletes nothing from storage until you lift it.")
	} else {
		fmt.Fprintln(stdout, "A restore fence was already up, and stays.")
	}
	fmt.Fprint(stdout, `
Next:
  1. Start serve.
  2. clipmux admin restore report   what storage holds that this database doesn't know: the videos
                                    uploaded after the backup. Nothing there is deleted while the fence is up.
  3. clipmux admin restore finish --yes   when you have read it.
`)
	return 0
}

// verifyBackup proves a backup on the target restorable and records the
// result. With no ID it checks the newest backup, like the weekly loop.
func verifyBackup(ctx context.Context, svc *backup.Service, id string, getenv func(string) string, stdout, stderr io.Writer) int {
	fail := func(err error) int { fmt.Fprintf(stderr, "clipmux admin: %v\n", err); return 1 }
	var err error
	if svc.Box, err = loadBox(getenv); err != nil {
		return fail(err)
	}
	var m backup.Manifest
	if id != "" {
		m, err = svc.Pick(ctx, id)
	} else {
		var ok bool
		m, ok, err = svc.LatestManifest(ctx)
		if err == nil && !ok {
			err = fmt.Errorf("no backups on %s to verify", svc.Target.Desc)
		}
	}
	if err != nil {
		return fail(err)
	}
	pool, err := db.Open(ctx, getenv("DATABASE_URL"))
	if err != nil {
		return fail(err)
	}
	defer pool.Close()
	svc.Pool = pool
	svc.TempDir = filepath.Join(orDefault(getenv("CLIPMUX_DATA_DIR"), config.DefaultDataDir), "restore-tmp")
	fmt.Fprintf(stdout, "verifying backup %s, taken %s\n", m.ID, m.CreatedAt.UTC().Format(time.RFC3339))
	res, err := svc.VerifyAndRecord(ctx, m)
	for _, c := range res.Checks {
		fmt.Fprintf(stdout, "  ok: %s\n", c)
	}
	if err != nil {
		return fail(fmt.Errorf("the backup failed verification: %w", err))
	}
	if res.Mode == backup.ModeIntegrity {
		fmt.Fprintln(stdout, "  note: this database role can't create databases, so the backup was proved readable but not restored; run as a role with CREATEDB for the full check")
	}
	fmt.Fprintln(stdout, "the backup can be restored")
	return 0
}

// restoreSourcesCmd copies mirrored sources back into the stores that lost
// them and queues re-encodes through the job path (D146). Run it after the
// database is restored and the stores are reachable again.
func restoreSourcesCmd(ctx context.Context, svc *backup.Service, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup restore-sources", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asset := fs.String("asset", "", "restore one asset")
	all := fs.Bool("all", false, "restore every asset that needs it")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || (*asset == "") == !*all {
		if err == nil {
			fmt.Fprintln(stderr, "clipmux admin: pass exactly one of --asset ID or --all")
		}
		return 2
	}
	fail := func(err error) int { fmt.Fprintf(stderr, "clipmux admin: %v\n", err); return 1 }
	box, err := loadBox(getenv)
	if err != nil {
		return fail(err)
	}
	pool, err := db.Open(ctx, getenv("DATABASE_URL"))
	if err != nil {
		return fail(err)
	}
	defer pool.Close()
	set, fsStore, err := openStores(pool, getenv)
	if err != nil {
		return fail(err)
	}
	defer fsStore.Close()
	svc.Pool, svc.Box, svc.Stores = pool, box, set
	life := lifecycle.New(pool, set, nil)
	res, err := svc.RestoreSources(ctx, backup.RestoreSourcesOptions{
		AssetID: *asset,
		Reencode: func(ctx context.Context, projectID, appID, assetID string) (bool, error) {
			_, err := life.Reencode(ctx, apikeys.Key{ProjectID: projectID, AppID: appID}, assetID, lifecycle.ReencodeRequest{}, "")
			var p *httpjson.Problem
			if errors.As(err, &p) && p.Code == "job_active" {
				return false, nil // already being encoded
			}
			return err == nil, err
		},
	})
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "sources restored to their stores: %d\n", len(res.Restored))
	fmt.Fprintf(stdout, "re-encodes queued:                %d\n", len(res.Queued))
	fmt.Fprintf(stdout, "assets with nothing lost:         %d\n", len(res.Present))
	for _, id := range res.Tombstoned {
		fmt.Fprintf(stdout, "  skipped %s: deleted after the backup (it has a tombstone)\n", id)
	}
	for _, id := range res.Missing {
		fmt.Fprintf(stdout, "  %s: no copy on the target and no source on its store; the mirror never reached it\n", id)
	}
	for _, p := range res.Problems {
		fmt.Fprintf(stderr, "  problem: %s\n", p)
	}
	if len(res.Queued) > 0 {
		fmt.Fprintln(stdout, "The re-encodes run through the usual workers; watch them in the dashboard's jobs list.")
	}
	if len(res.Problems) > 0 {
		return 1
	}
	return 0
}

// pruneSourcesCmd deletes the mirrored copies of long-deleted assets, on a
// target whose key may delete.
func pruneSourcesCmd(ctx context.Context, svc *backup.Service, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup prune-sources", flag.ContinueOnError)
	fs.SetOutput(stderr)
	older := fs.Duration("older-than", backup.TombstoneGrace, "prune copies of assets deleted at least this long ago")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return 2
	}
	n, err := svc.PruneSources(ctx, *older)
	switch {
	case errors.Is(err, backup.ErrNotPrunable):
		fmt.Fprintf(stderr, "clipmux admin: %v\n", err)
		return 1
	case err != nil:
		fmt.Fprintf(stderr, "clipmux admin: pruned %d, then: %v\n", n, err)
		return 1
	}
	fmt.Fprintf(stdout, "pruned the copies of %d deleted assets' sources\n", n)
	return 0
}
