package db_test

// Real-Postgres tests for the core model (docs/architecture.md, "Core model").
// Expected values come from the spec, not from the migration.

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
)

const (
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
	pgCheckViolation      = "23514"
)

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) error {
	t.Helper()
	_, err := pool.Exec(context.Background(), sql, args...)
	return err
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if err := exec(t, pool, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func wantPgCode(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("want Postgres error %s, got %v", code, err)
	}
	if pgErr.Code != code {
		t.Fatalf("want Postgres error %s, got %s (%s)", code, pgErr.Code, pgErr.Message)
	}
}

// seed creates an app, an asset in waiting_upload and returns their IDs.
func seed(t *testing.T, pool *pgxpool.Pool) (appID, assetID string) {
	t.Helper()
	mustExec(t, pool, `INSERT INTO apps (id, project_id, name) VALUES ('app_lms', 'prj_default', 'LMS')`)
	mustExec(t, pool, `INSERT INTO assets (id, project_id, app_id, status, storage_id, passthrough)
		VALUES ('ast_1', 'prj_default', 'app_lms', 'waiting_upload', 'sto_default', 'lesson-42')`)
	return "app_lms", "ast_1"
}

func addOutput(t *testing.T, pool *pgxpool.Pool, id, assetID string) {
	t.Helper()
	mustExec(t, pool, `INSERT INTO outputs (id, project_id, asset_id, key_version) VALUES ($1, 'prj_default', $2, 1)`, id, assetID)
}

func TestFirstBootDefaults(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()

	var project string
	if err := pool.QueryRow(ctx, `SELECT id FROM projects`).Scan(&project); err != nil {
		t.Fatal(err)
	}
	if project != "prj_default" {
		t.Fatalf("default project = %q, want prj_default", project)
	}

	// Dashboard uploads go to a built-in Dashboard app.
	var dashboard string
	if err := pool.QueryRow(ctx, `SELECT id FROM apps WHERE is_builtin`).Scan(&dashboard); err != nil {
		t.Fatal(err)
	}
	if dashboard != "app_dashboard" {
		t.Fatalf("built-in app = %q, want app_dashboard", dashboard)
	}

	// Filesystem storage is active on a fresh install.
	var kind string
	if err := pool.QueryRow(ctx, `SELECT kind FROM storage_configs WHERE is_default`).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != "fs" {
		t.Fatalf("default storage = %q, want fs", kind)
	}
}

func TestEveryRowCarriesProjectID(t *testing.T) {
	pool := dbtest.New(t)
	rows, err := pool.Query(context.Background(), `
		SELECT t.table_name
		FROM information_schema.tables t
		WHERE t.table_schema = current_schema()
		  AND t.table_type = 'BASE TABLE'
		  AND t.table_name NOT IN ('projects', 'goose_db_version')
		  AND NOT EXISTS (
		    SELECT 1 FROM information_schema.columns c
		    WHERE c.table_schema = t.table_schema AND c.table_name = t.table_name
		      AND c.column_name = 'project_id' AND c.is_nullable = 'NO')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		_ = rows.Scan(&name)
		t.Errorf("table %s has no NOT NULL project_id", name)
	}
}

func TestOneActiveMutatingJobPerAsset(t *testing.T) {
	pool := dbtest.New(t)
	_, asset := seed(t, pool)

	mustExec(t, pool, `INSERT INTO jobs (id, project_id, asset_id, kind, routed_to, status) VALUES ('job_1', 'prj_default', $1, 'transcode', 'workers', 'running')`, asset)

	// A move while the transcode runs must be refused (the API turns this into 409).
	err := exec(t, pool, `INSERT INTO jobs (id, project_id, asset_id, kind, status, source_storage_id, target_storage_id) VALUES ('job_2', 'prj_default', $1, 'move', 'queued', 'sto_default', 'sto_default')`, asset)
	wantPgCode(t, err, pgUniqueViolation)

	// Once the first job ends, the next one may queue.
	mustExec(t, pool, `UPDATE jobs SET status = 'succeeded' WHERE id = 'job_1'`)
	mustExec(t, pool, `INSERT INTO jobs (id, project_id, asset_id, kind, routed_to, status) VALUES ('job_2', 'prj_default', $1, 'reencode', 'workers', 'queued')`, asset)
}

// Two families (P5-T13): a transcription runs beside an encode, but never
// beside another track job, a move or a delete.
func TestOneActiveJobPerFamily(t *testing.T) {
	pool := dbtest.New(t)
	_, asset := seed(t, pool)
	mustExec(t, pool, `INSERT INTO tracks (id, project_id, asset_id, kind) VALUES ('trk_1', 'prj_default', $1, 'subtitles')`, asset)
	mustExec(t, pool, `INSERT INTO jobs (id, project_id, asset_id, kind, routed_to, status) VALUES ('job_1', 'prj_default', $1, 'reencode', 'workers', 'running')`, asset)
	mustExec(t, pool, `INSERT INTO jobs (id, project_id, asset_id, kind, routed_to, status, track_id) VALUES ('job_2', 'prj_default', $1, 'transcribe', 'workers', 'queued', 'trk_1')`, asset)

	err := exec(t, pool, `INSERT INTO jobs (id, project_id, asset_id, kind, status, track_id) VALUES ('job_3', 'prj_default', $1, 'chapters', 'queued', 'trk_1')`, asset)
	wantPgCode(t, err, pgUniqueViolation)
	mustExec(t, pool, `UPDATE jobs SET status = 'succeeded' WHERE id = 'job_1'`)
	// The encode ended, but a move still waits for the transcription.
	err = exec(t, pool, `INSERT INTO jobs (id, project_id, asset_id, kind, status, source_storage_id, target_storage_id) VALUES ('job_3', 'prj_default', $1, 'move', 'queued', 'sto_default', 'sto_default')`, asset)
	wantPgCode(t, err, pgUniqueViolation)

	// A track job names its track; a media job doesn't, and a transcription
	// is never pinned.
	wantPgCode(t, exec(t, pool, `INSERT INTO jobs (id, project_id, asset_id, kind, routed_to, status) VALUES ('job_4', 'prj_default', $1, 'transcribe', 'workers', 'failed')`, asset), pgCheckViolation)
	wantPgCode(t, exec(t, pool, `INSERT INTO jobs (id, project_id, asset_id, kind, routed_to, status, track_id) VALUES ('job_4', 'prj_default', $1, 'transcode', 'workers', 'failed', 'trk_1')`, asset), pgCheckViolation)
}

func TestCurrentOutputMustBelongToTheAsset(t *testing.T) {
	pool := dbtest.New(t)
	_, asset := seed(t, pool)
	mustExec(t, pool, `INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_2', 'prj_default', 'app_lms', 'processing', 'sto_default')`)
	addOutput(t, pool, "out_a", asset)
	addOutput(t, pool, "out_b", "ast_2")

	wantPgCode(t, exec(t, pool, `UPDATE assets SET current_output_id = 'out_b' WHERE id = $1`, asset), pgForeignKeyViolation)
	mustExec(t, pool, `UPDATE assets SET current_output_id = 'out_a' WHERE id = $1`, asset)
}

func TestOutputPrefixIsDerivedAndNeverReused(t *testing.T) {
	pool := dbtest.New(t)
	_, asset := seed(t, pool)
	addOutput(t, pool, "out_a", asset)

	var prefix string
	if err := pool.QueryRow(context.Background(), `SELECT prefix FROM outputs WHERE id = 'out_a'`).Scan(&prefix); err != nil {
		t.Fatal(err)
	}
	if prefix != "media/ast_1/out_a/" {
		t.Fatalf("prefix = %q, want media/ast_1/out_a/", prefix)
	}
	wantPgCode(t, exec(t, pool, `UPDATE outputs SET status = 'committed' WHERE id = 'out_a'`), pgCheckViolation) // committed needs a manifest
}

func TestReadyAssetHasAnOutputAndErroredHasACode(t *testing.T) {
	pool := dbtest.New(t)
	_, asset := seed(t, pool)

	wantPgCode(t, exec(t, pool, `UPDATE assets SET status = 'ready' WHERE id = $1`, asset), pgCheckViolation)
	wantPgCode(t, exec(t, pool, `UPDATE assets SET status = 'errored' WHERE id = $1`, asset), pgCheckViolation)
	mustExec(t, pool, `UPDATE assets SET status = 'errored', error_code = 'upload_expired' WHERE id = $1`, asset)
}

func TestAttemptWritesAnOutputOfItsJobsAsset(t *testing.T) {
	pool := dbtest.New(t)
	_, asset := seed(t, pool)
	mustExec(t, pool, `INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_2', 'prj_default', 'app_lms', 'processing', 'sto_default')`)
	mustExec(t, pool, `INSERT INTO jobs (id, project_id, asset_id, kind, routed_to, status) VALUES ('job_1', 'prj_default', $1, 'transcode', 'workers', 'running')`, asset)
	addOutput(t, pool, "out_mine", asset)
	addOutput(t, pool, "out_other", "ast_2")

	insert := `INSERT INTO attempts (id, project_id, job_id, asset_id, attempt_number, output_id, executor, token_hash, lease_expires_at)
		VALUES ($1, 'prj_default', 'job_1', $2, $3, $4, 'builtin', $5, now() + interval '90 seconds')`
	wantPgCode(t, exec(t, pool, insert, "att_x", "ast_1", 1, "out_other", []byte("h1")), pgForeignKeyViolation)
	mustExec(t, pool, insert, "att_1", "ast_1", 1, "out_mine", []byte("h2"))

	// A retry is a new attempt number and needs a new output.
	wantPgCode(t, exec(t, pool, insert, "att_2", "ast_1", 2, "out_mine", []byte("h3")), pgUniqueViolation)
}

func TestOneActiveUploadPerAssetWithinPartLimit(t *testing.T) {
	pool := dbtest.New(t)
	_, asset := seed(t, pool)
	const mib32 = 32 << 20
	insert := `INSERT INTO uploads (id, project_id, asset_id, multipart_id, declared_size, part_size, token_hash)
		VALUES ($1, 'prj_default', $2, 'mp', $3, $4, $5)`

	// 32 MiB parts × 10,000 parts is the ceiling (~312 GiB).
	wantPgCode(t, exec(t, pool, insert, "upl_big", asset, int64(mib32)*10_000+1, mib32, []byte("t0")), pgCheckViolation)
	mustExec(t, pool, insert, "upl_1", asset, int64(4<<30), mib32, []byte("t1"))
	wantPgCode(t, exec(t, pool, insert, "upl_2", asset, int64(4<<30), mib32, []byte("t2")), pgUniqueViolation)
}

func TestStatusesAreConstrained(t *testing.T) {
	pool := dbtest.New(t)
	_, asset := seed(t, pool)
	cases := []struct{ name, sql string }{
		{"asset", `UPDATE assets SET status = 'transcoding' WHERE id = '` + asset + `'`},
		{"job kind", `INSERT INTO jobs (id, project_id, asset_id, kind, status) VALUES ('j', 'prj_default', '` + asset + `', 'thumbnail', 'queued')`},
		{"playback policy", `INSERT INTO playback_ids (id, project_id, asset_id, policy) VALUES ('p', 'prj_default', '` + asset + `', 'private')`},
		{"api key scope", `INSERT INTO api_keys (id, project_id, app_id, name, key_hash, scopes) VALUES ('k', 'prj_default', 'app_lms', 'k', 'h', '{apps:write}')`},
		{"agent scope", `INSERT INTO agents (id, project_id, name, key_hash, scope) VALUES ('agt', 'prj_default', 'pc', 'h', 'everything')`},
		{"pairing consumed without an agent", `INSERT INTO agent_pairings (id, project_id, code_hash, status, expires_at) VALUES ('pair', 'prj_default', 'h', 'consumed', now())`},
		{"app signing key holding a private key", `INSERT INTO signing_keys (id, project_id, app_id, public_key_pem, private_key_encrypted) VALUES ('kid', 'prj_default', 'app_lms', 'pem', 'secret')`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { wantPgCode(t, exec(t, pool, c.sql), pgCheckViolation) })
	}
}

func TestIdempotencyKeysAreScopedToTheApp(t *testing.T) {
	pool := dbtest.New(t)
	seed(t, pool)
	insert := `INSERT INTO idempotency_keys (project_id, app_id, key, request_hash) VALUES ('prj_default', $1, 'k-1', 'h')`
	mustExec(t, pool, insert, "app_lms")
	wantPgCode(t, exec(t, pool, insert, "app_lms"), pgUniqueViolation)
	mustExec(t, pool, insert, "app_dashboard")
}

func TestMigrationsRollBackAndReapply(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	if err := db.MigrateDownAll(ctx, pool); err != nil {
		t.Fatalf("down: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name <> 'goose_db_version'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d tables left after rolling back every migration", n)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
}

// Rolling back the families (P5-T13) with both kinds of job active drops
// the track jobs and keeps the encode, so the single index can come back.
func TestTheTrackMigrationRollsBackWithJobsActive(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	_, asset := seed(t, pool)
	mustExec(t, pool, `INSERT INTO tracks (id, project_id, asset_id, kind) VALUES ('trk_1', 'prj_default', $1, 'subtitles')`, asset)
	mustExec(t, pool, `INSERT INTO jobs (id, project_id, asset_id, kind, routed_to, status) VALUES ('job_1', 'prj_default', $1, 'reencode', 'workers', 'running')`, asset)
	mustExec(t, pool, `INSERT INTO jobs (id, project_id, asset_id, kind, routed_to, status, track_id) VALUES ('job_2', 'prj_default', $1, 'transcribe', 'workers', 'queued', 'trk_1')`, asset)
	if err := db.MigrateDownTo(ctx, pool, 23); err != nil {
		t.Fatalf("down to 23: %v", err)
	}
	var kinds string
	if err := pool.QueryRow(ctx, `SELECT string_agg(kind, ',' ORDER BY id) FROM jobs`).Scan(&kinds); err != nil {
		t.Fatal(err)
	}
	if kinds != "reencode" {
		t.Fatalf("jobs after rollback: %s", kinds)
	}
	err := exec(t, pool, `INSERT INTO jobs (id, project_id, asset_id, kind, status, source_storage_id, target_storage_id) VALUES ('job_3', 'prj_default', $1, 'move', 'queued', 'sto_default', 'sto_default')`, asset)
	wantPgCode(t, err, pgUniqueViolation)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
}

// The built-in app takes no API keys (D123): the migration revokes any it
// had, and leaves other apps' keys alone.
func TestTheBuiltinAppMigrationRevokesItsKeys(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	if err := db.MigrateDownTo(ctx, pool, 34); err != nil {
		t.Fatalf("down to 34: %v", err)
	}
	seed(t, pool)
	mustExec(t, pool, `INSERT INTO api_keys (id, project_id, app_id, name, key_hash, scopes) VALUES
		('key_dash', 'prj_default', 'app_dashboard', 'backend', '\x01', '{assets:read}'),
		('key_lms', 'prj_default', 'app_lms', 'backend', '\x02', '{assets:read}')`)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("up: %v", err)
	}
	var revoked string
	if err := pool.QueryRow(ctx, `SELECT coalesce(string_agg(id, ',' ORDER BY id), '') FROM api_keys WHERE revoked_at IS NOT NULL`).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if revoked != "key_dash" {
		t.Fatalf("revoked keys = %q, want only the built-in app's", revoked)
	}
}
