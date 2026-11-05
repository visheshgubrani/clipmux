-- Storage configs (docs/architecture.md, "Per-asset storage").

-- name: GetStorageConfig :one
SELECT * FROM storage_configs WHERE id = $1;

-- name: AssetStorageID :one
SELECT storage_id FROM assets WHERE id = $1;

-- name: ListStorageConfigs :many
SELECT * FROM storage_configs WHERE project_id = $1 ORDER BY created_at, id;

-- name: CreateStorageConfig :one
INSERT INTO storage_configs (id, project_id, kind, config, secrets_encrypted)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ClearDefaultStorage :exec
UPDATE storage_configs SET is_default = false WHERE project_id = $1 AND is_default;

-- name: SetDefaultStorage :execrows
UPDATE storage_configs SET is_default = true WHERE project_id = $1 AND id = $2;

-- name: UpdateStorageSecrets :exec
UPDATE storage_configs SET secrets_encrypted = $2 WHERE id = $1;
