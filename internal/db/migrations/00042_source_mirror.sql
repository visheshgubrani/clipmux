-- The source mirror (T29.7, D146). Each committed source is copied, encrypted,
-- to the backup target; source_backed_up_at is when it was. A deleted asset
-- that had been copied gets a tombstone on the target, and
-- source_tombstoned_at is when that was written, so the loop writes each once.

-- +goose Up
ALTER TABLE assets
    ADD COLUMN source_backed_up_at   timestamptz,
    ADD COLUMN source_tombstoned_at  timestamptz;

-- What the loop looks for: committed sources not yet copied, oldest first.
CREATE INDEX assets_source_unmirrored ON assets (created_at)
    WHERE source_backed_up_at IS NULL AND status IN ('processing', 'ready', 'errored');

-- Assets that were copied and since deleted, whose tombstone isn't written.
CREATE INDEX assets_source_untombstoned ON assets (created_at)
    WHERE source_backed_up_at IS NOT NULL AND source_tombstoned_at IS NULL AND status = 'deleted';

-- +goose Down
DROP INDEX assets_source_untombstoned;
DROP INDEX assets_source_unmirrored;
ALTER TABLE assets
    DROP COLUMN source_tombstoned_at,
    DROP COLUMN source_backed_up_at;
