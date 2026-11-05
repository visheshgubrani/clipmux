-- The upload sweeper visits uploads least-recently-swept first, so one that
-- keeps failing can't hold the head of every pass and starve the rest.

-- +goose Up
ALTER TABLE uploads ADD COLUMN last_swept_at timestamptz;
CREATE INDEX uploads_active_sweep ON uploads (last_swept_at NULLS FIRST, last_activity_at) WHERE status = 'active';

-- +goose Down
DROP INDEX uploads_active_sweep;
ALTER TABLE uploads DROP COLUMN last_swept_at;
