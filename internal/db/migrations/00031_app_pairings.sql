-- App pairings (P6-T23): the device flow behind `npx @clipmux/node init`.
-- The CLI starts a pairing and polls with a secret; the owner approves it on
-- the dashboard's /connect page, which issues the app's credentials (an API
-- key, a signing key and optionally a webhook endpoint) and seals them here.
-- The first poll after approval opens them, hands them over and clears the
-- column, so they are readable exactly once (secrets.Columns).

-- +goose Up
CREATE TABLE app_pairings (
    id                 text        PRIMARY KEY,
    project_id         text        NOT NULL REFERENCES projects (id),
    -- SHA-256 of the normalised code both screens show, e.g. KXFT-9M2Q.
    code_hash          bytea       NOT NULL UNIQUE CHECK (length(code_hash) = 32),
    poll_token_hash    bytea       NOT NULL UNIQUE CHECK (length(poll_token_hash) = 32),
    status             text        NOT NULL CHECK (status IN ('pending', 'approved', 'denied', 'consumed')),
    -- What the CLI said about itself, shown to the owner.
    name               text        NOT NULL DEFAULT '' CHECK (char_length(name) <= 100),
    hostname           text        NOT NULL DEFAULT '',
    client             text        NOT NULL DEFAULT '',
    ip                 inet,
    app_id             text        REFERENCES apps (id),
    credentials_sealed bytea,
    created_at         timestamptz NOT NULL DEFAULT now(),
    expires_at         timestamptz NOT NULL,
    CHECK ((status = 'approved') = (credentials_sealed IS NOT NULL)),
    CHECK ((status IN ('approved', 'consumed')) = (app_id IS NOT NULL))
);
CREATE INDEX app_pairings_live ON app_pairings (project_id, expires_at) WHERE status IN ('pending', 'approved');

-- +goose Down
DROP TABLE app_pairings;
