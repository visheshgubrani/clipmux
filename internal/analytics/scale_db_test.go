package analytics

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
)

// TestNinetyDaysOnFiveMillionRows is P7-T03's measurement of the API's
// longest range. It seeds what 180 days of 1,157 playback IDs played every
// hour leaves under the retention windows (D105): 5 million hourly rows'
// worth of history, kept as 208,260 daily rows and the last 14 days of
// hourly ones (388,752), plus 2,082,600 viewer rows from 2,000 recurring
// viewers. It times each 90-day read, warm. Seeding takes about a minute,
// so it runs only with CLIPMUX_ANALYTICS_SCALE=1.
func TestNinetyDaysOnFiveMillionRows(t *testing.T) {
	if os.Getenv("CLIPMUX_ANALYTICS_SCALE") == "" {
		t.Skip("set CLIPMUX_ANALYTICS_SCALE=1 to seed 180 days of history and time the 90-day queries")
	}
	pool := dbtest.New(t)
	ctx := context.Background()
	exec := func(sql string) {
		t.Helper()
		start := time.Now()
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		t.Logf("%.1fs: %.60s", time.Since(start).Seconds(), sql)
	}
	exec(`INSERT INTO apps (id, project_id, name) VALUES ('app_a', 'prj_default', 'a'), ('app_b', 'prj_default', 'b')`)
	exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id, title)
	      SELECT 'ast_' || i, 'prj_default', CASE WHEN i % 2 = 0 THEN 'app_a' ELSE 'app_b' END, 'processing', 'sto_default', 'Lesson ' || i
	      FROM generate_series(1, 1157) i`)
	exec(`INSERT INTO playback_hourly (project_id, source, hour, app_id, asset_id, playback_id, views, watch_seconds, bytes)
	      SELECT 'prj_default', 'origin', h, CASE WHEN i % 2 = 0 THEN 'app_a' ELSE 'app_b' END, 'ast_' || i, 'pid_' || i,
	             i % 7, (i % 7) * 600, (i % 7)::bigint * 400000000
	      FROM generate_series(date_trunc('hour', now(), 'UTC') - interval '335 hours', date_trunc('hour', now(), 'UTC'), interval '1 hour') h,
	           generate_series(1, 1157) i`)
	exec(`INSERT INTO playback_daily (project_id, source, day, app_id, asset_id, playback_id, views, watch_seconds, bytes)
	      SELECT 'prj_default', 'origin', d::date, CASE WHEN i % 2 = 0 THEN 'app_a' ELSE 'app_b' END, 'ast_' || i, 'pid_' || i,
	             24 * (i % 7), 24 * (i % 7) * 600, 24 * (i % 7)::bigint * 400000000
	      FROM generate_series(now() - interval '179 days', now(), interval '1 day') d, generate_series(1, 1157) i`)
	// 180 days x 1,157 assets x 10 of 2,000 viewers.
	exec(`INSERT INTO playback_viewers (project_id, day, asset_id, viewer, app_id)
	      SELECT DISTINCT 'prj_default', d::date, 'ast_' || i,
	             decode(lpad(to_hex((i * 7 + v * 131 + extract(doy FROM d)::int) % 2000), 32, '0'), 'hex'),
	             CASE WHEN i % 2 = 0 THEN 'app_a' ELSE 'app_b' END
	      FROM generate_series(now() - interval '179 days', now(), interval '1 day') d, generate_series(1, 1157) i, generate_series(1, 10) v`)
	exec(`INSERT INTO playback_viewer_days (project_id, day, app_id, viewer)
	      SELECT DISTINCT project_id, day, app_id, viewer FROM playback_viewers`)
	exec(`ANALYZE playback_hourly`)
	exec(`ANALYZE playback_viewers`)
	exec(`ANALYZE playback_daily`)
	exec(`ANALYZE playback_viewer_days`)

	a := NewAPI(pool)
	win, _ := windowOf("90d", time.Now())
	for _, c := range []struct {
		name string
		s    scope
	}{
		{"project", scope{project: "prj_default"}},
		{"one app", scope{project: "prj_default", app: pgtype.Text{String: "app_a", Valid: true}}},
		{"one asset", scope{project: "prj_default", asset: pgtype.Text{String: "ast_2", Valid: true}}},
	} {
		if _, err := a.read(ctx, c.s, win); err != nil { // warm the cache
			t.Fatal(err)
		}
		best := time.Hour
		for range 3 {
			start := time.Now()
			out, err := a.read(ctx, c.s, win)
			if err != nil {
				t.Fatal(err)
			}
			best = min(best, time.Since(start))
			if out.Totals.Views == 0 {
				t.Fatalf("%s: no views read", c.name)
			}
		}
		t.Logf("90d, %s: %v", c.name, best)
		if best > 500*time.Millisecond {
			t.Errorf("90d, %s took %v; over 500 ms, add the daily rollup (P7 plan)", c.name, best)
		}
	}
}
