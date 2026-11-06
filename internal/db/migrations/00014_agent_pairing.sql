-- PC agents pair with a short code (P3-T01, D043). The owner either mints a
-- code in the dashboard and pastes the command on the PC, or runs
-- `clipmux worker --server URL` on the PC and approves the code it shows.
-- Either way the owner has approved the agent by the time it holds a key.

-- +goose Up
CREATE TABLE agent_pairings (
    id              text PRIMARY KEY,
    project_id      text NOT NULL REFERENCES projects (id),
    -- SHA-256 of the normalised code the owner sees, e.g. KXFT-9M2Q.
    code_hash       bytea NOT NULL UNIQUE,
    -- Set when the agent started the pairing: it polls with this secret.
    poll_token_hash bytea UNIQUE,
    status          text NOT NULL CHECK (status IN ('pending', 'approved', 'denied', 'consumed')),
    -- Who asked: the agent's own description, shown to the owner.
    name            text NOT NULL DEFAULT '',
    hostname        text NOT NULL DEFAULT '',
    platform        text NOT NULL DEFAULT '',
    ip              inet,
    agent_id        text REFERENCES agents (id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,
    CHECK ((status = 'consumed') = (agent_id IS NOT NULL))
);
CREATE INDEX agent_pairings_live ON agent_pairings (project_id, expires_at) WHERE status IN ('pending', 'approved');

ALTER TABLE agents
    ADD COLUMN hostname text NOT NULL DEFAULT '',
    ADD COLUMN platform text NOT NULL DEFAULT '',
    -- The trust rule moves to the claim (unsandboxed agents take pinned jobs
    -- and the owner's own uploads), so an agent can exist before it reports.
    DROP CONSTRAINT agents_check;

-- +goose Down
ALTER TABLE agents
    DROP COLUMN hostname,
    DROP COLUMN platform;
UPDATE agents SET scope = 'local' WHERE NOT sandboxed;
ALTER TABLE agents ADD CONSTRAINT agents_check CHECK (sandboxed OR scope = 'local');
DROP TABLE agent_pairings;
