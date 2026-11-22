-- Analytics accuracy and storage-taken accounting:
--
--   1. Storage bytes tracked on assets (sources), outputs and tracks so
--      Storage and Analytics show actual disk / bucket bytes taken.
--   2. Per-install Analytics Engine isolation (projects.analytics_install_id
--      written to blob7 by the Worker) so multiple installs sharing one
--      Cloudflare account never pollute each other's counts.
--   3. Hourly unique viewer tables (kept 14 days beside playback_hourly) so
--      24h unique_viewers and its previous window never overlap on UTC days.

-- +goose Up
ALTER TABLE assets ADD COLUMN source_bytes bigint NOT NULL DEFAULT 0 CHECK (source_bytes >= 0);

UPDATE assets a
SET source_bytes = u.declared_size
FROM uploads u
WHERE u.asset_id = a.id
  AND u.status = 'completed'
  AND a.source_kept
  AND a.agent_source_id IS NULL
  AND a.deleted_at IS NULL;

ALTER TABLE outputs ADD COLUMN bytes bigint NOT NULL DEFAULT 0 CHECK (bytes >= 0);

UPDATE outputs o
SET bytes = coalesce((
    SELECT sum((f->>'size')::bigint)
    FROM jsonb_array_elements(coalesce(o.manifest->'files', '[]'::jsonb)) f
    WHERE f ? 'size'
), 0)
WHERE o.status IN ('committed', 'retired') AND o.purged_at IS NULL;

ALTER TABLE tracks ADD COLUMN bytes bigint NOT NULL DEFAULT 0 CHECK (bytes >= 0);

ALTER TABLE projects ADD COLUMN analytics_install_id text;
UPDATE projects
SET analytics_install_id = 'ins_' || replace(gen_random_uuid()::text, '-', '')
WHERE analytics_install_id IS NULL;
ALTER TABLE projects ALTER COLUMN analytics_install_id SET NOT NULL;
ALTER TABLE projects ALTER COLUMN analytics_install_id SET DEFAULT ('ins_' || replace(gen_random_uuid()::text, '-', ''));
ALTER TABLE projects ADD CONSTRAINT projects_analytics_install_id_check CHECK (analytics_install_id ~ '^[a-z0-9_]{1,64}$');

ALTER TABLE cloudflare_connections ADD COLUMN analytics_tagged boolean NOT NULL DEFAULT false;
ALTER TABLE cloudflare_connections ADD COLUMN analytics_legacy_until timestamptz;

CREATE TABLE playback_viewers_hourly (
    project_id text        NOT NULL REFERENCES projects (id),
    hour       timestamptz NOT NULL CHECK (date_trunc('hour', hour, 'UTC') = hour),
    asset_id   text        NOT NULL,
    viewer     bytea       NOT NULL CHECK (length(viewer) = 16),
    app_id     text        NOT NULL,
    PRIMARY KEY (project_id, hour, asset_id, viewer)
);
CREATE INDEX playback_viewers_hourly_asset_idx ON playback_viewers_hourly (project_id, asset_id, hour);

CREATE TABLE playback_viewer_hours (
    project_id text        NOT NULL REFERENCES projects (id),
    hour       timestamptz NOT NULL CHECK (date_trunc('hour', hour, 'UTC') = hour),
    app_id     text        NOT NULL,
    viewer     bytea       NOT NULL CHECK (length(viewer) = 16),
    PRIMARY KEY (project_id, hour, app_id, viewer)
);
CREATE INDEX playback_viewer_hours_app_idx ON playback_viewer_hours (project_id, app_id, hour);

-- +goose Down
DROP TABLE playback_viewer_hours;
DROP TABLE playback_viewers_hourly;
ALTER TABLE cloudflare_connections DROP COLUMN analytics_legacy_until;
ALTER TABLE cloudflare_connections DROP COLUMN analytics_tagged;
ALTER TABLE projects DROP CONSTRAINT projects_analytics_install_id_check;
ALTER TABLE projects DROP COLUMN analytics_install_id;
ALTER TABLE tracks DROP COLUMN bytes;
ALTER TABLE outputs DROP COLUMN bytes;
ALTER TABLE assets DROP COLUMN source_bytes;
