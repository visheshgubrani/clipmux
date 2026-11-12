-- App pairings (P6-T23): the device flow behind `npx @clipmux/node init`.

-- name: CreateAppPairing :one
INSERT INTO app_pairings (id, project_id, code_hash, poll_token_hash, status, name, hostname, client, ip, expires_at)
VALUES ($1, $2, $3, $4, 'pending', $5, $6, $7, $8, now() + make_interval(secs => sqlc.arg(ttl_seconds)::double precision))
RETURNING *;

-- name: CountLiveAppPairings :one
-- Pairings still waiting for the owner, which bound how many an
-- unauthenticated caller can open.
SELECT count(*) FROM app_pairings
WHERE project_id = $1 AND status = 'pending' AND expires_at > now();

-- name: GetAppPairingByCode :one
SELECT * FROM app_pairings WHERE project_id = $1 AND code_hash = $2;

-- name: LockLiveAppPairingByCode :one
SELECT * FROM app_pairings
WHERE project_id = $1 AND code_hash = $2 AND status = 'pending' AND expires_at > now()
FOR UPDATE;

-- name: LockAppPairingByPollToken :one
SELECT *, (expires_at <= now())::boolean AS expired FROM app_pairings
WHERE poll_token_hash = $1
FOR UPDATE;

-- name: DenyAppPairing :exec
UPDATE app_pairings SET status = 'denied' WHERE id = $1;

-- name: ApproveAppPairing :one
-- The CLI polls every couple of seconds; a pairing approved in its last
-- moments keeps a minute to be collected in.
UPDATE app_pairings
SET status = 'approved', app_id = $2, credentials_sealed = $3,
    expires_at = greatest(expires_at, now() + interval '1 minute')
WHERE id = $1
RETURNING *;

-- name: ConsumeAppPairing :exec
UPDATE app_pairings SET status = 'consumed', credentials_sealed = NULL WHERE id = $1;

-- name: ExpireAppPairing :exec
-- Approved but never collected: the credentials are dropped, not kept.
UPDATE app_pairings SET status = 'consumed', credentials_sealed = NULL WHERE id = $1 AND status = 'approved';

-- name: PruneAppPairings :execrows
DELETE FROM app_pairings WHERE id IN (
    SELECT p.id FROM app_pairings p WHERE p.expires_at < sqlc.arg(before) LIMIT sqlc.arg(max_rows));

-- name: GetIntegrationSettings :one
-- GET /v1/app: the URLs an integration needs and the longest token
-- delivery accepts.
SELECT coalesce(public_url, '')::text AS public_url,
       coalesce(media_url, public_url, '')::text AS base_url,
       max_token_lifetime_seconds
FROM projects WHERE id = $1;

-- name: ListAppPublicKeys :many
-- An app's signing keys that still verify tokens: not revoked, not past
-- retirement.
SELECT id, public_key_pem, created_at, retired_at FROM signing_keys
WHERE app_id = $1 AND revoked_at IS NULL AND (retired_at IS NULL OR retired_at > now())
ORDER BY created_at DESC, id DESC;
