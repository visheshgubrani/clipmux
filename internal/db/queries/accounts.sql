-- The owner account and its dashboard sessions.

-- name: OwnerExists :one
SELECT EXISTS (SELECT 1 FROM owners WHERE project_id = $1);

-- name: CreateOwner :one
INSERT INTO owners (id, project_id, email, password_hash) VALUES ($1, $2, $3, $4)
ON CONFLICT (project_id) DO NOTHING
RETURNING *;

-- name: GetOwnerByEmail :one
SELECT * FROM owners WHERE project_id = sqlc.arg(project_id) AND lower(email) = lower(sqlc.arg(email));

-- name: GetOwner :one
SELECT * FROM owners WHERE project_id = $1;

-- name: SetOwnerPassword :exec
UPDATE owners SET password_hash = $2, password_changed_at = now() WHERE id = $1;

-- name: CreateOwnerSession :exec
INSERT INTO owner_sessions (token_hash, project_id, owner_id, expires_at)
VALUES ($1, $2, $3, now() + make_interval(secs => sqlc.arg(lifetime_seconds)::double precision));

-- name: GetOwnerSession :one
-- A session ends at its expiry, or after idle_seconds without a request.
SELECT s.owner_id, s.project_id, o.email, (s.last_seen_at < now() - interval '1 minute')::boolean AS stale
FROM owner_sessions s JOIN owners o ON o.id = s.owner_id
WHERE s.token_hash = sqlc.arg(token_hash) AND s.expires_at > now()
  AND s.last_seen_at > now() - make_interval(secs => sqlc.arg(idle_seconds)::double precision);

-- name: TouchOwnerSession :exec
UPDATE owner_sessions SET last_seen_at = now() WHERE token_hash = $1;

-- name: DeleteOwnerSession :exec
DELETE FROM owner_sessions WHERE token_hash = $1;

-- name: DeleteOwnerSessions :execrows
DELETE FROM owner_sessions WHERE owner_id = $1;

-- name: GetOwnerTOTP :one
SELECT id, password_hash, totp_secret_encrypted, totp_enabled_at, totp_last_step FROM owners WHERE id = $1;

-- name: SetOwnerTOTPSecret :exec
-- A secret being enrolled: kept until a code proves the authenticator has it.
UPDATE owners SET totp_secret_encrypted = $2, totp_enabled_at = NULL, totp_last_step = NULL WHERE id = $1;

-- name: EnableOwnerTOTP :execrows
UPDATE owners SET totp_enabled_at = now(), totp_last_step = $2 WHERE id = $1 AND totp_secret_encrypted IS NOT NULL;

-- name: AcceptTOTPStep :execrows
-- Fenced on the last step, so the same code can't sign in twice.
UPDATE owners SET totp_last_step = sqlc.arg(step)
WHERE id = sqlc.arg(id) AND (totp_last_step IS NULL OR totp_last_step < sqlc.arg(step));

-- name: DisableOwnerTOTP :execrows
UPDATE owners SET totp_secret_encrypted = NULL, totp_enabled_at = NULL, totp_last_step = NULL WHERE project_id = $1;

-- name: DeleteRecoveryCodes :exec
DELETE FROM owner_recovery_codes WHERE owner_id = $1;

-- name: InsertRecoveryCode :exec
INSERT INTO owner_recovery_codes (project_id, owner_id, code_hash) VALUES ($1, $2, $3);

-- name: UseRecoveryCode :execrows
UPDATE owner_recovery_codes SET used_at = now() WHERE owner_id = $1 AND code_hash = $2 AND used_at IS NULL;

-- name: DeleteOtherOwnerSessions :exec
DELETE FROM owner_sessions WHERE owner_id = $1 AND token_hash <> $2;
