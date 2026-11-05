-- Core model: docs/architecture.md, "Core model". Every row carries project_id;
-- IDs are text with a type prefix (internal/ids). States are CHECK-constrained
-- so an invalid state can't be written by any code path.

-- +goose Up
CREATE TABLE projects (
    id                         text PRIMARY KEY,
    name                       text NOT NULL,
    public_url                 text,
    media_hostname             text,
    -- Delivery rejects tokens whose exp - iat exceeds this (default 7 days).
    max_token_lifetime_seconds integer NOT NULL DEFAULT 604800 CHECK (max_token_lifetime_seconds > 0),
    created_at                 timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE storage_configs (
    id                text PRIMARY KEY,
    project_id        text NOT NULL REFERENCES projects (id),
    kind              text NOT NULL CHECK (kind IN ('fs', 'r2', 's3')),
    config            jsonb NOT NULL DEFAULT '{}',
    -- Adapter secrets, encrypted with a key held outside Postgres.
    secrets_encrypted bytea,
    is_default        boolean NOT NULL DEFAULT false,
    created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX storage_configs_one_default ON storage_configs (project_id) WHERE is_default;

CREATE TABLE apps (
    id          text PRIMARY KEY,
    project_id  text NOT NULL REFERENCES projects (id),
    name        text NOT NULL,
    -- The built-in Dashboard app receives dashboard uploads.
    is_builtin  boolean NOT NULL DEFAULT false,
    disabled_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, name)
);
CREATE UNIQUE INDEX apps_one_builtin ON apps (project_id) WHERE is_builtin;

CREATE TABLE api_keys (
    id           text PRIMARY KEY,
    project_id   text NOT NULL REFERENCES projects (id),
    app_id       text NOT NULL REFERENCES apps (id),
    name         text NOT NULL,
    key_hash     bytea NOT NULL UNIQUE,
    scopes       text[] NOT NULL CHECK (
        cardinality(scopes) > 0
        AND scopes <@ ARRAY['assets:read', 'assets:write', 'uploads:write']::text[]),
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    revoked_at   timestamptz
);

-- ES256 token-signing keys; id is the JWT kid. App keys keep only the public
-- key (the app's backend signs). The server's internal key (app_id NULL) also
-- keeps its private key, encrypted like adapter secrets.
CREATE TABLE signing_keys (
    id                    text PRIMARY KEY,
    project_id            text NOT NULL REFERENCES projects (id),
    app_id                text REFERENCES apps (id),
    algorithm             text NOT NULL DEFAULT 'ES256' CHECK (algorithm = 'ES256'),
    public_key_pem        text NOT NULL,
    private_key_encrypted bytea,
    created_at            timestamptz NOT NULL DEFAULT now(),
    retired_at            timestamptz,
    revoked_at            timestamptz,
    CHECK ((app_id IS NULL) = (private_key_encrypted IS NOT NULL))
);

CREATE TABLE webhook_endpoints (
    id               text PRIMARY KEY,
    project_id       text NOT NULL REFERENCES projects (id),
    app_id           text NOT NULL REFERENCES apps (id),
    url              text NOT NULL,
    secret_encrypted bytea NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    disabled_at      timestamptz
);

CREATE TABLE agents (
    id           text PRIMARY KEY,
    project_id   text NOT NULL REFERENCES projects (id),
    name         text NOT NULL,
    key_hash     bytea NOT NULL UNIQUE,
    status       text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'revoked')),
    -- 'local' agents claim only jobs pinned to them.
    scope        text NOT NULL DEFAULT 'all' CHECK (scope IN ('all', 'local')),
    sandboxed    boolean NOT NULL DEFAULT false,
    capabilities jsonb NOT NULL DEFAULT '{}',
    last_seen_at timestamptz,
    last_ip      inet,
    created_at   timestamptz NOT NULL DEFAULT now(),
    approved_at  timestamptz,
    revoked_at   timestamptz,
    -- Unsandboxed agents get local jobs only.
    CHECK (sandboxed OR scope = 'local')
);

CREATE TABLE agent_sources (
    id         text PRIMARY KEY,
    project_id text NOT NULL REFERENCES projects (id),
    agent_id   text NOT NULL REFERENCES agents (id),
    size       bigint NOT NULL CHECK (size > 0),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE assets (
    id                text PRIMARY KEY,
    project_id        text NOT NULL REFERENCES projects (id),
    app_id            text NOT NULL REFERENCES apps (id),
    status            text NOT NULL CHECK (status IN ('waiting_upload', 'processing', 'ready', 'errored', 'deleted')),
    error_code        text,
    -- Goes up on every change; webhooks carry it.
    version           bigint NOT NULL DEFAULT 1 CHECK (version >= 1),
    storage_id        text NOT NULL REFERENCES storage_configs (id),
    -- e.g. uploads/{asset_id}/source, or agent://{agent_id}/{source_id}.
    source_uri        text,
    agent_source_id   text REFERENCES agent_sources (id),
    source_kept       boolean NOT NULL DEFAULT true,
    current_output_id text,
    passthrough       text CHECK (length(passthrough) <= 255),
    duration_seconds  double precision,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    deleted_at        timestamptz,
    CHECK (status <> 'ready' OR current_output_id IS NOT NULL),
    CHECK ((status = 'errored') = (error_code IS NOT NULL))
);
CREATE INDEX assets_app_passthrough ON assets (app_id, passthrough) WHERE passthrough IS NOT NULL;
CREATE INDEX assets_app_created ON assets (app_id, created_at DESC);

CREATE TABLE outputs (
    id            text PRIMARY KEY,
    project_id    text NOT NULL REFERENCES projects (id),
    asset_id      text NOT NULL REFERENCES assets (id),
    -- A path is never reused, so every stored object can be cached as immutable.
    prefix        text GENERATED ALWAYS AS ('media/' || asset_id || '/' || id || '/') STORED UNIQUE,
    status        text NOT NULL DEFAULT 'writing' CHECK (status IN ('writing', 'committed', 'retired')),
    manifest      jsonb,
    output_format integer NOT NULL DEFAULT 1,
    -- Version of the content key secret this output's key derives from.
    key_version   integer NOT NULL CHECK (key_version >= 1),
    created_at    timestamptz NOT NULL DEFAULT now(),
    committed_at  timestamptz,
    retired_at    timestamptz,
    UNIQUE (id, asset_id),
    CHECK (status = 'writing' OR manifest IS NOT NULL)
);

-- Only current_output_id moves, and only to one of the asset's own outputs.
ALTER TABLE assets ADD CONSTRAINT assets_current_output_fk
    FOREIGN KEY (current_output_id, id) REFERENCES outputs (id, asset_id);

CREATE TABLE jobs (
    id              text PRIMARY KEY,
    project_id      text NOT NULL REFERENCES projects (id),
    asset_id        text NOT NULL REFERENCES assets (id),
    kind            text NOT NULL CHECK (kind IN ('import', 'transcode', 'reencode', 'move', 'delete')),
    status          text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
    phase           text,
    progress        real CHECK (progress BETWEEN 0 AND 1),
    profile_id      text NOT NULL DEFAULT 'default',
    profile_version integer NOT NULL DEFAULT 1,
    -- Jobs on agent-local sources run only on that agent (no fallback).
    pinned_agent_id text REFERENCES agents (id),
    -- Separate retry budgets for lost leases and reported failures.
    lost_count      integer NOT NULL DEFAULT 0 CHECK (lost_count >= 0),
    failed_count    integer NOT NULL DEFAULT 0 CHECK (failed_count >= 0),
    error_code      text,
    routed_to       text,
    route_reason    text,
    available_at    timestamptz NOT NULL DEFAULT now(),
    created_at      timestamptz NOT NULL DEFAULT now(),
    started_at      timestamptz,
    finished_at     timestamptz,
    UNIQUE (id, asset_id)
);
-- One mutating job per asset: import, transcode, re-encode, move and delete
-- never overlap on the same asset.
CREATE UNIQUE INDEX jobs_one_active_per_asset ON jobs (asset_id) WHERE status IN ('queued', 'running');
CREATE INDEX jobs_claimable ON jobs (project_id, available_at) WHERE status = 'queued';

CREATE TABLE attempts (
    id                text PRIMARY KEY,
    project_id        text NOT NULL REFERENCES projects (id),
    job_id            text NOT NULL,
    asset_id          text NOT NULL,
    attempt_number    integer NOT NULL CHECK (attempt_number >= 1),
    -- Every attempt writes its own output, of its own job's asset.
    output_id         text NOT NULL UNIQUE,
    executor          text NOT NULL CHECK (executor IN ('builtin', 'agent', 'modal', 'runpod')),
    agent_id          text REFERENCES agents (id),
    status            text NOT NULL DEFAULT 'running' CHECK (status IN ('running', 'committed', 'failed', 'lost')),
    token_hash        bytea NOT NULL UNIQUE,
    lease_expires_at  timestamptz NOT NULL,
    phase             text,
    progress          real CHECK (progress BETWEEN 0 AND 1),
    -- ffmpeg/Shaka versions, encoders and free disk reported at claim.
    worker_info       jsonb NOT NULL DEFAULT '{}',
    failure_kind      text CHECK (failure_kind IN ('transient', 'permanent')),
    error_code        text,
    started_at        timestamptz NOT NULL DEFAULT now(),
    last_heartbeat_at timestamptz,
    finished_at       timestamptz,
    UNIQUE (job_id, attempt_number),
    FOREIGN KEY (job_id, asset_id) REFERENCES jobs (id, asset_id),
    FOREIGN KEY (output_id, asset_id) REFERENCES outputs (id, asset_id),
    CHECK ((executor = 'agent') = (agent_id IS NOT NULL))
);
CREATE INDEX attempts_running_lease ON attempts (lease_expires_at) WHERE status = 'running';

CREATE TABLE playback_ids (
    id         text PRIMARY KEY,
    project_id text NOT NULL REFERENCES projects (id),
    asset_id   text NOT NULL REFERENCES assets (id),
    policy     text NOT NULL CHECK (policy IN ('public', 'signed')),
    -- 'revoking' while the unpublish is not yet confirmed (DELETE returns 202).
    status     text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'revoking', 'revoked')),
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz,
    CHECK ((status = 'revoked') = (revoked_at IS NOT NULL))
);
CREATE INDEX playback_ids_asset ON playback_ids (asset_id);

CREATE TABLE uploads (
    id               text PRIMARY KEY,
    project_id       text NOT NULL REFERENCES projects (id),
    asset_id         text NOT NULL REFERENCES assets (id),
    multipart_id     text NOT NULL,
    declared_size    bigint NOT NULL CHECK (declared_size > 0),
    part_size        bigint NOT NULL CHECK (part_size > 0),
    token_hash       bytea NOT NULL UNIQUE,
    status           text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'completed', 'cancelled', 'expired')),
    last_activity_at timestamptz NOT NULL DEFAULT now(),
    created_at       timestamptz NOT NULL DEFAULT now(),
    finished_at      timestamptz,
    -- S3/R2 multipart allows at most 10,000 parts.
    CHECK (declared_size <= part_size * 10000)
);
CREATE UNIQUE INDEX uploads_one_active_per_asset ON uploads (asset_id) WHERE status = 'active';
CREATE INDEX uploads_active_activity ON uploads (last_activity_at) WHERE status = 'active';

-- Side effects outside Postgres (webhooks, route records) run from rows written
-- in the same transaction as the state change.
CREATE TABLE outbox (
    id           text PRIMARY KEY,
    project_id   text NOT NULL REFERENCES projects (id),
    kind         text NOT NULL CHECK (kind IN ('webhook', 'route_sync')),
    app_id       text REFERENCES apps (id),
    asset_id     text REFERENCES assets (id),
    payload      jsonb NOT NULL,
    available_at timestamptz NOT NULL DEFAULT now(),
    attempts     integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error   text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    done_at      timestamptz
);
CREATE INDEX outbox_pending ON outbox (kind, available_at) WHERE done_at IS NULL;

CREATE TABLE idempotency_keys (
    project_id      text NOT NULL REFERENCES projects (id),
    app_id          text NOT NULL REFERENCES apps (id),
    key             text NOT NULL CHECK (length(key) BETWEEN 1 AND 255),
    request_hash    bytea NOT NULL,
    response_status integer,
    response_body   jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (app_id, key)
);

-- First boot: the hidden default project, the built-in Dashboard app and
-- filesystem storage are active before the owner configures anything.
INSERT INTO projects (id, name) VALUES ('prj_default', 'Default');
INSERT INTO apps (id, project_id, name, is_builtin) VALUES ('app_dashboard', 'prj_default', 'Dashboard', true);
INSERT INTO storage_configs (id, project_id, kind, is_default) VALUES ('sto_default', 'prj_default', 'fs', true);

-- +goose Down
DROP TABLE idempotency_keys;
DROP TABLE outbox;
DROP TABLE uploads;
DROP TABLE playback_ids;
DROP TABLE attempts;
DROP TABLE jobs;
ALTER TABLE assets DROP CONSTRAINT assets_current_output_fk;
DROP TABLE outputs;
DROP TABLE assets;
DROP TABLE agent_sources;
DROP TABLE agents;
DROP TABLE webhook_endpoints;
DROP TABLE signing_keys;
DROP TABLE api_keys;
DROP TABLE apps;
DROP TABLE storage_configs;
DROP TABLE projects;
