-- Route records and _keys.json (docs/architecture.md, "Route records").
-- They are a projection of Postgres: what the built-in handler would serve.

-- name: GetProjectDelivery :one
SELECT delivery, delivery_storage_id FROM projects WHERE id = $1;

-- name: SetProjectDelivery :exec
UPDATE projects SET delivery = sqlc.arg(delivery), delivery_storage_id = sqlc.narg(delivery_storage_id)
WHERE id = sqlc.arg(id);

-- name: ListServableOutputs :many
-- Pinned paths and keys may name only these outputs.
SELECT id, key_version FROM outputs
WHERE asset_id = $1 AND status IN ('committed', 'retired')
ORDER BY id;

-- name: ListServableRoutes :many
-- Every playback ID the built-in handler would serve, for the reconciler.
SELECT p.id AS playback_id, p.policy, a.id AS asset_id, a.app_id, a.current_output_id, a.storage_id
FROM playback_ids p
JOIN assets a ON a.id = p.asset_id
JOIN apps ap ON ap.id = a.app_id
WHERE p.project_id = $1 AND p.status = 'active' AND a.status <> 'deleted'
  AND a.current_output_id IS NOT NULL AND ap.disabled_at IS NULL
ORDER BY p.id;

-- name: ListServableOutputsForProject :many
SELECT o.asset_id, o.id, o.key_version FROM outputs o
JOIN assets a ON a.id = o.asset_id
WHERE a.project_id = $1 AND o.status IN ('committed', 'retired')
ORDER BY o.asset_id, o.id;

-- name: ListPublishedKeys :many
-- Keys delivery accepts now: not revoked, and not yet past their retirement.
SELECT k.id, k.app_id, k.public_key_pem, k.retired_at
FROM signing_keys k
WHERE k.project_id = $1 AND k.revoked_at IS NULL AND (k.retired_at IS NULL OR k.retired_at > now())
ORDER BY k.id;

-- name: GetProjectTokenLifetime :one
SELECT max_token_lifetime_seconds FROM projects WHERE id = $1;

-- name: LockPendingKeySyncs :many
SELECT * FROM outbox
WHERE kind = 'keys_sync' AND done_at IS NULL AND available_at <= now()
ORDER BY created_at, id
FOR UPDATE SKIP LOCKED;

-- name: ListActivePlaybackIDsForApp :many
SELECT p.id, p.asset_id FROM playback_ids p
JOIN assets a ON a.id = p.asset_id
WHERE a.app_id = $1 AND p.status = 'active' AND a.status <> 'deleted'
ORDER BY p.id;
