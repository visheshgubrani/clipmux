-- The renditions an uploader chose for an asset (D130): ladder names such
-- as '1080p', in ladder order. NULL is every rung up to the source. A
-- re-encode may change them; the claim hands them to the worker and the
-- commit checks the output against them.

-- +goose Up
ALTER TABLE assets ADD COLUMN renditions text[]
    CONSTRAINT assets_renditions_check CHECK (
        renditions IS NULL OR (
            cardinality(renditions) BETWEEN 1 AND 6
            AND renditions <@ ARRAY['2160p', '1440p', '1080p', '720p', '480p', '360p']::text[]
        )
    );

-- +goose Down
ALTER TABLE assets DROP COLUMN renditions;
