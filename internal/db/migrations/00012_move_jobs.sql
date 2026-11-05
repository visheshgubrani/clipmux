-- The move job (docs/architecture.md, "Per-asset storage", P2-T05) copies an
-- asset between stores in the control plane; it records both ends.

-- +goose Up
ALTER TABLE jobs
    ADD COLUMN source_storage_id text REFERENCES storage_configs (id),
    ADD COLUMN target_storage_id text REFERENCES storage_configs (id),
    ADD CONSTRAINT jobs_move_has_stores CHECK (kind <> 'move' OR (source_storage_id IS NOT NULL AND target_storage_id IS NOT NULL));

-- +goose Down
ALTER TABLE jobs DROP CONSTRAINT jobs_move_has_stores, DROP COLUMN target_storage_id, DROP COLUMN source_storage_id;
