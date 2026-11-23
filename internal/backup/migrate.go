package backup

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/secrets"
)

// MigrateOptions are what Migrate needs to back a database up first.
type MigrateOptions struct {
	Config Config
	// Box returns the secrets key the backup is encrypted to. It is called only
	// when a backup is taken, so a database with nothing to migrate needs no
	// key, target or pg_dump.
	Box         func() (*secrets.Box, error)
	DatabaseURL string
	// Version is this build's version, recorded in the manifest.
	Version string
	Logger  *slog.Logger
}

// MigrateResult is what Migrate did.
type MigrateResult struct {
	// Pending is how many migrations it applied.
	Pending int
	// Backup is the backup taken first, nil if none was needed or asked for.
	Backup *Manifest
}

// Migrate applies the pending migrations, after backing up the database as it
// is (kind pre_migrate) when there are some to apply: an upgrade that goes
// wrong can then be rolled back to what it started from. Every path that
// migrates goes through it (serve, and the admin commands); only a restore
// and a rollback to a chosen version don't, since a restore has its own source
// and a rollback is asked for by name.
//
// If the backup fails nothing is migrated, and the error says how to go on
// without one (CLIPMUX_BACKUP_BEFORE_MIGRATE=false). A new database, with no
// migration applied yet, holds nothing to back up.
func Migrate(ctx context.Context, pool *pgxpool.Pool, o MigrateOptions) (MigrateResult, error) {
	logger := o.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	st, err := db.MigrationStatus(ctx, pool)
	if err != nil {
		return MigrateResult{}, err
	}
	res := MigrateResult{}
	applied := 0
	for _, m := range st {
		if m.Applied {
			applied++
		} else {
			res.Pending++
		}
	}
	if res.Pending == 0 || applied == 0 || !o.Config.BeforeMigrate {
		// db.Migrate does the rest, including refusing a database newer than
		// this binary.
		return res, db.Migrate(ctx, pool)
	}

	logger.Info("migrations are pending: backing up the database first", "pending", res.Pending)
	m, err := backUpBeforeMigrating(ctx, pool, o, logger)
	if err != nil {
		return MigrateResult{}, fmt.Errorf("not migrating: the backup before migrating failed: %w; fix the backup target, or set CLIPMUX_BACKUP_BEFORE_MIGRATE=false to migrate without one", err)
	}
	res.Backup = &m.Manifest
	if err := db.Migrate(ctx, pool); err != nil {
		return res, fmt.Errorf("%w (the database as it was is backup %s)", err, m.Manifest.ID)
	}
	// The ledger is written now: a database older than the backups table has
	// it only after migrating.
	if !m.Recorded {
		svc := &Service{Pool: pool}
		if _, err := svc.Record(ctx, m.Manifest); err != nil {
			logger.Warn("the backup before migrating is on the target but not in the database's ledger", "id", m.Manifest.ID, "err", err)
		}
	}
	return res, nil
}

func backUpBeforeMigrating(ctx context.Context, pool *pgxpool.Pool, o MigrateOptions, logger *slog.Logger) (Result, error) {
	box, err := o.Box()
	if err != nil {
		return Result{}, err
	}
	target, err := OpenTarget(o.Config)
	if err != nil {
		return Result{}, err
	}
	defer target.Close()
	svc := New(pool, box, target, o.Config, o.DatabaseURL, o.Version)
	svc.Logger = logger
	res, err := svc.Backup(ctx, PreMigrate)
	if res.Manifest.ID == "" {
		return Result{}, err
	}
	if err != nil {
		// Taken and safe on the target; what followed (pruning) failed.
		logger.Warn("the backup before migrating was taken, but housekeeping after it failed", "err", err)
	}
	logger.Info("backed up before migrating", "id", res.Manifest.ID, "bytes", res.Manifest.SizeBytes, "target", target.Desc)
	return res, nil
}
