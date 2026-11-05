-- Asset lifecycle in the control plane: delete (docs/architecture.md, "One
-- mutating job per asset"). Lock order: upload, job, then asset.

-- name: LockActiveUploadForAsset :one
SELECT * FROM uploads WHERE asset_id = $1 AND status = 'active' FOR UPDATE;

-- name: GetActiveJobForAsset :one
SELECT * FROM jobs WHERE asset_id = $1 AND status IN ('queued', 'running');

-- name: MarkAssetDeleted :one
UPDATE assets SET status = 'deleted', error_code = NULL, version = version + 1, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: MarkPlaybackIDsRevoking :many
UPDATE playback_ids SET status = 'revoking' WHERE asset_id = $1 AND status = 'active'
RETURNING id;

-- name: ClaimControlJob :one
-- serve runs delete jobs itself; workers never claim them. A claim leases the
-- job by pushing available_at out, so a crashed pass is picked up again.
UPDATE jobs SET status = 'running', started_at = coalesce(started_at, now()),
    available_at = now() + make_interval(secs => sqlc.arg(lease_seconds)::double precision)
WHERE id = (
    SELECT c.id FROM jobs c
    WHERE c.kind = 'delete' AND c.status IN ('queued', 'running') AND c.available_at <= now()
      AND (sqlc.narg(job_id)::text IS NULL OR c.id = sqlc.narg(job_id))
    ORDER BY c.available_at, c.id
    LIMIT 1
    FOR UPDATE SKIP LOCKED)
RETURNING *;

-- name: DeferJob :exec
UPDATE jobs SET available_at = now() + make_interval(secs => sqlc.arg(delay_seconds)::double precision)
WHERE id = sqlc.arg(id);

-- name: ListUploadsForAsset :many
SELECT id, multipart_id FROM uploads WHERE asset_id = $1;

-- name: LateWritesUntil :one
-- PUT URLs signed for a job's attempts outlive the job by up to their TTL; a
-- file written in that window must be deleted by a second pass after it.
SELECT coalesce(max(finished_at), '-infinity'::timestamptz) + make_interval(secs => sqlc.arg(window_seconds)::double precision) > now() AS pending,
       greatest(extract(epoch FROM coalesce(max(finished_at), now()) + make_interval(secs => sqlc.arg(window_seconds)::double precision) - now()), 0)::double precision AS wait_seconds
FROM jobs WHERE asset_id = sqlc.arg(asset_id) AND kind <> 'delete';

-- name: MarkAssetFilesDeleted :execrows
UPDATE assets SET deleted_at = now() WHERE id = $1 AND deleted_at IS NULL;
