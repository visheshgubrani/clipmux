package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Open connects to Postgres and checks the connection.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return pool, nil
}

func provider(pool *pgxpool.Pool) (*goose.Provider, func() error, error) {
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return nil, nil, err
	}
	sqlDB := stdlib.OpenDBFromPool(pool)
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations)
	if err != nil {
		_ = sqlDB.Close()
		return nil, nil, err
	}
	return p, sqlDB.Close, nil
}

// Migrate applies every pending migration. It is safe to run on every boot.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	p, closeDB, err := provider(pool)
	if err != nil {
		return err
	}
	defer closeDB()
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// MigrateDownAll rolls back every migration (tests and disaster drills only).
func MigrateDownAll(ctx context.Context, pool *pgxpool.Pool) error {
	p, closeDB, err := provider(pool)
	if err != nil {
		return err
	}
	defer closeDB()
	if _, err := p.DownTo(ctx, 0); err != nil {
		return fmt.Errorf("migrate down: %w", err)
	}
	return nil
}
