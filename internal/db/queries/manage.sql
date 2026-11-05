-- The owner's management API: apps, their keys and endpoints, the library.

-- name: ListApps :many
SELECT * FROM apps WHERE project_id = $1 ORDER BY is_builtin DESC, created_at, id;

-- name: GetApp :one
SELECT * FROM apps WHERE project_id = $1 AND id = $2;

-- name: SetAppDisabled :one
-- The built-in Dashboard app can't be disabled: dashboard uploads need it.
UPDATE apps SET disabled_at = CASE WHEN sqlc.arg(disabled)::boolean THEN coalesce(disabled_at, now()) END
WHERE project_id = sqlc.arg(project_id) AND id = sqlc.arg(id) AND NOT is_builtin
RETURNING *;

-- name: ListAPIKeys :many
SELECT id, name, scopes, created_at, last_used_at, revoked_at FROM api_keys
WHERE app_id = $1 ORDER BY created_at DESC, id DESC;

-- name: RevokeAPIKey :execrows
UPDATE api_keys SET revoked_at = coalesce(revoked_at, now()) WHERE project_id = $1 AND id = $2;

-- name: ListSigningKeys :many
SELECT id, created_at, retired_at, revoked_at FROM signing_keys
WHERE app_id = $1 ORDER BY created_at DESC, id DESC;

-- name: RevokeSigningKey :execrows
-- App keys only; the internal key is the server's.
UPDATE signing_keys SET revoked_at = coalesce(revoked_at, now())
WHERE project_id = $1 AND id = $2 AND app_id IS NOT NULL;

-- name: ListWebhookEndpoints :many
SELECT id, url, created_at, disabled_at FROM webhook_endpoints
WHERE app_id = $1 ORDER BY created_at DESC, id DESC;

-- name: DisableWebhookEndpoint :execrows
UPDATE webhook_endpoints SET disabled_at = coalesce(disabled_at, now()) WHERE project_id = $1 AND id = $2;

-- name: ListLibrary :many
-- Every app's assets, newest first, keyset-paged on (created_at, id).
SELECT * FROM assets
WHERE project_id = sqlc.arg(project_id)
  AND (sqlc.narg(app_id)::text IS NULL OR app_id = sqlc.narg(app_id))
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
  AND (created_at, id) < (sqlc.arg(before_time)::timestamptz, sqlc.arg(before_id)::text)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(max_assets);

-- name: GetAssetInProject :one
SELECT * FROM assets WHERE project_id = $1 AND id = $2;
