-- Commit and route publication (docs/architecture.md, "Commit").

-- name: GetOutput :one
SELECT * FROM outputs WHERE id = $1;

-- name: CommitOutput :execrows
UPDATE outputs SET status = 'committed', manifest = $2, committed_at = now()
WHERE id = $1 AND status = 'writing';

-- name: MoveCurrentOutput :one
-- The only pointer that moves. Bumps the asset version.
UPDATE assets SET current_output_id = $2, version = version + 1, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: RetireOutput :exec
UPDATE outputs SET status = 'retired', retired_at = now() WHERE id = $1 AND status = 'committed';

-- name: SucceedJob :exec
UPDATE jobs SET status = 'succeeded', phase = NULL, progress = 1, finished_at = now() WHERE id = $1;

-- name: LockPendingRouteSyncs :many
SELECT * FROM outbox
WHERE kind = 'route_sync' AND done_at IS NULL AND available_at <= now()
ORDER BY created_at, id
LIMIT $1
FOR UPDATE SKIP LOCKED;

-- name: MarkOutboxDone :exec
UPDATE outbox SET done_at = now(), attempts = attempts + 1 WHERE id = $1;

-- name: RetryOutbox :exec
UPDATE outbox
SET attempts = attempts + 1, last_error = sqlc.arg(last_error),
    available_at = now() + make_interval(secs => sqlc.arg(delay_seconds)::double precision)
WHERE id = sqlc.arg(id);

-- name: CountPendingRoutesForOutput :one
SELECT count(*) FROM outbox
WHERE kind = 'route_sync' AND done_at IS NULL AND asset_id = sqlc.arg(asset_id)
  AND payload->>'output_id' = sqlc.arg(output_id)::text;

-- name: MarkAssetReady :one
UPDATE assets SET status = 'ready', version = version + 1, updated_at = now()
WHERE id = $1 AND status = 'processing'
RETURNING version;

-- name: RevokePlaybackID :execrows
-- A revoked ID reads revoked only once its unpublish is confirmed.
UPDATE playback_ids SET status = 'revoked', revoked_at = now() WHERE id = $1 AND status = 'revoking';

-- name: CountPendingRoutesForAsset :one
SELECT count(*) FROM outbox WHERE kind = 'route_sync' AND done_at IS NULL AND asset_id = $1;
