-- Asset lifecycle in the control plane: delete (docs/architecture.md, "One
-- mutating job per asset"). Lock order: upload, job, then asset.

-- name: LockActiveUploadForAsset :one
SELECT * FROM uploads WHERE asset_id = $1 AND status = 'active' FOR UPDATE;

-- name: GetActiveJobForAsset :many
-- The asset's active jobs of the kinds given: one family (outputs or
-- tracks, at most one job each) or both. Move and delete belong to both.
SELECT * FROM jobs WHERE asset_id = $1 AND status IN ('queued', 'running') AND kind = ANY (sqlc.arg(kinds)::text[])
ORDER BY id;

-- name: MarkAssetDeleted :one
UPDATE assets SET status = 'deleted', error_code = NULL, version = version + 1, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: MarkPlaybackIDsRevoking :many
UPDATE playback_ids SET status = 'revoking' WHERE asset_id = $1 AND status = 'active'
RETURNING id;

-- name: ClaimControlJob :one
-- serve runs delete and move jobs itself; workers never claim them. A claim leases the
-- job by pushing available_at out, so a crashed pass is picked up again.
UPDATE jobs SET status = 'running', started_at = coalesce(started_at, now()),
    available_at = now() + make_interval(secs => sqlc.arg(lease_seconds)::double precision)
WHERE id = (
    SELECT c.id FROM jobs c
    WHERE c.kind IN ('delete', 'move') AND c.status IN ('queued', 'running') AND c.available_at <= now()
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

-- name: MarkAssetReprocessing :one
-- A re-encode of an errored asset whose source was kept.
UPDATE assets SET status = 'processing', error_code = NULL, version = version + 1, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: BumpAssetVersion :one
UPDATE assets SET version = version + 1, updated_at = now() WHERE id = $1
RETURNING *;

-- name: GetPlaybackIDOfAsset :one
SELECT * FROM playback_ids WHERE id = $1 AND asset_id = $2;

-- name: MarkPlaybackIDRevoking :execrows
UPDATE playback_ids SET status = 'revoking' WHERE id = $1 AND status = 'active';

-- name: CountLivePlaybackIDs :one
SELECT count(*) FROM playback_ids WHERE asset_id = $1 AND status <> 'revoked';

-- name: QueueMoveJob :one
-- One mutating job per asset: the partial unique index refuses a second.
INSERT INTO jobs (id, project_id, asset_id, kind, source_storage_id, target_storage_id)
VALUES ($1, $2, $3, 'move', $4, $5)
RETURNING *;

-- name: ListAssetsToMove :many
-- Live assets not on the target store and with no active job.
SELECT a.id, a.storage_id FROM assets a
WHERE a.project_id = $1 AND a.status IN ('ready', 'errored') AND a.storage_id <> sqlc.arg(target)::text
  AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.asset_id = a.id AND j.status IN ('queued', 'running'))
ORDER BY a.created_at, a.id;

-- name: SetJobPhase :exec
UPDATE jobs SET phase = sqlc.arg(phase), available_at = now() + make_interval(secs => sqlc.arg(delay_seconds)::double precision)
WHERE id = sqlc.arg(id);

-- name: MoveAssetStorage :execrows
UPDATE assets SET storage_id = sqlc.arg(target)::text, version = version + 1, updated_at = now()
WHERE id = sqlc.arg(id) AND storage_id = sqlc.arg(source)::text;

-- name: ListServableOutputIDs :many
SELECT id FROM outputs WHERE asset_id = $1 AND status IN ('committed', 'retired') AND purged_at IS NULL ORDER BY id;

-- name: MoveProgress :one
SELECT
    count(*) FILTER (WHERE j.status IN ('queued', 'running'))::bigint AS active,
    count(*) FILTER (WHERE j.status = 'succeeded')::bigint AS moved,
    count(*) FILTER (WHERE j.status = 'failed')::bigint AS failed
FROM jobs j WHERE j.project_id = $1 AND j.kind = 'move';

-- name: CountAssetsOffStore :one
SELECT count(*) FROM assets
WHERE project_id = $1 AND status IN ('ready', 'errored', 'processing') AND storage_id <> sqlc.arg(target)::text;

-- name: CountPendingRouteSyncsForAsset :one
SELECT count(*) FROM outbox WHERE kind = 'route_sync' AND done_at IS NULL AND asset_id = $1;
