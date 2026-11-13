-- When an output's files were checked against its manifest after commit
-- (D124): a worker's PUT URLs outlive the commit by up to 15 minutes, so
-- files could be added after verification. The file_retention loop deletes
-- what the manifest doesn't list once nothing can write there, and records
-- it here so each output is swept once. Outputs committed before this are
-- swept on the first passes.

-- +goose Up
ALTER TABLE outputs ADD COLUMN unlisted_swept_at timestamptz;
CREATE INDEX outputs_unswept ON outputs (committed_at)
    WHERE status IN ('committed', 'retired') AND purged_at IS NULL AND unlisted_swept_at IS NULL;

-- +goose Down
DROP INDEX outputs_unswept;
ALTER TABLE outputs DROP COLUMN unlisted_swept_at;
