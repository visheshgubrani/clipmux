package db_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
)

// schemaURL is TEST_DATABASE_URL pointed at pool's schema, so db.Open
// connects the way serve does.
func schemaURL(t *testing.T, pool *pgxpool.Pool, extra string) string {
	t.Helper()
	var schema string
	if err := pool.QueryRow(context.Background(), "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	url := os.Getenv("TEST_DATABASE_URL")
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	return url + sep + "search_path=" + schema + extra
}

func TestMigrationsTakeALockSoConcurrentRunsBothSucceed(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	if err := db.MigrateDownAll(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() { errs[i] = db.Migrate(ctx, pool) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("migration %d: %v", i, err)
		}
	}
	st, err := db.MigrationStatus(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range st {
		if !m.Applied {
			t.Fatalf("%s not applied", m.Name)
		}
	}
	if len(st) < 20 || !strings.HasPrefix(st[0].Name, "00001_") {
		t.Fatalf("status lists %d migrations, first %q", len(st), st[0].Name)
	}
}

func TestADatabaseNewerThanTheBinaryIsRefused(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO goose_db_version (version_id, is_applied) VALUES (99999, true)`); err != nil {
		t.Fatal(err)
	}
	err := db.Migrate(ctx, pool)
	if !errors.Is(err, db.ErrSchemaNewer) || !strings.Contains(err.Error(), "migration 99999") {
		t.Fatalf("err = %v, want schema_newer naming 99999", err)
	}
}

func TestSessionsGetTimeoutsTheURLCanOverride(t *testing.T) {
	ctx := context.Background()
	base := dbtest.New(t)
	pool, err := db.Open(ctx, schemaURL(t, base, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	show := func(p *pgxpool.Pool, name string) string {
		var v string
		if err := p.QueryRow(ctx, "SHOW "+name).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if got := show(pool, "statement_timeout"); got != "1min" {
		t.Errorf("statement_timeout = %q, want 1min", got)
	}
	if got := show(pool, "idle_in_transaction_session_timeout"); got != "5min" {
		t.Errorf("idle_in_transaction_session_timeout = %q, want 5min", got)
	}

	short, err := db.Open(ctx, schemaURL(t, base, "&statement_timeout=1s"))
	if err != nil {
		t.Fatal(err)
	}
	defer short.Close()
	if got := show(short, "statement_timeout"); got != "1s" {
		t.Fatalf("the URL's statement_timeout = %q, want 1s", got)
	}
	_, err = short.Exec(ctx, "SELECT pg_sleep(2)")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("a statement past its timeout: %v, want 57014 query_canceled", err)
	}
}

func TestOpenGivesUpAfterItsWaitWithoutTheURL(t *testing.T) {
	start := time.Now()
	_, err := db.OpenFor(context.Background(), "postgres://clipmux:hunter2@127.0.0.1:1/clipmux", 2*time.Second)
	if err == nil {
		t.Fatal("connected to nothing")
	}
	if took := time.Since(start); took < 500*time.Millisecond || took > 6*time.Second {
		t.Fatalf("gave up after %s, want within its 2 s wait", took)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("the error carries the password: %v", err)
	}
	if _, err := db.Open(context.Background(), "postgres://a:hunter2@%zz"); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("a bad URL: %v", err)
	}
}
