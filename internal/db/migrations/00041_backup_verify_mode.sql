-- How a backup was checked (T29.6, D143). verified_at and verify_error already
-- say when and with what result; the mode says how far the check went: 'restore'
-- loaded the dump into a scratch database and read it, 'integrity' only proved
-- the file (its hash, its decryption, and that pg_restore reads it), which is
-- all a role that can't create databases allows.

-- +goose Up
ALTER TABLE backups ADD COLUMN verify_mode text CHECK (verify_mode IN ('restore', 'integrity'));

-- +goose Down
ALTER TABLE backups DROP COLUMN verify_mode;
