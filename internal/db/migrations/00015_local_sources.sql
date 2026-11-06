-- `clipmux push --local` (P3-T05): a source that stays on the PC agent that
-- registered it. The server knows only its ID, size and agent; the path
-- stays on the agent. A source belongs to at most one asset.

-- +goose Up
CREATE UNIQUE INDEX assets_agent_source_once ON assets (agent_source_id) WHERE agent_source_id IS NOT NULL;
ALTER TABLE assets ADD CONSTRAINT assets_agent_source_uri
    CHECK (agent_source_id IS NULL OR source_uri = 'agent://' || split_part(source_uri, '/', 3) || '/' || agent_source_id);
CREATE INDEX jobs_pinned_queued ON jobs (pinned_agent_id) WHERE status = 'queued' AND pinned_agent_id IS NOT NULL;

-- +goose Down
DROP INDEX jobs_pinned_queued;
ALTER TABLE assets DROP CONSTRAINT assets_agent_source_uri;
DROP INDEX assets_agent_source_once;
