-- Restore fence and store epoch (D144).

-- name: EnsureInstallEpoch :one
-- This install's ID and generation, made on first use if migration didn't
-- (a project created since).
INSERT INTO install_epoch (project_id) VALUES ($1)
ON CONFLICT (project_id) DO UPDATE SET project_id = install_epoch.project_id
RETURNING *;

-- name: AdvanceInstallEpoch :one
UPDATE install_epoch SET generation = generation + 1, updated_at = now()
WHERE project_id = $1
RETURNING *;

-- name: RaiseInstallEpoch :exec
-- Moves the generation up to at least $2, past every marker a store holds.
UPDATE install_epoch SET generation = greatest(generation, sqlc.arg(generation)::bigint), updated_at = now()
WHERE project_id = sqlc.arg(project_id);

-- name: FenceActive :one
SELECT EXISTS (SELECT 1 FROM restore_fence WHERE project_id = $1 AND lifted_at IS NULL)::boolean;

-- name: GetActiveFence :one
SELECT * FROM restore_fence WHERE project_id = $1 AND lifted_at IS NULL;

-- name: EngageFence :one
-- Raises the fence, unless one is up already (then no row comes back).
INSERT INTO restore_fence (project_id, reason, detail, backup_id, backup_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (project_id) WHERE lifted_at IS NULL DO NOTHING
RETURNING *;

-- name: LiftFence :execrows
UPDATE restore_fence SET lifted_at = now() WHERE project_id = $1 AND lifted_at IS NULL;

-- name: ListAssetIDs :many
SELECT id FROM assets WHERE project_id = $1;

-- name: ListOutputIDs :many
SELECT id FROM outputs WHERE project_id = $1;

-- name: ListPlaybackIDStrings :many
SELECT id FROM playback_ids WHERE project_id = $1;

-- name: ListSigningKeyIDs :many
-- Every signing key the database has a row for, revoked or not.
SELECT id FROM signing_keys WHERE project_id = $1;
