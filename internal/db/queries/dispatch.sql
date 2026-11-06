-- Dispatches (P4-T02, D054): waking a serverless provider for one job.

-- name: ProviderHasRoom :one
-- Called after LockExecutorConfigByKind, as its own statement: under READ
-- COMMITTED a statement's snapshot is taken before it waits for a row lock,
-- so a room check in the locking statement itself would miss the
-- dispatches the previous lock holder just committed (D058).
SELECT provider_has_room(sqlc.arg(id), sqlc.arg(asset_id))::boolean;

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
-- Connected providers: a disconnected one keeps its row (its dispatches
-- are spend history) without credentials.
SELECT * FROM executor_configs WHERE project_id = $1 AND secrets_encrypted IS NOT NULL ORDER BY kind;

-- name: GetExecutorConfigByKind :one
SELECT * FROM executor_configs WHERE project_id = $1 AND kind = $2;

-- name: LockExecutorConfigByKind :one
SELECT * FROM executor_configs WHERE project_id = $1 AND kind = $2 FOR UPDATE;

-- name: UpsertExecutorConfig :one
-- Connect, or connect again: new settings and credentials, and the
-- provider must pass a setup test again before it takes work.
INSERT INTO executor_configs (id, project_id, kind, config, secrets_encrypted, max_concurrent, monthly_limit_cents,
                              rate_cents_per_hour, disk_bytes)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (project_id, kind) DO UPDATE
SET config = EXCLUDED.config, secrets_encrypted = EXCLUDED.secrets_encrypted, max_concurrent = EXCLUDED.max_concurrent,
    monthly_limit_cents = EXCLUDED.monthly_limit_cents, rate_cents_per_hour = EXCLUDED.rate_cents_per_hour,
    disk_bytes = EXCLUDED.disk_bytes, verified_at = NULL, wake_failures = 0, backoff_until = NULL, updated_at = now()
RETURNING *;

-- name: DisconnectExecutor :exec
UPDATE executor_configs SET secrets_encrypted = NULL, verified_at = NULL, updated_at = now() WHERE id = $1;

-- name: ExecutorStatus :many
SELECT c.id, c.kind, c.config, (c.secrets_encrypted IS NOT NULL)::boolean AS connected, c.verified_at,
       c.max_concurrent, c.monthly_limit_cents, c.rate_cents_per_hour, c.disk_bytes, c.backoff_until,
       provider_month_cents(c.id)::double precision AS month_cents,
       (SELECT count(*) FROM dispatches d WHERE d.executor_config_id = c.id
          AND (d.status <> 'ended' OR (d.instance_id IS NOT NULL AND d.stopped_at IS NULL)))::bigint AS machines
FROM executor_configs c WHERE c.project_id = $1 ORDER BY c.kind;

-- name: ListProviderMachines :many
-- The machines a provider should be running: those of dispatches that
-- haven't been stopped. A waking dispatch without an instance yet is known
-- by its name, because its wake may be in flight.
SELECT id, instance_id FROM dispatches
WHERE executor_config_id = $1
  AND (status <> 'ended' OR (instance_id IS NOT NULL AND stopped_at IS NULL));
