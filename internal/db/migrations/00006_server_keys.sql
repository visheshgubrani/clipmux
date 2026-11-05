-- Server secrets in Postgres, sealed with the secrets key (P1-T10): every
-- version of the content key secret an output still uses, and the server's
-- own token-signing key.

-- +goose Up
CREATE TABLE content_key_secrets (
    project_id       text NOT NULL REFERENCES projects (id),
    version          integer NOT NULL CHECK (version >= 1),
    secret_encrypted bytea NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, version)
);

-- One active internal signing key per project (dashboard preview, setup
-- tests, share links); app keys keep app_id.
CREATE UNIQUE INDEX signing_keys_one_internal ON signing_keys (project_id)
    WHERE app_id IS NULL AND retired_at IS NULL AND revoked_at IS NULL;

-- +goose Down
DROP INDEX signing_keys_one_internal;
DROP TABLE content_key_secrets;
