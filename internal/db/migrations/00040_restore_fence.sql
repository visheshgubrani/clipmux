-- Restore fence and store epoch (D144), and the backups ledger (D143).
--
-- Bringing back an older database makes every storage sweep delete what the
-- database doesn't know, which is exactly what a restore leaves in storage:
-- the videos uploaded since. The leader stamps each store with this install's
-- ID and a generation it has already committed here (install_epoch). A store
-- stamped by another install, or with a generation ahead of this database's,
-- means the database went back in time or belongs to another install, and the
-- fence (restore_fence) stops every delete from storage until the owner lifts
-- it. The tables are in the dump, so a restore brings back the ID and the
-- generation it was taken at, which is how it is noticed.
--
-- backups lists the dumps taken (T29.3); a dump can't list itself, so each
-- row describes an earlier file.

-- +goose Up
CREATE TABLE install_epoch (
    project_id text PRIMARY KEY REFERENCES projects (id),
    install_id text NOT NULL DEFAULT ('ins_' || replace(gen_random_uuid()::text, '-', ''))
        CHECK (install_id ~ '^ins_[a-z0-9]{1,60}$'),
    -- Committed here before any store is stamped with it.
    generation bigint NOT NULL DEFAULT 0 CHECK (generation >= 0),
    updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO install_epoch (project_id) SELECT id FROM projects;

CREATE TABLE restore_fence (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id  text NOT NULL REFERENCES projects (id),
    -- restored: `clipmux admin restore` set it. database_behind: a store
    -- holds a generation this database never reached. foreign_install: a
    -- store was stamped by another install.
    reason      text NOT NULL CHECK (reason IN ('restored', 'database_behind', 'foreign_install')),
    -- What was found, for the report and the dashboard.
    detail      text NOT NULL DEFAULT '',
    -- The backup restored, when the reason is 'restored'.
    backup_id   text,
    backup_at   timestamptz,
    detected_at timestamptz NOT NULL DEFAULT now(),
    lifted_at   timestamptz
);
CREATE UNIQUE INDEX restore_fence_one_active ON restore_fence (project_id) WHERE lifted_at IS NULL;

CREATE TABLE backups (
    id                 text PRIMARY KEY,
    project_id         text NOT NULL REFERENCES projects (id),
    kind               text NOT NULL CHECK (kind IN ('scheduled', 'manual', 'pre_migrate', 'pre_restore')),
    -- The dump's key on the backup target.
    object_key         text NOT NULL,
    size_bytes         bigint NOT NULL CHECK (size_bytes >= 0),
    sha256             text NOT NULL,
    clipmux_version    text NOT NULL,
    -- The goose version the dump was taken at, which a restore checks
    -- against the binary's.
    schema_version     bigint NOT NULL,
    postgres_version   text NOT NULL,
    secrets_key_version integer NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    verified_at        timestamptz,
    verify_error       text
);
CREATE INDEX backups_recent ON backups (project_id, created_at DESC);

-- +goose Down
DROP TABLE backups;
DROP TABLE restore_fence;
DROP TABLE install_epoch;
