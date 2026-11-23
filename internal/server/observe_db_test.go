package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
)

func scrape(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("/metrics: %d", rec.Code)
	}
	return rec.Body.String()
}

func TestMetricsCountJobsOutboxAndAttemptsFromPostgres(t *testing.T) {
	pool := dbtest.New(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"ast_1", "ast_2", "ast_3"} {
		exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ($1, 'prj_default', 'app_dashboard', 'processing', 'sto_default')`, id)
	}
	exec(`INSERT INTO jobs (id, project_id, asset_id, kind, routed_to, route_reason) VALUES ('job_1', 'prj_default', 'ast_1', 'transcode', 'workers', 'primary'), ('job_2', 'prj_default', 'ast_2', 'transcode', 'workers', 'primary')`)
	exec(`INSERT INTO jobs (id, project_id, asset_id, kind, status) VALUES ('job_3', 'prj_default', 'ast_3', 'delete', 'succeeded')`)
	exec(`UPDATE jobs SET available_at = now() - interval '90 seconds' WHERE id = 'job_1'`)
	exec(`INSERT INTO outbox (id, project_id, kind, payload, created_at) VALUES ('obx_1', 'prj_default', 'route_sync', '{}', now() - interval '600 seconds')`)
	exec(`INSERT INTO outbox (id, project_id, kind, payload, done_at) VALUES ('obx_2', 'prj_default', 'webhook', '{}', now())`)
	for i, code := range []string{"transcode_failed", "made_up_by_a_worker", "made_up_too"} {
		exec(`INSERT INTO outputs (id, project_id, asset_id, key_version) VALUES ($1, 'prj_default', 'ast_3', 1)`, "out_"+strconv.Itoa(i))
		exec(`INSERT INTO attempts (id, project_id, job_id, asset_id, attempt_number, output_id, executor, status, error_code, token_hash, lease_expires_at)
		      VALUES ($1, 'prj_default', 'job_3', 'ast_3', $2, $3, 'builtin', 'failed', $4, $5, now())`,
			"att_"+strconv.Itoa(i), i+1, "out_"+strconv.Itoa(i), code, []byte{byte(i)})
	}

	out := scrape(t, newObserver(pool, nil, "test").Handler(false))
	for _, want := range []string{
		`clipmux_jobs{kind="transcode",status="queued",routed_to="workers",route_reason="primary"} 2`,
		`clipmux_outbox_pending{kind="route_sync"} 1`,
		`clipmux_attempts_finished_total{executor="builtin",result="failed",error_code="transcode_failed"} 1`,
		`clipmux_attempts_finished_total{executor="builtin",result="failed",error_code="other"} 2`,
		`clipmux_workers_online{kind="agent"} 0`,
		`clipmux_build_info{version="test"} 1`,
		`clipmux_leader 0`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(out, `clipmux_outbox_pending{kind="webhook"}`) || strings.Contains(out, `kind="delete"`) {
		t.Error("done rows and finished jobs must not count as pending or active")
	}
	age := regexp.MustCompile(`clipmux_outbox_oldest_pending_seconds\{kind="route_sync"\} ([0-9.]+)`).FindStringSubmatch(out)
	if age == nil {
		t.Fatal("no outbox age")
	}
	if v, _ := strconv.ParseFloat(age[1], 64); v < 599 || v > 660 {
		t.Errorf("oldest pending route_sync = %v s, want about 600", v)
	}
	oldest := regexp.MustCompile(`clipmux_job_queue_oldest_seconds\{kind="transcode"\} ([0-9.]+)`).FindStringSubmatch(out)
	if oldest == nil {
		t.Fatal("no queue age")
	}
	if v, _ := strconv.ParseFloat(oldest[1], 64); v < 89 || v > 150 {
		t.Errorf("oldest queued transcode = %v s, want about 90", v)
	}
}

func TestRequestsAndLoopsAreCountedAndMetricsStayOffThePublicListener(t *testing.T) {
	obs := newObserver(nil, nil, "test")
	h := requestLog{logger: slog.New(slog.DiscardHandler), obs: obs}.wrap(Handler(Deps{DB: fakeDB{}}))
	for _, p := range []string{"/healthz", "/healthz", "/metrics"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if p == "/metrics" && rec.Code == 200 && strings.Contains(rec.Body.String(), "clipmux_") {
			t.Fatal("/metrics is served on the public listener")
		}
	}
	task := obs.loop("routes", func(context.Context) (int, error) { return 1, nil })
	if _, err := task(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := scrape(t, obs.Handler(false))
	for _, want := range []string{
		`clipmux_http_requests_total{route="GET /healthz",code="200"} 2`,
		`clipmux_http_in_flight 0`,
		`clipmux_sweeper_pass_seconds_count{sweeper="routes"} 1`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %s\n%s", want, out)
		}
	}
	if !regexp.MustCompile(`clipmux_sweeper_last_success_timestamp_seconds\{sweeper="routes"\} \d`).MatchString(out) {
		t.Error("no last success")
	}
	rec := httptest.NewRecorder()
	obs.Handler(false).ServeHTTP(rec, httptest.NewRequest("GET", "/debug/pprof/", nil))
	if rec.Code != 404 {
		t.Fatalf("pprof without CLIPMUX_PPROF: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	obs.Handler(true).ServeHTTP(rec, httptest.NewRequest("GET", "/debug/pprof/", nil))
	if rec.Code != 200 {
		t.Fatalf("pprof with CLIPMUX_PPROF: %d", rec.Code)
	}
}

// The shipped alerts (deploy/prometheus/alerts.yml) name only metrics serve
// exports, so a rename can't silently disarm one.
func TestAlertsUseMetricsServeExports(t *testing.T) {
	rules, err := os.ReadFile(filepath.Join("..", "..", "deploy", "prometheus", "alerts.yml"))
	if err != nil {
		t.Fatal(err)
	}
	exported := map[string]bool{}
	obs := newObserver(nil, nil, "test")
	obs.inFlight.Add(0)
	obs.backups(time.Hour, t.TempDir())
	for _, line := range strings.Split(string(obs.reg.Expose()), "\n") {
		if f := strings.Fields(line); len(f) >= 3 && f[0] == "#" && f[1] == "TYPE" {
			exported[f[2]] = true
		}
	}
	// The Postgres-backed families exist only with a pool; name them here.
	for _, n := range []string{"clipmux_jobs", "clipmux_job_queue_oldest_seconds", "clipmux_attempts_finished_total",
		"clipmux_outbox_pending", "clipmux_outbox_oldest_pending_seconds", "clipmux_webhook_deliveries",
		"clipmux_dispatches", "clipmux_workers_online", "clipmux_db_pool_acquired_conns", "clipmux_db_pool_max_conns",
		"clipmux_backup_last_success_timestamp_seconds", "clipmux_backup_last_verified_timestamp_seconds", "clipmux_backup_verify_failed",
		"clipmux_backup_last_bytes", "clipmux_backup_sources_pending", "clipmux_restore_fenced", "clipmux_db_size_bytes"} {
		exported[n] = true
	}
	for _, m := range regexp.MustCompile(`clipmux_[a-z_]+`).FindAllString(string(rules), -1) {
		// A histogram is exported as its name and used as _bucket, _sum and _count.
		for _, suffix := range []string{"_bucket", "_sum", "_count"} {
			if base := strings.TrimSuffix(m, suffix); base != m && exported[base] {
				m = base
			}
		}
		if !exported[m] {
			t.Errorf("alerts.yml uses %s, which serve doesn't export", m)
		}
	}
}

func TestMetricsReportBackupsSourcesTheFenceAndTheDisk(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	obs := newObserver(pool, nil, "test")
	obs.backups(time.Hour, t.TempDir())

	// Nothing backed up yet: zeros, which an alert reads together with the interval.
	out := scrape(t, obs.Handler(false))
	for _, want := range []string{
		`clipmux_backup_last_success_timestamp_seconds 0`, `clipmux_backup_last_verified_timestamp_seconds 0`,
		`clipmux_backup_verify_failed 0`, `clipmux_backup_last_bytes 0`, `clipmux_backup_sources_pending 0`,
		`clipmux_restore_fenced 0`, `clipmux_backup_interval_seconds 3600`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %s", want)
		}
	}
	free := regexp.MustCompile(`clipmux_data_free_bytes ([0-9.e+]+)`).FindStringSubmatch(out)
	total := regexp.MustCompile(`clipmux_data_total_bytes ([0-9.e+]+)`).FindStringSubmatch(out)
	if free == nil || total == nil {
		t.Fatal("no disk gauges")
	}
	if f, _ := strconv.ParseFloat(free[1], 64); f <= 0 {
		t.Errorf("free bytes = %v, want the temp directory's disk to have some", f)
	}
	if size := regexp.MustCompile(`clipmux_db_size_bytes ([0-9.e+]+)`).FindStringSubmatch(out); size == nil {
		t.Error("no database size")
	}

	// Two backups, the older proved restorable, the newer checked and failed;
	// two sources waiting; the fence up.
	exec(`INSERT INTO backups (id, project_id, kind, object_key, size_bytes, sha256, clipmux_version, schema_version, postgres_version, secrets_key_version, created_at, verified_at, verify_mode)
		VALUES ('bak_old', 'prj_default', 'scheduled', 'db/hourly/a', 111, 'x', 'v', 1, '18', 1, '2026-11-01T10:00:00Z', '2026-11-01T11:00:00Z', 'restore')`)
	exec(`INSERT INTO backups (id, project_id, kind, object_key, size_bytes, sha256, clipmux_version, schema_version, postgres_version, secrets_key_version, created_at, verified_at, verify_mode, verify_error)
		VALUES ('bak_new', 'prj_default', 'scheduled', 'db/hourly/b', 222, 'x', 'v', 1, '18', 1, '2026-11-02T10:00:00Z', '2026-11-02T11:00:00Z', 'restore', 'broken')`)
	for _, id := range []string{"ast_1", "ast_2"} {
		exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id, source_uri) VALUES ($1, 'prj_default', 'app_dashboard', 'processing', 'sto_default', 'uploads/'||$1||'/source')`, id)
	}
	exec(`INSERT INTO restore_fence (project_id, reason) VALUES ('prj_default', 'restored')`)
	time.Sleep(dbCacheFor + 100*time.Millisecond) // the counts are cached for 10 s
	out = scrape(t, obs.Handler(false))
	for _, want := range []string{
		`clipmux_backup_last_success_timestamp_seconds 1.7936136e+09`, `clipmux_backup_last_verified_timestamp_seconds 1.7935308e+09`,
		`clipmux_backup_verify_failed 1`, `clipmux_backup_last_bytes 222`, `clipmux_backup_sources_pending 2`, `clipmux_restore_fenced 1`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %s\n%s", want, grepLines(out, "clipmux_backup", "clipmux_restore"))
		}
	}
}

func grepLines(out string, prefixes ...string) string {
	var keep []string
	for _, l := range strings.Split(out, "\n") {
		for _, p := range prefixes {
			if strings.HasPrefix(l, p) {
				keep = append(keep, l)
			}
		}
	}
	return strings.Join(keep, "\n")
}
