-- Built-in delivery (docs/architecture.md, "Delivery"). The built-in handler
-- reads routes straight from Postgres.

-- name: GetRoute :one
SELECT p.id AS playback_id, p.policy, p.status AS playback_status,
       a.id AS asset_id, a.app_id, a.status AS asset_status, a.current_output_id,
       pr.max_token_lifetime_seconds
FROM playback_ids p
JOIN assets a ON a.id = p.asset_id
JOIN projects pr ON pr.id = a.project_id
WHERE p.id = $1;

-- name: CreateSigningKey :one
INSERT INTO signing_keys (id, project_id, app_id, public_key_pem)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetSigningKey :one
SELECT * FROM signing_keys WHERE id = $1;
