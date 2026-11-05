-- The setup test (docs/architecture.md, "Setup test"): after any change, a
-- sample runs through the configured path and its playback is checked.

-- +goose Up
CREATE TABLE setup_tests (
    id          text PRIMARY KEY,
    project_id  text NOT NULL REFERENCES projects (id),
    status      text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'passed', 'failed')),
    trigger     text NOT NULL CHECK (trigger IN ('first_boot', 'urls_saved', 'manual', 'cli')),
    asset_id    text REFERENCES assets (id),
    -- [{name, ok, detail}]; details never hold a URL or a token.
    checks      jsonb NOT NULL DEFAULT '[]',
    created_at  timestamptz NOT NULL DEFAULT now(),
    started_at  timestamptz,
    finished_at timestamptz,
    CHECK ((status IN ('passed', 'failed')) = (finished_at IS NOT NULL))
);
-- One test at a time per project.
CREATE UNIQUE INDEX setup_tests_one_active ON setup_tests (project_id) WHERE status IN ('queued', 'running');

-- +goose Down
DROP TABLE setup_tests;
