-- Connect Cloudflare (docs/architecture.md, "Optional upgrades", P2-T04).
-- The raw API token is never stored: only its ID, which with the sealed
-- R2 credentials (storage_configs) is all that later steps need.

-- +goose Up
CREATE TABLE cloudflare_connections (
    project_id     text PRIMARY KEY REFERENCES projects (id),
    account_id     text NOT NULL,
    zone_id        text NOT NULL,
    hostname       text NOT NULL,
    bucket         text NOT NULL,
    script_name    text NOT NULL,
    token_id       text NOT NULL,
    storage_id     text NOT NULL REFERENCES storage_configs (id),
    -- The embedded Worker build last deployed; serve asks for a token when
    -- the binary carries a newer one.
    worker_version text NOT NULL,
    connected_at   timestamptz NOT NULL DEFAULT now(),
    deployed_at    timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE cloudflare_connections;
