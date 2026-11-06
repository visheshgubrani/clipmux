-- Dispatches (P4-T02, D054): waking a serverless provider for one job.

-- name: CreateDispatch :one
-- In the router's transaction: the provider's rate and claim deadline are
-- copied onto the dispatch.
INSERT INTO dispatches (id, project_id, job_id, executor_config_id, kind, key_hash, key_sealed, rate_cents_per_hour, claim_deadline)
SELECT sqlc.arg(id), c.project_id, sqlc.arg(job_id), c.id, c.kind, sqlc.arg(key_hash), sqlc.arg(key_sealed),
       c.rate_cents_per_hour, now() + make_interval(secs => c.claim_deadline_seconds)
FROM executor_configs c
WHERE c.project_id = sqlc.arg(project_id) AND c.kind = sqlc.arg(kind)
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
