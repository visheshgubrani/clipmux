-- Webhook delivery, asset deletion and catch-up (P1-T09).
--
-- An outbox webhook row is one event; it fans out to one delivery per enabled
-- endpoint of its app, retried with backoff until 24 h after the event.

-- +goose Up
-- A value sealed with the secrets key on first boot. serve refuses to start
-- with a key that can't open it, rather than sealing new secrets under a key
-- that can't open the old ones.
ALTER TABLE projects ADD COLUMN secrets_check bytea;

CREATE TABLE webhook_deliveries (
    id              text PRIMARY KEY,
    project_id      text NOT NULL REFERENCES projects (id),
    event_id        text NOT NULL REFERENCES outbox (id),
    endpoint_id     text NOT NULL REFERENCES webhook_endpoints (id),
    status          text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'delivered', 'failed')),
    attempts        integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    -- When the next try is due; a claimed delivery holds a short lease here,
    -- so a sender that dies mid-request is retried.
    next_attempt_at timestamptz,
    give_up_at      timestamptz NOT NULL,
    last_status     integer,
    -- A short code such as http_500 or timeout, never a response body.
    last_error      text CHECK (length(last_error) <= 200),
    created_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz,
    UNIQUE (event_id, endpoint_id),
    CHECK ((status = 'pending') = (next_attempt_at IS NOT NULL)),
    CHECK ((status = 'pending') = (finished_at IS NULL))
);
CREATE INDEX webhook_deliveries_due ON webhook_deliveries (next_attempt_at) WHERE status = 'pending';
CREATE INDEX webhook_endpoints_app ON webhook_endpoints (app_id) WHERE disabled_at IS NULL;

-- GET /v1/assets?updated_since= pages by (updated_at, id) within an app.
CREATE INDEX assets_app_updated ON assets (app_id, updated_at, id);

-- Control-plane jobs (move, delete) are run by serve, never claimed by workers.
CREATE INDEX jobs_control_due ON jobs (available_at) WHERE kind IN ('move', 'delete') AND status IN ('queued', 'running');

-- +goose Down
DROP INDEX jobs_control_due;
DROP INDEX assets_app_updated;
DROP INDEX webhook_endpoints_app;
DROP TABLE webhook_deliveries;
ALTER TABLE projects DROP COLUMN secrets_check;
