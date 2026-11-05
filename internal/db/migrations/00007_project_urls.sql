-- First boot sets the public URL and the media URL (P1-T10). The media URL
-- is an origin, not a bare hostname, so dev setups keep their scheme and
-- port. urls_confirmed_at is when the owner (or the environment) set them.

-- +goose Up
ALTER TABLE projects RENAME COLUMN media_hostname TO media_url;
ALTER TABLE projects ADD COLUMN urls_confirmed_at timestamptz;

-- +goose Down
ALTER TABLE projects DROP COLUMN urls_confirmed_at;
ALTER TABLE projects RENAME COLUMN media_url TO media_hostname;
