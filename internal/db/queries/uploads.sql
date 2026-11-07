-- Direct uploads (docs/architecture.md, "Multipart upload flow").

-- name: CreateUpload :one
INSERT INTO uploads (id, project_id, asset_id, multipart_id, declared_size, part_size, token_hash)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: CreatePlaybackID :one
INSERT INTO playback_ids (id, project_id, asset_id, policy)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListPlaybackIDs :many
SELECT * FROM playback_ids WHERE asset_id = $1 AND status <> 'revoked' ORDER BY created_at, id;

-- name: GetUploadByTokenHash :one
-- Disabling an app cuts off everything it owns, upload tokens included.
SELECT * FROM uploads u
WHERE u.token_hash = $1
  AND NOT EXISTS (SELECT 1 FROM assets a JOIN apps ap ON ap.id = a.app_id
                  WHERE a.id = u.asset_id AND ap.disabled_at IS NOT NULL);

-- name: GetUpload :one
SELECT * FROM uploads WHERE id = $1;

-- name: LockUpload :one
SELECT * FROM uploads WHERE id = $1 FOR UPDATE;

-- name: TouchUpload :exec
UPDATE uploads SET last_activity_at = now() WHERE id = $1;

-- name: SetUploadPartsSeen :exec
UPDATE uploads SET parts_seen = $2, last_activity_at = now() WHERE id = $1;

-- name: FinishUpload :exec
UPDATE uploads SET status = $2, finished_at = now() WHERE id = $1;

-- name: ListActiveUploads :many
SELECT id FROM uploads WHERE status = 'active'
ORDER BY last_swept_at NULLS FIRST, last_activity_at, id
LIMIT $1;

-- name: MarkUploadSwept :exec
UPDATE uploads SET last_swept_at = now() WHERE id = $1;

-- name: UploadIdleSince :one
-- Whether the upload has had no activity for the given number of seconds.
SELECT last_activity_at < now() - make_interval(secs => sqlc.arg(seconds)::double precision) AS idle
FROM uploads WHERE id = sqlc.arg(id);

-- name: MarkAssetProcessing :one
UPDATE assets
SET status = 'processing', source_uri = $2, version = version + 1, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: GetAssetByID :one
SELECT * FROM assets WHERE id = $1;

-- name: ClaimIdempotencyKey :one
-- Inserts the key, or returns nothing if it already exists (after waiting
-- for a concurrent request that holds it to finish).
INSERT INTO idempotency_keys (project_id, app_id, key, request_hash)
VALUES ($1, $2, $3, $4)
ON CONFLICT (app_id, key) DO NOTHING
RETURNING key;

-- name: GetIdempotencyKey :one
SELECT * FROM idempotency_keys WHERE app_id = $1 AND key = $2;

-- name: SaveIdempotentResponse :exec
UPDATE idempotency_keys SET response_status = $3, response_body = $4 WHERE app_id = $1 AND key = $2;

-- name: RotateUploadToken :execrows
UPDATE uploads SET token_hash = $2 WHERE id = $1 AND status = 'active';

-- name: ListPlaybackIDsForAssets :many
-- One query for a page of assets (D072), in the order ListPlaybackIDs uses.
SELECT * FROM playback_ids WHERE asset_id = ANY(sqlc.arg(asset_ids)::text[]) AND status <> 'revoked'
ORDER BY asset_id, created_at, id;
