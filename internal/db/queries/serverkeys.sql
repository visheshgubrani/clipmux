-- Server secrets, sealed with the secrets key (internal/secrets).

-- name: ListContentKeySecrets :many
SELECT version, secret_encrypted, (usable_at IS NOT NULL)::boolean AS usable FROM content_key_secrets WHERE project_id = $1 ORDER BY version;

-- name: InsertContentKeySecret :execrows
INSERT INTO content_key_secrets (project_id, version, secret_encrypted, usable_at) VALUES ($1, $2, $3, now())
ON CONFLICT (project_id, version) DO NOTHING;

-- name: InsertPendingContentKeySecret :one
-- A new version, not yet used for new outputs.
INSERT INTO content_key_secrets (project_id, version, secret_encrypted)
VALUES ($1, (SELECT coalesce(max(version), 0) + 1 FROM content_key_secrets WHERE project_id = $1), $2)
RETURNING version;

-- name: MakeContentKeysUsable :execrows
UPDATE content_key_secrets SET usable_at = now() WHERE project_id = $1 AND usable_at IS NULL;

-- name: ListOutputKeyVersions :many
-- Every key version an output still uses; each must stay derivable.
SELECT DISTINCT key_version FROM outputs WHERE project_id = $1 ORDER BY key_version;

-- name: GetInternalSigningKey :one
SELECT * FROM signing_keys
WHERE project_id = $1 AND app_id IS NULL AND retired_at IS NULL AND revoked_at IS NULL;

-- name: CreateInternalSigningKey :execrows
-- Does nothing if another serve created the project's key first.
INSERT INTO signing_keys (id, project_id, public_key_pem, private_key_encrypted) VALUES ($1, $2, $3, $4)
ON CONFLICT DO NOTHING;

-- name: SetContentKeySecret :exec
UPDATE content_key_secrets SET secret_encrypted = $3 WHERE project_id = $1 AND version = $2;
