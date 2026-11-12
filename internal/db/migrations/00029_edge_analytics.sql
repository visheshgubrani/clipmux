-- Edge analytics (P7-T05, D103): the read-only Cloudflare token serve pulls
-- the Worker's Analytics Engine data points with, and how far it has pulled.
-- Connect Cloudflare's own token is forgotten after setup, so this is a
-- second token, with only Account Analytics: Read. It is sealed under the
-- project's box (secrets.Columns). The row outlives a Cloudflare
-- disconnect, so the hours the Worker counted before it are still pulled.
--
-- pulled_through is the end of the last pass: each pass recomputes from an
-- hour before it, so points that land late still count, and replaces those
-- hours' edge rows.

-- +goose Up
CREATE TABLE edge_analytics (
    project_id     text        PRIMARY KEY REFERENCES projects (id),
    account_id     text        NOT NULL,
    token_sealed   bytea       NOT NULL,
    pulled_through timestamptz NOT NULL,
    last_pull_at   timestamptz,
    last_error     text,
    connected_at   timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE edge_analytics;
