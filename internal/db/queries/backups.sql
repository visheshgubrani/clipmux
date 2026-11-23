-- Backups ledger (D143): one row per dump written to the backup target.

-- name: InsertBackup :exec
-- No RETURNING *: a backup is recorded before the migrations that add columns
-- to this table have run, and must work against the older table.
INSERT INTO backups (id, project_id, kind, object_key, size_bytes, sha256, clipmux_version, schema_version, postgres_version, secrets_key_version, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11);

-- name: LastBackup :one
SELECT * FROM backups WHERE project_id = $1 ORDER BY created_at DESC LIMIT 1;

-- name: ListBackups :many
SELECT * FROM backups WHERE project_id = $1 ORDER BY created_at DESC LIMIT $2;

-- name: RecordBackupVerification :execrows
-- The result of checking one backup: when, how far, and what went wrong.
UPDATE backups SET verified_at = sqlc.arg(verified_at), verify_mode = sqlc.arg(verify_mode), verify_error = sqlc.narg(verify_error)
WHERE project_id = sqlc.arg(project_id) AND id = sqlc.arg(id);

-- name: LastBackupVerification :one
-- The newest check, a failed one included.
SELECT * FROM backups WHERE project_id = $1 AND verified_at IS NOT NULL ORDER BY verified_at DESC LIMIT 1;

-- name: RequestBackup :exec
-- The time is the server's, as a backup's created_at is, so the two compare.
UPDATE projects SET backup_requested_at = sqlc.arg(requested_at) WHERE id = sqlc.arg(id);

-- name: GetBackupState :one
SELECT backup_requested_at, backup_error, backup_error_at, recovery_kit_confirmed_at FROM projects WHERE id = $1;

-- name: SetBackupError :exec
-- The newest attempt's failure; a null message clears it.
UPDATE projects SET backup_error = sqlc.narg(message), backup_error_at = CASE WHEN sqlc.narg(message)::text IS NULL THEN NULL ELSE now() END
WHERE id = sqlc.arg(id);

-- name: SetRecoveryKitConfirmed :exec
UPDATE projects SET recovery_kit_confirmed_at = CASE WHEN sqlc.arg(confirmed)::boolean THEN now() ELSE NULL END
WHERE id = sqlc.arg(id);
