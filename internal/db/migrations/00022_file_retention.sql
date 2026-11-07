-- File retention (P5-T07, D070): retired outputs and the files of attempts
-- that were replaced are deleted once nothing can still play or write them,
-- and an attempt that writes no output (upload_source) gets none.

-- +goose Up
-- When an output's files were deleted. Delivery and route records treat a
-- purged output as gone; its row stays, as history.
ALTER TABLE outputs ADD COLUMN purged_at timestamptz;
CREATE INDEX outputs_retired_unpurged ON outputs (retired_at) WHERE status = 'retired' AND purged_at IS NULL;
CREATE INDEX outputs_writing_unpurged ON outputs (created_at) WHERE status = 'writing' AND purged_at IS NULL;

-- An upload_source attempt uploads a source, not an output.
ALTER TABLE attempts ALTER COLUMN output_id DROP NOT NULL;
-- An empty writing output its upload_source attempt never used.
UPDATE outputs o SET purged_at = now()
FROM attempts a JOIN jobs j ON j.id = a.job_id
WHERE a.output_id = o.id AND j.kind = 'upload_source' AND o.status = 'writing';

-- Where the daily orphan sweep left off.
CREATE TABLE retention_cursors (
    project_id text NOT NULL REFERENCES projects (id),
    name       text NOT NULL,
    value      text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, name)
);

-- +goose Down
DROP TABLE retention_cursors;
INSERT INTO outputs (id, project_id, asset_id, key_version)
SELECT 'out_' || substr(a.id, 5), a.project_id, a.asset_id, 1 FROM attempts a WHERE a.output_id IS NULL;
UPDATE attempts SET output_id = 'out_' || substr(id, 5) WHERE output_id IS NULL;
ALTER TABLE attempts ALTER COLUMN output_id SET NOT NULL;
DROP INDEX outputs_writing_unpurged;
DROP INDEX outputs_retired_unpurged;
ALTER TABLE outputs DROP COLUMN purged_at;
