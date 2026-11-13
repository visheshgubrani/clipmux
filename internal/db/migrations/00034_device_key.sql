-- The key that signs the owner's known-device cookie (D121): a device the
-- owner signed in from before skips the global sign-in limit, so failures
-- from other addresses can't lock the owner out. Sealed like the analytics
-- key, and created on first use.

-- +goose Up
ALTER TABLE projects ADD COLUMN device_key_sealed bytea;

-- +goose Down
ALTER TABLE projects DROP COLUMN device_key_sealed;
