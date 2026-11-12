-- Playback analytics (P7-T02, D103): counted where delivery answers the
-- viewer and rolled up here, not in a second database.
--
-- playback_hourly holds additive totals per hour and playback ID. Origin
-- rows are added to by each replica's flush; edge rows (T05) are replaced
-- with what Analytics Engine holds for the hour, so a pull repeated never
-- counts twice. asset_id and app_id carry no foreign key: totals outlive
-- deleted assets, and a flush must never fail on one.
--
-- playback_daily is the same per UTC day, written in the same flush. Ranges
-- of a week or more read it: 90 days of a busy install is about 200,000
-- daily rows against 5 million hourly ones (P7-T03). Hourly rows are kept
-- only for the 24-hour range, so they go after 14 days.
--
-- playback_viewers holds one row per viewer key per asset per day, for an
-- asset's unique viewers; playback_viewer_days one per viewer per app per
-- day, for a project's or an app's. A viewer key is analytics.ViewerKey
-- under the project's analytics key; no raw vid, IP or User-Agent is
-- stored anywhere.

-- +goose Up
CREATE TABLE playback_hourly (
    project_id    text        NOT NULL REFERENCES projects (id),
    source        text        NOT NULL CHECK (source IN ('origin', 'edge')),
    hour          timestamptz NOT NULL CHECK (date_trunc('hour', hour, 'UTC') = hour),
    app_id        text        NOT NULL,
    asset_id      text        NOT NULL,
    playback_id   text        NOT NULL,
    views         bigint      NOT NULL DEFAULT 0 CHECK (views >= 0),
    watch_seconds bigint      NOT NULL DEFAULT 0 CHECK (watch_seconds >= 0),
    bytes         bigint      NOT NULL DEFAULT 0 CHECK (bytes >= 0),
    PRIMARY KEY (project_id, source, hour, asset_id, playback_id)
);
CREATE INDEX playback_hourly_asset ON playback_hourly (project_id, asset_id, hour);

CREATE TABLE playback_daily (
    project_id    text   NOT NULL REFERENCES projects (id),
    source        text   NOT NULL CHECK (source IN ('origin', 'edge')),
    day           date   NOT NULL,
    app_id        text   NOT NULL,
    asset_id      text   NOT NULL,
    playback_id   text   NOT NULL,
    views         bigint NOT NULL DEFAULT 0 CHECK (views >= 0),
    watch_seconds bigint NOT NULL DEFAULT 0 CHECK (watch_seconds >= 0),
    bytes         bigint NOT NULL DEFAULT 0 CHECK (bytes >= 0),
    PRIMARY KEY (project_id, source, day, asset_id, playback_id)
);
CREATE INDEX playback_daily_asset ON playback_daily (project_id, asset_id, day);

CREATE TABLE playback_viewers (
    project_id text  NOT NULL REFERENCES projects (id),
    day        date  NOT NULL,
    asset_id   text  NOT NULL,
    viewer     bytea NOT NULL CHECK (length(viewer) = 16),
    app_id     text  NOT NULL,
    PRIMARY KEY (project_id, day, asset_id, viewer)
);
CREATE INDEX playback_viewers_asset ON playback_viewers (project_id, asset_id, day);

CREATE TABLE playback_viewer_days (
    project_id text  NOT NULL REFERENCES projects (id),
    day        date  NOT NULL,
    app_id     text  NOT NULL,
    viewer     bytea NOT NULL CHECK (length(viewer) = 16),
    PRIMARY KEY (project_id, day, app_id, viewer)
);

-- The key viewer keys are made under, created at boot, sealed.
ALTER TABLE projects ADD COLUMN analytics_key_sealed bytea;

-- analytics:read (P7-T03): an opt-in scope for an app's own analytics.
ALTER TABLE api_keys DROP CONSTRAINT api_keys_scopes_check,
    ADD CONSTRAINT api_keys_scopes_check CHECK (cardinality(scopes) > 0
        AND scopes <@ ARRAY['assets:read', 'assets:write', 'uploads:write', 'analytics:read']::text[]);

-- +goose Down
DELETE FROM api_keys WHERE 'analytics:read' = ANY (scopes) AND cardinality(scopes) = 1;
UPDATE api_keys SET scopes = array_remove(scopes, 'analytics:read');
ALTER TABLE api_keys DROP CONSTRAINT api_keys_scopes_check,
    ADD CONSTRAINT api_keys_scopes_check CHECK (cardinality(scopes) > 0
        AND scopes <@ ARRAY['assets:read', 'assets:write', 'uploads:write']::text[]);
ALTER TABLE projects DROP COLUMN analytics_key_sealed;
DROP TABLE playback_viewer_days;
DROP TABLE playback_viewers;
DROP TABLE playback_daily;
DROP TABLE playback_hourly;
