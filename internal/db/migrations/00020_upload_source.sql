-- push --local --upload-source (P4-T07, D059): once a local source has
-- been encoded, its PC uploads it to storage, so later re-encodes can run
-- on any worker instead of only on that PC.

-- +goose Up
-- The owner asked for the file to be uploaded after its first encode.
ALTER TABLE agent_sources ADD COLUMN upload_source boolean NOT NULL DEFAULT false;

-- upload_source jobs run on the PC that holds the file: always pinned,
-- always routed to agents.
ALTER TABLE jobs DROP CONSTRAINT jobs_kind_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_kind_check
    CHECK (kind IN ('import', 'transcode', 'reencode', 'move', 'delete', 'upload_source'));
ALTER TABLE jobs DROP CONSTRAINT jobs_media_routed;
ALTER TABLE jobs ADD CONSTRAINT jobs_media_routed
    CHECK ((routed_to IS NOT NULL) = (kind IN ('transcode', 'reencode', 'upload_source')));
ALTER TABLE jobs ADD CONSTRAINT jobs_upload_source_pinned
    CHECK (kind <> 'upload_source' OR pinned_agent_id IS NOT NULL);

-- The multipart upload an upload_source attempt writes the source into,
-- and its part size (the filesystem store derives it from the file's size).
ALTER TABLE attempts ADD COLUMN source_multipart_id text, ADD COLUMN source_part_size bigint;

-- +goose Down
ALTER TABLE attempts DROP COLUMN source_part_size, DROP COLUMN source_multipart_id;
DELETE FROM attempts WHERE job_id IN (SELECT id FROM jobs WHERE kind = 'upload_source');
DELETE FROM jobs WHERE kind = 'upload_source';
ALTER TABLE jobs DROP CONSTRAINT jobs_upload_source_pinned;
ALTER TABLE jobs DROP CONSTRAINT jobs_media_routed;
ALTER TABLE jobs ADD CONSTRAINT jobs_media_routed CHECK ((routed_to IS NOT NULL) = (kind IN ('transcode', 'reencode')));
ALTER TABLE jobs DROP CONSTRAINT jobs_kind_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_kind_check CHECK (kind IN ('import', 'transcode', 'reencode', 'move', 'delete'));
ALTER TABLE agent_sources DROP COLUMN upload_source;
