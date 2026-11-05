-- Project-wide settings (docs/architecture.md, "Accounts and tenancy").

-- name: GetSecretsCheck :one
SELECT secrets_check FROM projects WHERE id = $1;

-- name: SetSecretsCheck :execrows
-- Only the first boot writes it; later boots verify.
UPDATE projects SET secrets_check = $2 WHERE id = $1 AND secrets_check IS NULL;
