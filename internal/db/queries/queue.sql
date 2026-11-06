-- Leased job queue (docs/architecture.md, "Compute" and "Leases").
-- Lock order everywhere: job, then attempt, then asset.

-- name: CreateJob :one
INSERT INTO jobs (id, project_id, asset_id, kind, pinned_agent_id)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: LockNextClaimableJob :one
-- Media work only: import, move and delete run in the control plane. Pinned jobs go
-- only to their agent, and local-scoped agents take only pinned jobs. An
-- untrusted (unsandboxed) agent takes only jobs pinned to it and the owner's
-- own uploads (the built-in Dashboard app's assets), never public uploads.
SELECT * FROM jobs
WHERE jobs.project_id = sqlc.arg(project_id)
  AND jobs.status = 'queued'
  AND available_at <= now()
  AND kind IN ('transcode', 'reencode')
  AND (pinned_agent_id IS NULL OR pinned_agent_id = sqlc.narg(agent_id))
  AND (NOT sqlc.arg(local_only)::boolean OR pinned_agent_id = sqlc.narg(agent_id))
  AND (NOT sqlc.arg(untrusted)::boolean OR pinned_agent_id = sqlc.narg(agent_id) OR EXISTS (
      SELECT 1 FROM assets s JOIN apps p ON p.id = s.app_id
      WHERE s.id = jobs.asset_id AND p.is_builtin))
  -- A worker that reports its free disk takes only sources that fit: three
  -- times an upload's size (source, encodes, packaged output), twice a source
  -- already on the agent. Unknown sizes (imports) and unreported disk pass.
  AND (sqlc.narg(free_disk_bytes)::bigint IS NULL OR coalesce(
      (SELECT 3 * u.declared_size FROM uploads u WHERE u.asset_id = jobs.asset_id ORDER BY u.created_at DESC LIMIT 1),
      (SELECT 2 * g.size FROM assets s JOIN agent_sources g ON g.id = s.agent_source_id WHERE s.id = jobs.asset_id),
      0) <= sqlc.narg(free_disk_bytes)::bigint)
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

-- name: ReserveSignedPaths :one
-- Counts paths against the attempt's budget; no row means the budget is spent.
UPDATE attempts SET signed_paths = signed_paths + sqlc.arg(n)::integer
WHERE id = sqlc.arg(id) AND signed_paths + sqlc.arg(n)::integer <= sqlc.arg(budget)::integer
RETURNING signed_paths;

-- name: DeleteAttempt :exec
DELETE FROM attempts WHERE id = $1;

-- name: DeleteWritingOutput :exec
DELETE FROM outputs WHERE id = $1 AND status = 'writing';
