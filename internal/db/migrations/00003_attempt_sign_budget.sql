-- Write path checks (docs/architecture.md): sign caps the file count per
-- attempt, so a runaway or hostile worker can't fill storage with names.

-- +goose Up
ALTER TABLE attempts ADD COLUMN signed_paths integer NOT NULL DEFAULT 0 CHECK (signed_paths >= 0);

-- +goose Down
ALTER TABLE attempts DROP COLUMN signed_paths;
