-- Leased job queue (docs/architecture.md, "Compute" and "Leases").
-- Lock order everywhere: job, then attempt, then asset.

-- name: CreateJob :one
INSERT INTO jobs (id, project_id, asset_id, kind, pinned_agent_id)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: LockNextClaimableJob :one
-- Media work only: move and delete run in the control plane. Pinned jobs go
-- only to their agent, and local-scoped agents take only pinned jobs.
SELECT * FROM jobs
WHERE project_id = sqlc.arg(project_id)
  AND status = 'queued'
  AND available_at <= now()
  AND kind IN ('import', 'transcode', 'reencode')
  AND (pinned_agent_id IS NULL OR pinned_agent_id = sqlc.narg(agent_id))
  AND (NOT sqlc.arg(local_only)::boolean OR pinned_agent_id = sqlc.narg(agent_id))
ORDER BY available_at, id
LIMIT 1
FOR UPDATE SKIP LOCKED;

-- name: NextAttemptNumber :one
SELECT (coalesce(max(attempt_number), 0) + 1)::integer FROM attempts WHERE job_id = $1;

-- name: LatestAttemptNumber :one
SELECT coalesce(max(attempt_number), 0)::integer FROM attempts WHERE job_id = $1;

-- name: CreateOutput :one
INSERT INTO outputs (id, project_id, asset_id, key_version)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: CreateAttempt :one
INSERT INTO attempts (id, project_id, job_id, asset_id, attempt_number, output_id, executor, agent_id,
                      token_hash, lease_expires_at, worker_info)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now() + make_interval(secs => sqlc.arg(lease_seconds)::double precision), $10)
RETURNING *;

-- name: StartJob :exec
UPDATE jobs
SET status = 'running', phase = NULL, progress = NULL, started_at = coalesce(started_at, now()),
    routed_to = $2, route_reason = $3
WHERE id = $1;

-- name: GetAttempt :one
SELECT * FROM attempts WHERE id = $1;

-- name: GetAttemptByTokenHash :one
SELECT * FROM attempts WHERE token_hash = $1;

-- name: GetJob :one
SELECT * FROM jobs WHERE id = $1;

-- name: LockJob :one
SELECT * FROM jobs WHERE id = $1 FOR UPDATE;

-- name: LockAttempt :one
SELECT sqlc.embed(attempts), (lease_expires_at < now())::boolean AS lease_expired
FROM attempts WHERE id = $1 FOR UPDATE;

-- name: RenewLease :exec
UPDATE attempts
SET status = 'running', last_heartbeat_at = now(),
    lease_expires_at = now() + make_interval(secs => sqlc.arg(lease_seconds)::double precision),
    phase = sqlc.narg(phase), progress = sqlc.narg(progress)
WHERE id = sqlc.arg(id);

-- name: SetJobRunningProgress :exec
UPDATE jobs SET status = 'running', phase = $2, progress = $3 WHERE id = $1;

-- name: ListExpiredLeases :many
SELECT id FROM attempts
WHERE status = 'running' AND lease_expires_at < now()
ORDER BY lease_expires_at
LIMIT $1;

-- name: SetAttemptStatus :exec
UPDATE attempts
SET status = $2, failure_kind = $3, error_code = $4,
    finished_at = CASE WHEN $2 IN ('committed', 'failed') THEN now() ELSE finished_at END
WHERE id = $1;

-- name: CountJobLoss :one
UPDATE jobs SET lost_count = lost_count + 1 WHERE id = $1 RETURNING lost_count;

-- name: CountJobFailure :one
UPDATE jobs SET failed_count = failed_count + 1 WHERE id = $1 RETURNING failed_count;

-- name: RequeueJob :exec
UPDATE jobs
SET status = 'queued', phase = NULL, progress = NULL,
    available_at = now() + make_interval(secs => sqlc.arg(delay_seconds)::double precision)
WHERE id = sqlc.arg(id);

-- name: FailJob :exec
UPDATE jobs SET status = 'failed', error_code = $2, finished_at = now() WHERE id = $1;

-- name: CancelJob :exec
UPDATE jobs SET status = 'cancelled', finished_at = now() WHERE id = $1 AND status IN ('queued', 'running');

-- name: LockAsset :one
SELECT * FROM assets WHERE id = $1 FOR UPDATE;

-- name: MarkAssetErrored :one
UPDATE assets
SET status = 'errored', error_code = $2, version = version + 1, updated_at = now()
WHERE id = $1
RETURNING version;

-- name: InsertOutbox :exec
INSERT INTO outbox (id, project_id, kind, app_id, asset_id, payload)
VALUES ($1, $2, $3, $4, $5, $6);
