package retention_test

import (
	"context"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/retention"
)

func TestEachRuleDeletesExactlyWhatIsPastItsWindow(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	count := func(sql string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, sql).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Idempotency keys: 25 h old goes, 1 h old stays.
	exec(`INSERT INTO idempotency_keys (project_id, app_id, key, request_hash, created_at) VALUES
	      ('prj_default', 'app_dashboard', 'old', '\x01', now() - interval '25 hours'),
	      ('prj_default', 'app_dashboard', 'new', '\x02', now() - interval '1 hour')`)
	// Sessions: expired, idle 8 days, and live.
	exec(`INSERT INTO owners (id, project_id, email, password_hash) VALUES ('own_1', 'prj_default', 'o@example.com', 'x')`)
	exec(`INSERT INTO owner_sessions (token_hash, project_id, owner_id, expires_at, last_seen_at) VALUES
	      ('\x01', 'prj_default', 'own_1', now() - interval '1 minute', now()),
	      ('\x02', 'prj_default', 'own_1', now() + interval '20 days', now() - interval '8 days'),
	      ('\x03', 'prj_default', 'own_1', now() + interval '20 days', now())`)
	// Pairings: expired 2 days ago goes, an hour ago stays.
	exec(`INSERT INTO agent_pairings (id, project_id, code_hash, status, expires_at) VALUES
	      ('pair_1', 'prj_default', '\x01', 'pending', now() - interval '2 days'),
	      ('pair_2', 'prj_default', '\x02', 'pending', now() - interval '1 hour')`)
	// Webhooks: an old event with a finished old delivery goes (both); an
	// old event whose delivery is still pending stays; a recent one stays.
	exec(`INSERT INTO webhook_endpoints (id, project_id, app_id, url, secret_encrypted) VALUES ('whe_1', 'prj_default', 'app_dashboard', 'https://x.example/h', '\x00')`)
	exec(`INSERT INTO outbox (id, project_id, kind, payload, done_at) VALUES
	      ('evt_old', 'prj_default', 'webhook', '{}', now() - interval '31 days'),
	      ('evt_waiting', 'prj_default', 'webhook', '{}', now() - interval '31 days'),
	      ('evt_new', 'prj_default', 'webhook', '{}', now() - interval '1 day'),
	      ('obx_old', 'prj_default', 'route_sync', '{}', now() - interval '8 days'),
	      ('obx_new', 'prj_default', 'route_sync', '{}', now() - interval '6 days'),
	      ('obx_stuck', 'prj_default', 'route_sync', '{}', NULL)`)
	exec(`UPDATE outbox SET created_at = now() - interval '2 hours' WHERE id = 'obx_stuck'`)
	exec(`INSERT INTO webhook_deliveries (id, project_id, event_id, endpoint_id, status, give_up_at, finished_at, next_attempt_at) VALUES
	      ('whd_old', 'prj_default', 'evt_old', 'whe_1', 'delivered', now(), now() - interval '31 days', NULL),
	      ('whd_waiting', 'prj_default', 'evt_waiting', 'whe_1', 'pending', now(), NULL, now()),
	      ('whd_new', 'prj_default', 'evt_new', 'whe_1', 'failed', now(), now() - interval '1 day', NULL)`)
	// Setup tests: 22 finished and one running; the newest 20 finished stay.
	for i := range 22 {
		exec(`INSERT INTO setup_tests (id, project_id, status, trigger, created_at, finished_at) VALUES ($1, 'prj_default', 'passed', 'manual', now() - make_interval(mins => $2), now())`,
			"stt_"+string(rune('a'+i)), 100-i)
	}
	exec(`INSERT INTO setup_tests (id, project_id, status, trigger, created_at) VALUES ('stt_running', 'prj_default', 'running', 'manual', now() - interval '1 day')`)

	// Analytics (D105): hourly rows 15 days old go and 13 stay; daily rows
	// and viewers 181 days old go and 179 stay.
	exec(`INSERT INTO playback_hourly (project_id, source, hour, app_id, asset_id, playback_id, views) VALUES
	      ('prj_default', 'origin', date_trunc('hour', now() - interval '15 days', 'UTC'), 'app_a', 'ast_1', 'pid_1', 1),
	      ('prj_default', 'edge', date_trunc('hour', now() - interval '15 days', 'UTC'), 'app_a', 'ast_1', 'pid_1', 1),
	      ('prj_default', 'origin', date_trunc('hour', now() - interval '13 days', 'UTC'), 'app_a', 'ast_1', 'pid_1', 1)`)
	exec(`INSERT INTO playback_daily (project_id, source, day, app_id, asset_id, playback_id, views) VALUES
	      ('prj_default', 'origin', (now() - interval '181 days')::date, 'app_a', 'ast_1', 'pid_1', 1),
	      ('prj_default', 'origin', (now() - interval '179 days')::date, 'app_a', 'ast_1', 'pid_1', 1)`)
	exec(`INSERT INTO playback_viewer_days (project_id, day, app_id, viewer) VALUES
	      ('prj_default', (now() - interval '181 days')::date, 'app_a', '\x000102030405060708090a0b0c0d0e0f'),
	      ('prj_default', (now() - interval '179 days')::date, 'app_a', '\x000102030405060708090a0b0c0d0e0f')`)
	exec(`INSERT INTO playback_viewers (project_id, day, asset_id, viewer, app_id) VALUES
	      ('prj_default', (now() - interval '181 days')::date, 'ast_1', '\x000102030405060708090a0b0c0d0e0f', 'app_a'),
	      ('prj_default', (now() - interval '179 days')::date, 'ast_1', '\x000102030405060708090a0b0c0d0e0f', 'app_a')`)
	exec(`INSERT INTO playback_viewers_hourly (project_id, hour, asset_id, viewer, app_id) VALUES
	      ('prj_default', date_trunc('hour', now() - interval '15 days', 'UTC'), 'ast_1', '\x000102030405060708090a0b0c0d0e0f', 'app_a'),
	      ('prj_default', date_trunc('hour', now() - interval '13 days', 'UTC'), 'ast_1', '\x000102030405060708090a0b0c0d0e0f', 'app_a')`)
	exec(`INSERT INTO playback_viewer_hours (project_id, hour, app_id, viewer) VALUES
	      ('prj_default', date_trunc('hour', now() - interval '15 days', 'UTC'), 'app_a', '\x000102030405060708090a0b0c0d0e0f'),
	      ('prj_default', date_trunc('hour', now() - interval '13 days', 'UTC'), 'app_a', '\x000102030405060708090a0b0c0d0e0f')`)

	r := retention.New(pool)
	dry, err := r.Prune(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := count(`SELECT count(*) FROM idempotency_keys`); got != 2 {
		t.Fatalf("a dry run deleted rows: %d keys left", got)
	}
	rules, err := r.Prune(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{
		"idempotency_keys": 1, "owner_sessions": 2, "agent_pairings": 1, "app_pairings": 0, "webhook_deliveries": 1,
		"webhook_events": 1, "outbox": 1, "setup_tests": 2, "dispatch_keys": 0,
		"playback_hourly": 2, "playback_daily": 1, "playback_viewers": 1, "playback_viewer_days": 1,
		"playback_viewers_hourly": 1, "playback_viewer_hours": 1,
	}
	for i, rule := range rules {
		if rule.Deleted != want[rule.Name] {
			t.Errorf("%s deleted %d, want %d", rule.Name, rule.Deleted, want[rule.Name])
		}
		if dry[i] != rule {
			t.Errorf("dry run said %v, the run did %v", dry[i], rule)
		}
	}
	for sql, n := range map[string]int{
		`SELECT count(*) FROM idempotency_keys WHERE key = 'new'`:                                    1,
		`SELECT count(*) FROM owner_sessions`:                                                        1,
		`SELECT count(*) FROM agent_pairings WHERE id = 'pair_2'`:                                    1,
		`SELECT count(*) FROM outbox WHERE id IN ('evt_waiting', 'evt_new', 'obx_new', 'obx_stuck')`: 4,
		`SELECT count(*) FROM outbox`:                                                                4,
		`SELECT count(*) FROM setup_tests WHERE status = 'passed'`:                                   20,
		`SELECT count(*) FROM setup_tests WHERE id IN ('stt_a', 'stt_b')`:                            0,
		`SELECT count(*) FROM setup_tests WHERE id = 'stt_running'`:                                  1,
		`SELECT count(*) FROM playback_hourly WHERE hour > now() - interval '14 days'`:               1,
		`SELECT count(*) FROM playback_hourly`:                                                       1,
		`SELECT count(*) FROM playback_daily`:                                                        1,
		`SELECT count(*) FROM playback_viewers`:                                                      1,
		`SELECT count(*) FROM playback_viewer_days`:                                                  1,
		`SELECT count(*) FROM playback_viewers_hourly`:                                               1,
		`SELECT count(*) FROM playback_viewer_hours`:                                                 1,
	} {
		if got := count(sql); got != n {
			t.Errorf("%s = %d, want %d", sql, got, n)
		}
	}
	stuck, err := r.Stuck(ctx)
	if err != nil || len(stuck) != 1 || stuck[0].ID != "obx_stuck" {
		t.Fatalf("stuck = %v, %v; want obx_stuck", stuck, err)
	}
	if ok, err := r.RetryNow(ctx, "obx_stuck"); !ok || err != nil {
		t.Fatalf("retry: %v %v", ok, err)
	}
}
