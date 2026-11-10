-- Share links (P7-T06, D104). A link's state follows its expiry and its
-- playback ID's status, which the service reads together.

-- name: CreateShareLink :one
INSERT INTO share_links (id, project_id, app_id, asset_id, playback_id, name, expires_at, secret_hash, secret_sealed)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: ListShareLinks :many
-- An asset's links, newest first, with their views since a day: origin and
-- edge rows alike.
SELECT s.id, s.app_id, s.asset_id, s.playback_id, s.name, s.expires_at, s.secret_sealed, s.created_at,
       p.status AS playback_status,
       coalesce((SELECT sum(d.views) FROM playback_daily d
                 WHERE d.project_id = s.project_id AND d.asset_id = s.asset_id AND d.playback_id = s.playback_id
                   AND d.day >= sqlc.arg(since)::date), 0)::bigint AS views
FROM share_links s
JOIN playback_ids p ON p.id = s.playback_id
WHERE s.asset_id = sqlc.arg(asset_id) AND s.app_id = sqlc.arg(app_id)
ORDER BY s.created_at DESC, s.id DESC;

-- name: GetShareLink :one
SELECT s.id, s.project_id, s.app_id, s.asset_id, s.playback_id, s.name, s.expires_at, s.secret_sealed, s.created_at,
       p.status AS playback_status
FROM share_links s
JOIN playback_ids p ON p.id = s.playback_id
WHERE s.id = $1 AND s.app_id = $2;

-- name: GetShareBySecret :one
-- What the public page needs to answer: the link, its playback ID, and
-- whether its asset still plays.
SELECT s.id, s.project_id, s.app_id, s.asset_id, s.playback_id, s.expires_at,
       p.status AS playback_status,
       a.status AS asset_status, (ap.disabled_at IS NOT NULL)::boolean AS app_disabled
FROM share_links s
JOIN playback_ids p ON p.id = s.playback_id
JOIN assets a ON a.id = s.asset_id
JOIN apps ap ON ap.id = s.app_id
WHERE s.secret_hash = $1;

-- name: ListExpiredShareLinks :many
-- Links past their expiry whose playback ID still plays, for the expiry loop.
SELECT s.id, s.project_id, s.app_id, s.asset_id, s.playback_id
FROM share_links s
JOIN playback_ids p ON p.id = s.playback_id
WHERE s.expires_at <= sqlc.arg(now)::timestamptz AND p.status = 'active'
ORDER BY s.expires_at, s.id
LIMIT sqlc.arg(max_links);

-- name: ListPreviewPlaybackIDs :many
-- An asset's playback IDs a dashboard preview may use: share links' own
-- IDs are left out, so revoking a link never ends a preview.
SELECT p.* FROM playback_ids p
WHERE p.asset_id = $1 AND p.status <> 'revoked'
  AND NOT EXISTS (SELECT 1 FROM share_links s WHERE s.playback_id = p.id)
ORDER BY p.created_at, p.id;
