-- name: CreateAPIKey :one
INSERT INTO api_keys (id, project_id, app_id, name, key_hash, scopes)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetAPIKeyByHash :one
-- A key works only while neither it nor its app is disabled.
SELECT k.id, k.project_id, k.app_id, k.scopes
FROM api_keys k JOIN apps a ON a.id = k.app_id
WHERE k.key_hash = $1 AND k.revoked_at IS NULL AND a.disabled_at IS NULL;

-- name: TouchAPIKey :exec
UPDATE api_keys SET last_used_at = now()
WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');

-- name: GetAppByName :one
SELECT * FROM apps WHERE project_id = $1 AND name = $2;
