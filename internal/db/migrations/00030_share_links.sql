-- Share links (P7-T06, D104): a page on the public URL that plays one
-- asset without a session or an app's token. Each link owns one signed
-- playback ID, created with it; the link's state follows that ID and its
-- expiry, so there is no revoked_at here: past its expiry a link reads
-- expired, and before it a stopped ID reads revoked.
--
-- The secret in the URL is looked up by its SHA-256 and also sealed
-- (secrets.Columns), so the owner can copy the link again.

-- +goose Up
CREATE TABLE share_links (
    id            text        PRIMARY KEY,
    project_id    text        NOT NULL REFERENCES projects (id),
    app_id        text        NOT NULL REFERENCES apps (id),
    asset_id      text        NOT NULL REFERENCES assets (id),
    playback_id   text        NOT NULL UNIQUE REFERENCES playback_ids (id),
    name          text        NOT NULL DEFAULT '' CHECK (char_length(name) <= 100),
    expires_at    timestamptz,
    secret_hash   bytea       NOT NULL UNIQUE CHECK (length(secret_hash) = 32),
    secret_sealed bytea       NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX share_links_asset ON share_links (asset_id, created_at);
CREATE INDEX share_links_expiring ON share_links (expires_at) WHERE expires_at IS NOT NULL;

-- +goose Down
DROP TABLE share_links;
