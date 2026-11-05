-- Project-wide settings (docs/architecture.md, "Accounts and tenancy").

-- name: GetSecretsCheck :one
SELECT secrets_check FROM projects WHERE id = $1;

-- name: SetSecretsCheck :execrows
-- Only the first boot writes it; later boots verify.
UPDATE projects SET secrets_check = $2 WHERE id = $1 AND secrets_check IS NULL;

-- name: GetProjectURLs :one
SELECT public_url, media_url, urls_confirmed_at FROM projects WHERE id = $1;

-- name: InitProjectURLs :exec
-- First boot: the defaults, until the owner confirms real ones.
UPDATE projects SET public_url = coalesce(public_url, $2), media_url = coalesce(media_url, $3) WHERE id = $1;

-- name: SetProjectURLs :exec
UPDATE projects SET public_url = $2, media_url = $3, urls_confirmed_at = now() WHERE id = $1;

-- name: GetMediaURL :one
-- base_url in asset views and webhook events.
SELECT coalesce(media_url, public_url, '')::text FROM projects WHERE id = $1;
