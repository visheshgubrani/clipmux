-- Key rotation and the owner's second factor (P5-T08, D071).

-- +goose Up
-- A content key secret version new outputs may use. With Worker delivery a
-- new version is usable only once the Worker has it (D037).
ALTER TABLE content_key_secrets ADD COLUMN usable_at timestamptz;
UPDATE content_key_secrets SET usable_at = created_at;

-- TOTP (RFC 6238): the secret sealed under owners/{id}, when it was turned
-- on, and the last time step accepted, so a code is used once.
ALTER TABLE owners
    ADD COLUMN totp_secret_encrypted bytea,
    ADD COLUMN totp_enabled_at       timestamptz,
    ADD COLUMN totp_last_step        bigint,
    ADD CONSTRAINT owners_totp CHECK ((totp_enabled_at IS NULL) OR (totp_secret_encrypted IS NOT NULL));

-- Single-use recovery codes, stored as SHA-256: they are long and random,
-- so a slow hash adds nothing.
CREATE TABLE owner_recovery_codes (
    project_id text NOT NULL REFERENCES projects (id),
    owner_id   text NOT NULL REFERENCES owners (id) ON DELETE CASCADE,
    code_hash  bytea PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now(),
    used_at    timestamptz
);
CREATE INDEX owner_recovery_codes_owner ON owner_recovery_codes (owner_id);

-- +goose Down
DROP TABLE owner_recovery_codes;
ALTER TABLE owners
    DROP CONSTRAINT owners_totp,
    DROP COLUMN totp_last_step,
    DROP COLUMN totp_enabled_at,
    DROP COLUMN totp_secret_encrypted;
ALTER TABLE content_key_secrets DROP COLUMN usable_at;
