-- name: GetBuiltinApp :one
SELECT id, project_id, name FROM apps WHERE project_id = $1 AND is_builtin;

-- name: GetDefaultStorage :one
SELECT id, kind FROM storage_configs WHERE project_id = $1 AND is_default;

-- name: CreateApp :one
INSERT INTO apps (id, project_id, name) VALUES ($1, $2, $3)
RETURNING *;

-- name: CreateAsset :one
INSERT INTO assets (id, project_id, app_id, status, storage_id, passthrough)
VALUES ($1, $2, $3, 'waiting_upload', $4, $5)
RETURNING *;

-- name: GetAsset :one
-- Apps see only their own assets.
SELECT * FROM assets WHERE id = $1 AND app_id = $2;

-- name: ListAssetsByPassthrough :many
-- Lets an app find its videos by its own reference (e.g. LMS upload recovery).
SELECT * FROM assets
WHERE app_id = $1 AND passthrough = $2 AND status <> 'deleted'
ORDER BY created_at DESC, id DESC
LIMIT $3;

-- name: ListAssetsUpdatedSince :many
-- Catch-up for apps that missed webhooks: oldest change first, deleted
-- assets included, keyset-paged on (updated_at, id).
SELECT * FROM assets
WHERE app_id = sqlc.arg(app_id)
  AND (updated_at, id) > (sqlc.arg(after_time)::timestamptz, sqlc.arg(after_id)::text)
ORDER BY updated_at, id
LIMIT sqlc.arg(max_assets);
