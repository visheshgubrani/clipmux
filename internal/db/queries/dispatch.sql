-- Dispatches (P4-T02, D054): waking a serverless provider for one job.

-- name: LockProviderWithRoom :one
-- In the router's transaction, before creating a dispatch: the provider's
-- row lock serialises routers, so its limits can't be overshot (D055).
SELECT c.id, provider_has_room(c.id, sqlc.arg(asset_id))::boolean AS has_room
FROM executor_configs c
WHERE c.project_id = sqlc.arg(project_id) AND c.kind = sqlc.arg(kind)
FOR UPDATE;

-- name: CreateDispatch :one
-- The provider's rate and claim deadline, and the job's estimated run as
-- its reservation, are copied onto the dispatch.
INSERT INTO dispatches (id, project_id, job_id, executor_config_id, kind, key_hash, key_sealed, rate_cents_per_hour,
                        claim_deadline, reserved_seconds)
SELECT sqlc.arg(id), c.project_id, sqlc.arg(job_id), c.id, c.kind, sqlc.arg(key_hash), sqlc.arg(key_sealed),
       c.rate_cents_per_hour, now() + make_interval(secs => c.claim_deadline_seconds),
       job_estimated_seconds(sqlc.arg(asset_id))
FROM executor_configs c
WHERE c.id = sqlc.arg(executor_config_id)
RETURNING *;

-- name: GetClaimableDispatchByKeyHash :one
SELECT * FROM dispatches WHERE key_hash = $1 AND status = 'waking' AND claim_deadline > now();

-- name: LockPendingDispatchOutbox :many
SELECT * FROM outbox
WHERE kind IN ('wake', 'stop') AND done_at IS NULL AND available_at <= now()
ORDER BY created_at, id
LIMIT $1
FOR UPDATE SKIP LOCKED;

-- name: LockDispatch :one
SELECT * FROM dispatches WHERE id = $1 FOR UPDATE;

-- name: GetDispatch :one
SELECT * FROM dispatches WHERE id = $1;

-- name: GetExecutorConfig :one
SELECT * FROM executor_configs WHERE id = $1;

-- name: SetDispatchWoken :exec
UPDATE dispatches SET instance_id = $2, woken_at = coalesce(woken_at, now()) WHERE id = $1;

-- name: EndDispatch :one
UPDATE dispatches SET status = 'ended', end_reason = $2, ended_at = now()
WHERE id = $1 AND status <> 'ended'
RETURNING *;

-- name: SetDispatchStopped :exec
UPDATE dispatches SET stopped_at = now() WHERE id = $1;

-- name: VerifyExecutor :exec
-- A setup test through this provider passed.
UPDATE executor_configs SET verified_at = now(), updated_at = now() WHERE project_id = $1 AND kind = $2;

-- name: LatestDispatchForAsset :one
-- The setup test's encode check fails at once when its dispatch ends
-- without a claim.
SELECT d.* FROM dispatches d JOIN jobs j ON j.id = d.job_id
WHERE j.asset_id = $1
ORDER BY d.created_at DESC, d.id DESC
LIMIT 1;

-- name: RecordWakeFailure :exec
-- Three failed wakes in a row put the provider in a 5-minute backoff, so
-- jobs wait for the primary instead of looping (D055).
UPDATE executor_configs
SET wake_failures = CASE WHEN wake_failures + 1 >= 3 THEN 0 ELSE wake_failures + 1 END,
    backoff_until = CASE WHEN wake_failures + 1 >= 3 THEN now() + interval '5 minutes' ELSE backoff_until END
WHERE id = $1;

-- name: RecordWakeSuccess :exec
UPDATE executor_configs SET wake_failures = 0 WHERE id = $1 AND wake_failures <> 0;

-- name: ListDispatchesToEnd :many
-- Live dispatches the reaper should end; DispatchEndReason decides again
-- under the dispatch's lock.
SELECT d.id FROM dispatches d
JOIN jobs j ON j.id = d.job_id
LEFT JOIN attempts a ON a.id = d.attempt_id
WHERE d.status <> 'ended'
  AND (d.created_at < now() - make_interval(secs => sqlc.arg(max_lifetime_seconds)::double precision)
       OR (d.status = 'claimed' AND a.status <> 'running')
       OR (d.status = 'waking' AND (j.status <> 'queued' OR d.claim_deadline <= now())))
ORDER BY d.created_at, d.id
LIMIT sqlc.arg(max_rows);

-- name: DispatchEndReason :one
-- Why a live dispatch should end now, or '' if it shouldn't.
SELECT (CASE
    WHEN d.status = 'ended' THEN ''
    WHEN d.created_at < now() - make_interval(secs => sqlc.arg(max_lifetime_seconds)::double precision) THEN 'max_lifetime'
    WHEN d.status = 'claimed' AND a.status <> 'running' THEN 'attempt_ended'
    WHEN d.status = 'waking' AND j.status <> 'queued' THEN 'job_gone'
    WHEN d.status = 'waking' AND d.claim_deadline <= now() THEN 'wake_timeout'
    ELSE '' END)::text
FROM dispatches d
JOIN jobs j ON j.id = d.job_id
LEFT JOIN attempts a ON a.id = d.attempt_id
WHERE d.id = sqlc.arg(id);

-- name: ListExecutorConfigs :many
SELECT * FROM executor_configs WHERE project_id = $1 ORDER BY kind;

-- name: ListProviderMachines :many
-- The machines a provider should be running: those of dispatches that
-- haven't been stopped. A waking dispatch without an instance yet is known
-- by its name, because its wake may be in flight.
SELECT id, instance_id FROM dispatches
WHERE executor_config_id = $1
  AND (status <> 'ended' OR (instance_id IS NOT NULL AND stopped_at IS NULL));
