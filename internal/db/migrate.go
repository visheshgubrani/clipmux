package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// DefaultProjectID is the hidden project the first migration seeds; it owns
// every object until a multi-tenant edition adds more.
const DefaultProjectID = "prj_default"

// Session settings every connection gets unless DATABASE_URL sets them
// (D068): no statement runs longer than a minute, and no transaction sits
// idle holding its locks for more than five.
var sessionDefaults = map[string]string{
	"statement_timeout":                   "60s",
	"idle_in_transaction_session_timeout": "5min",
}

// OpenWait is how long Open keeps trying to reach Postgres, which may still
// be starting when serve does.
const OpenWait = time.Minute

// Open connects to Postgres, retrying for OpenWait, and checks the
// connection. Pool size and other pgxpool settings come from the URL
// (pool_max_conns=…); session settings in the URL override the defaults.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	return open(ctx, url, OpenWait)
}

// OpenFor is Open with a wait of its own.
func OpenFor(ctx context.Context, url string, wait time.Duration) (*pgxpool.Pool, error) {
	return open(ctx, url, wait)
}

func open(ctx context.Context, url string, wait time.Duration) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		// pgx's error can quote the URL, password included.
		return nil, errors.New("open postgres: DATABASE_URL is not a valid connection string")
	}
	options := cfg.ConnConfig.RuntimeParams["options"]
	for name, value := range sessionDefaults {
		if _, set := cfg.ConnConfig.RuntimeParams[name]; !set && !strings.Contains(options, name) {
			cfg.ConnConfig.RuntimeParams[name] = value
		}
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	deadline := time.Now().Add(wait)
	for delay := time.Second; ; delay = min(delay*2, 10*time.Second) {
		err = pool.Ping(ctx)
		if err == nil {
			return pool, nil
		}
		if ctx.Err() != nil || time.Now().Add(delay).After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(delay):
		}
	}
	pool.Close()
	return nil, fmt.Errorf("ping postgres (tried for %s): %w", wait, err)
}

// ErrSchemaNewer means the database has migrations this binary doesn't
// know: a newer version ran against it. Running would write rows an older
// model doesn't expect, so serve and admin refuse.
var ErrSchemaNewer = errors.New("schema_newer")

func provider(ctx context.Context, pool *pgxpool.Pool) (*goose.Provider, func() error, error) {
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return nil, nil, err
	}
	// Migrations hold a session advisory lock, so two processes (two serve
	// replicas, or serve and clipmux admin) never migrate at once. It is
	// keyed by schema, like the loop leader (D035): installs in separate
	// schemas of one database migrate independently.
	var schema string
	if err := pool.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		return nil, nil, fmt.Errorf("current schema: %w", err)
	}
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockID(MigrationLockID(schema)))
	if err != nil {
		return nil, nil, err
	}
	sqlDB := stdlib.OpenDBFromPool(pool)
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations, goose.WithSessionLocker(locker))
	if err != nil {
		_ = sqlDB.Close()
		return nil, nil, err
	}
	return p, sqlDB.Close, nil
}

// MigrationLockID is the advisory lock migrations of schema hold: "cmx2"
// and the schema's FNV-1a hash.
func MigrationLockID(schema string) int64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(schema)) // a hash's Write never fails
	return int64(0x636d7832)<<32 | int64(h.Sum32())
}

// checkNotNewer refuses a database migrated past this binary.
func checkNotNewer(ctx context.Context, p *goose.Provider) error {
	current, err := p.GetDBVersion(ctx)
	if err != nil {
		return fmt.Errorf("schema version: %w", err)
	}
	sources := p.ListSources()
	known := int64(0)
	if len(sources) > 0 {
		known = sources[len(sources)-1].Version
	}
	if current > known {
		return fmt.Errorf("%w: the database is at migration %d, newer than this binary's %d; run the newer binary, or migrate down with it", ErrSchemaNewer, current, known)
	}
	return nil
}

// Migrate applies every pending migration. It is safe to run on every boot,
// and from several processes at once.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	p, closeDB, err := provider(ctx, pool)
	if err != nil {
		return err
	}
	defer closeDB()
	if err := checkNotNewer(ctx, p); err != nil {
		return err
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// Migration is one migration's state, for clipmux admin migrate status.
type Migration struct {
	Version   int64
	Name      string
	Applied   bool
	AppliedAt time.Time
}

// MigrationStatus lists every migration this binary knows and whether the
// database has it.
func MigrationStatus(ctx context.Context, pool *pgxpool.Pool) ([]Migration, error) {
	p, closeDB, err := provider(ctx, pool)
	if err != nil {
		return nil, err
	}
	defer closeDB()
	st, err := p.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("migrate status: %w", err)
	}
	out := make([]Migration, len(st))
	for i, m := range st {
		out[i] = Migration{
			Version: m.Source.Version, Name: filepath.Base(m.Source.Path),
			Applied: m.State == goose.StateApplied, AppliedAt: m.AppliedAt,
		}
	}
	return out, nil
}

// MigrateDownTo rolls back every migration above version.
func MigrateDownTo(ctx context.Context, pool *pgxpool.Pool, version int64) error {
	p, closeDB, err := provider(ctx, pool)
	if err != nil {
		return err
	}
	defer closeDB()
	if _, err := p.DownTo(ctx, version); err != nil {
		return fmt.Errorf("migrate down to %d: %w", version, err)
	}
	return nil
}

// MigrateDownAll rolls back every migration (tests and disaster drills only).
func MigrateDownAll(ctx context.Context, pool *pgxpool.Pool) error {
	p, closeDB, err := provider(ctx, pool)
	if err != nil {
		return err
	}
	defer closeDB()
	if _, err := p.DownTo(ctx, 0); err != nil {
		return fmt.Errorf("migrate down: %w", err)
	}
	return nil
}
