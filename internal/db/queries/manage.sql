-- The owner's management API: apps, their keys and endpoints, the library.

-- name: ListApps :many
SELECT * FROM apps WHERE project_id = $1 ORDER BY is_builtin DESC, created_at, id;

-- name: GetApp :one
SELECT * FROM apps WHERE project_id = $1 AND id = $2;

-- name: SetAppDisabled :one
-- The built-in Dashboard app can't be disabled: dashboard uploads need it.
UPDATE apps SET disabled_at = CASE WHEN sqlc.arg(disabled)::boolean THEN coalesce(disabled_at, now()) END
WHERE project_id = sqlc.arg(project_id) AND id = sqlc.arg(id) AND NOT is_builtin
RETURNING *;

-- name: ListAPIKeys :many
SELECT id, name, scopes, created_at, last_used_at, revoked_at FROM api_keys
WHERE app_id = $1 ORDER BY created_at DESC, id DESC;

-- name: RevokeAPIKey :execrows
UPDATE api_keys SET revoked_at = coalesce(revoked_at, now()) WHERE project_id = $1 AND id = $2;

-- name: ListSigningKeys :many
SELECT id, created_at, retired_at, revoked_at FROM signing_keys
WHERE app_id = $1 ORDER BY created_at DESC, id DESC;

-- name: RevokeSigningKey :execrows
-- App keys only; the internal key is the server's.
UPDATE signing_keys SET revoked_at = coalesce(revoked_at, now())
WHERE project_id = $1 AND id = $2 AND app_id IS NOT NULL;

-- name: ListWebhookEndpoints :many
SELECT id, url, created_at, disabled_at, consecutive_failures, circuit_open_until, pending_secret_at FROM webhook_endpoints
WHERE app_id = $1 ORDER BY created_at DESC, id DESC;

-- name: DisableWebhookEndpoint :execrows
UPDATE webhook_endpoints SET disabled_at = coalesce(disabled_at, now()) WHERE project_id = $1 AND id = $2;

-- name: ListLibrary :many
-- Every app's assets, newest first, keyset-paged on (created_at, id).
SELECT * FROM assets
WHERE project_id = sqlc.arg(project_id)
  AND (sqlc.narg(app_id)::text IS NULL OR app_id = sqlc.narg(app_id))
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
  AND (created_at, id) < (sqlc.arg(before_time)::timestamptz, sqlc.arg(before_id)::text)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(max_assets);

-- name: GetAssetInProject :one
SELECT * FROM assets WHERE project_id = $1 AND id = $2;

-- name: ListJobs :many
-- Jobs across the library, newest first, keyset-paged on (created_at, id).
SELECT j.*, s.app_id, s.passthrough FROM jobs j
JOIN assets s ON s.id = j.asset_id
WHERE j.project_id = sqlc.arg(project_id)
  AND (sqlc.narg(status)::text IS NULL OR j.status = sqlc.narg(status))
  AND (sqlc.narg(asset_id)::text IS NULL OR j.asset_id = sqlc.narg(asset_id))
  AND (sqlc.narg(agent_id)::text IS NULL OR j.pinned_agent_id = sqlc.narg(agent_id)
       OR EXISTS (SELECT 1 FROM attempts a WHERE a.job_id = j.id AND a.agent_id = sqlc.narg(agent_id)))
  AND (j.created_at, j.id) < (sqlc.arg(before_time)::timestamptz, sqlc.arg(before_id)::text)
ORDER BY j.created_at DESC, j.id DESC
LIMIT sqlc.arg(max_jobs);

-- name: ListJobAttempts :many
-- Each attempt of the given jobs: where it ran, how it ended, and which
-- encoder stage its output's manifest records.
SELECT a.id, a.job_id, a.attempt_number, a.executor, a.route_reason, a.agent_id, coalesce(g.name, '')::text AS agent_name,
       a.status, a.failure_kind, a.error_code, a.phase, a.progress, a.started_at, a.finished_at,
       a.last_heartbeat_at, a.output_id, (o.manifest -> 'encoder')::jsonb AS encoder
FROM attempts a
LEFT JOIN agents g ON g.id = a.agent_id
LEFT JOIN outputs o ON o.id = a.output_id
WHERE a.job_id = ANY (sqlc.arg(job_ids)::text[])
ORDER BY a.job_id, a.attempt_number;

-- name: GetJobInProject :one
SELECT j.*, s.app_id, s.passthrough FROM jobs j
JOIN assets s ON s.id = j.asset_id
WHERE j.project_id = $1 AND j.id = $2;

-- name: ListAgentCurrentAssets :many
-- Live assets whose current output an agent encoded, for re-encoding after
-- the agent is revoked.
SELECT s.id FROM assets s
JOIN attempts a ON a.output_id = s.current_output_id
WHERE a.agent_id = $1 AND s.project_id = $2 AND s.status = 'ready'
ORDER BY s.id;
