-- Removing a storage config (P6-T12, D099). Assets, move jobs, the
-- Cloudflare connection and the project's delivery keep referring to a
-- store long after it holds nothing, so a removed config stays as a row:
-- marked removed, its sealed credentials dropped, never listed, never the
-- default and never chosen again.

-- +goose Up
ALTER TABLE storage_configs ADD COLUMN removed_at timestamptz,
    ADD CONSTRAINT storage_configs_removed_not_default CHECK (removed_at IS NULL OR NOT is_default);

-- +goose Down
-- A removed config has no credentials; it comes back as a row that can't
-- open, as it was before removal was possible only by hand.
ALTER TABLE storage_configs DROP CONSTRAINT storage_configs_removed_not_default,
    DROP COLUMN removed_at;
