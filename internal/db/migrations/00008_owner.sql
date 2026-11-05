-- The owner account (docs/architecture.md, "Accounts and tenancy"): one per
-- project, created with the setup token, after which registration closes.

-- +goose Up
CREATE TABLE owners (
    id                  text PRIMARY KEY,
    -- One owner per project: a second registration fails on this key.
    project_id          text NOT NULL UNIQUE REFERENCES projects (id),
    email               text NOT NULL CHECK (length(email) BETWEEN 3 AND 320),
    -- pbkdf2-sha256$iterations$salt$hash (base64, no padding).
    password_hash       text NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    password_changed_at timestamptz NOT NULL DEFAULT now()
);

-- Dashboard sessions; the cookie holds a secret, only its SHA-256 is kept.
CREATE TABLE owner_sessions (
    token_hash   bytea PRIMARY KEY,
    project_id   text NOT NULL REFERENCES projects (id),
    owner_id     text NOT NULL REFERENCES owners (id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL
);
CREATE INDEX owner_sessions_owner ON owner_sessions (owner_id);
CREATE INDEX owner_sessions_expiry ON owner_sessions (expires_at);

-- +goose Down
DROP TABLE owner_sessions;
DROP TABLE owners;
