package assets_test

import (
	"context"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/assets"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
)

// counting counts the statements a db.Queries sends.
type counting struct {
	pool *pgxpool.Pool
	n    atomic.Int32
}

func (c *counting) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	c.n.Add(1)
	return c.pool.Exec(ctx, sql, args...)
}
func (c *counting) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.n.Add(1)
	return c.pool.Query(ctx, sql, args...)
}
func (c *counting) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c.n.Add(1)
	return c.pool.QueryRow(ctx, sql, args...)
}

func TestAPageOfAssetsLoadsInTwoQueries(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	for i := range 100 {
		id := fmt.Sprintf("ast_%03d", i)
		if _, err := pool.Exec(ctx, `INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ($1, 'prj_default', 'app_dashboard', 'processing', 'sto_default')`, id); err != nil {
			t.Fatal(err)
		}
		for j := range i % 3 { // none, one or two playback IDs
			if _, err := pool.Exec(ctx, `INSERT INTO playback_ids (id, project_id, asset_id, policy) VALUES ($1, 'prj_default', $2, 'signed')`, fmt.Sprintf("pid_%03d_%d", i, j), id); err != nil {
				t.Fatal(err)
			}
		}
	}
	rows, err := pool.Query(ctx, `SELECT * FROM assets ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	page, err := pgx.CollectRows(rows, pgx.RowToStructByPos[db.Asset])
	if err != nil {
		t.Fatal(err)
	}
	c := &counting{pool: pool}
	views, err := assets.LoadMany(ctx, db.New(c), page)
	if err != nil {
		t.Fatal(err)
	}
	// The media URL, every playback ID, every track (D082).
	if n := c.n.Load(); n != 3 {
		t.Fatalf("listing 100 assets took %d queries, want 3", n)
	}
	for i, a := range page {
		one, err := assets.Load(ctx, db.New(pool), a)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(views[i], one) {
			t.Fatalf("asset %s: LoadMany %+v, Load %+v", a.ID, views[i], one)
		}
	}
}
