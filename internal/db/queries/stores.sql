-- Storage configs (docs/architecture.md, "Per-asset storage").

-- name: GetStorageConfig :one
SELECT * FROM storage_configs WHERE id = $1;

-- name: AssetStores :one
-- The store holding an asset's source, and the one holding its encoded
-- files and tracks (D117).
SELECT storage_id, media_storage_id FROM assets WHERE id = $1;

-- name: GetMediaTarget :one
-- Where new assets' encoded files go: the delivery bucket while the Worker
-- delivers, otherwise the default store.
SELECT media_target(sqlc.arg(project_id)::text)::text AS id;

-- name: ListStorageConfigs :many
SELECT * FROM storage_configs WHERE project_id = $1 AND removed_at IS NULL ORDER BY created_at, id;

-- name: ListStorageSummaries :many
-- Each config the owner can see, with how many assets (deleted ones
-- aside) keep their source or encoded files on it, and whether Cloudflare
-- delivery reads it.
SELECT c.id, c.kind, c.config, c.is_default, c.created_at,
    (SELECT count(*) FROM assets a WHERE (a.storage_id = c.id OR a.media_storage_id = c.id) AND a.status <> 'deleted')::bigint AS asset_count,
    EXISTS (SELECT 1 FROM projects p WHERE p.id = c.project_id AND p.delivery = 'worker' AND p.delivery_storage_id = c.id)::boolean AS delivery
FROM storage_configs c
WHERE c.project_id = $1 AND c.removed_at IS NULL
ORDER BY c.created_at, c.id;

-- name: LockStorageConfig :one
SELECT * FROM storage_configs WHERE project_id = $1 AND id = $2 AND removed_at IS NULL FOR UPDATE;

-- name: StorageUsage :one
-- What still needs a store: assets whose files may be on it (a deleted
-- asset until its files are gone), active jobs moving to or from it, and
-- Cloudflare delivery reading from it.
SELECT
    (SELECT count(*) FROM assets a WHERE (a.storage_id = sqlc.arg(id)::text OR a.media_storage_id = sqlc.arg(id)::text)
        AND (a.status <> 'deleted' OR a.deleted_at IS NULL))::bigint AS assets,
    (SELECT count(*) FROM jobs j WHERE j.status IN ('queued', 'running')
        AND sqlc.arg(id)::text IN (j.source_storage_id, j.target_storage_id, j.source_media_storage_id, j.target_media_storage_id))::bigint AS jobs,
    EXISTS (SELECT 1 FROM projects p WHERE p.delivery_storage_id = sqlc.arg(id)::text AND p.delivery = 'worker')::boolean AS delivery;

-- name: RemoveStorageConfig :exec
UPDATE storage_configs SET removed_at = now(), secrets_encrypted = NULL WHERE id = $1 AND NOT is_default;

-- name: CreateStorageConfig :one
INSERT INTO storage_configs (id, project_id, kind, config, secrets_encrypted)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ClearDefaultStorage :exec
UPDATE storage_configs SET is_default = false WHERE project_id = $1 AND is_default;

-- name: SetDefaultStorage :execrows
UPDATE storage_configs SET is_default = true WHERE project_id = $1 AND id = $2 AND removed_at IS NULL;

-- name: UpdateStorageSecrets :execrows
UPDATE storage_configs SET secrets_encrypted = $2 WHERE id = $1 AND removed_at IS NULL;
