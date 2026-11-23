-- What the owner asks of backups and what they last answered (T29.8, D143).
-- backup_requested_at is "back up now" from the dashboard: serve's leader takes
-- the backup on its next pass, since only it runs pg_dump. backup_error is why
-- the newest attempt failed, cleared by the next success. The recovery kit is
-- the secrets key and DATABASE_URL, which a backup can't hold; the owner says
-- when they have put a copy somewhere that isn't this server.

-- +goose Up
ALTER TABLE projects
    ADD COLUMN backup_requested_at        timestamptz,
    ADD COLUMN backup_error               text,
    ADD COLUMN backup_error_at            timestamptz,
    ADD COLUMN recovery_kit_confirmed_at  timestamptz;

-- +goose Down
ALTER TABLE projects
    DROP COLUMN recovery_kit_confirmed_at,
    DROP COLUMN backup_error_at,
    DROP COLUMN backup_error,
    DROP COLUMN backup_requested_at;
