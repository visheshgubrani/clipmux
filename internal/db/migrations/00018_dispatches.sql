-- Dispatches (P4-T02, D054): a job routed to a serverless provider wakes one
-- machine, which claims that job alone with a one-time key.

-- +goose Up
-- A connected provider. Its credentials are sealed (D017, associated data
-- executor_configs/{id}); config holds the rest (image, GPU, region).
CREATE TABLE executor_configs (
    id                     text PRIMARY KEY,
    project_id             text NOT NULL REFERENCES projects (id),
    kind                   text NOT NULL CHECK (kind IN ('modal', 'runpod')),
    config                 jsonb NOT NULL DEFAULT '{}',
    secrets_encrypted      bytea,
    max_concurrent         integer NOT NULL DEFAULT 1 CHECK (max_concurrent BETWEEN 1 AND 100),
    monthly_limit_cents    bigint NOT NULL DEFAULT 0 CHECK (monthly_limit_cents >= 0),
    rate_cents_per_hour    integer NOT NULL DEFAULT 0 CHECK (rate_cents_per_hour >= 0),
    -- The scratch disk each machine gets, held to the claim's free-disk rule.
    disk_bytes             bigint NOT NULL CHECK (disk_bytes > 0),
    -- How long a woken machine has to claim its job.
    claim_deadline_seconds integer NOT NULL DEFAULT 600 CHECK (claim_deadline_seconds BETWEEN 60 AND 3600),
    -- Set when a setup test through this provider passes. Until then the
    -- router won't send it work, except that test's own job.
    verified_at            timestamptz,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, kind)
);

CREATE TABLE dispatches (
    id                  text PRIMARY KEY,
    project_id          text NOT NULL REFERENCES projects (id),
    job_id              text NOT NULL REFERENCES jobs (id),
    executor_config_id  text NOT NULL REFERENCES executor_configs (id),
    kind                text NOT NULL CHECK (kind IN ('modal', 'runpod')),
    -- waking: routed, the machine may be starting; claimed: its key claimed
    -- the job; ended: done with, for end_reason.
    status              text NOT NULL DEFAULT 'waking' CHECK (status IN ('waking', 'claimed', 'ended')),
    end_reason          text,
    -- The one-time key: its hash authenticates the claim; the sealed copy
    -- lets a retried wake hand the same key to the same machine.
    key_hash            bytea NOT NULL UNIQUE,
    key_sealed          bytea NOT NULL,
    -- The provider's ID for the machine, once Wake returned.
    instance_id         text,
    -- A copy of the provider's rate, so a later change doesn't rewrite cost.
    rate_cents_per_hour integer NOT NULL,
    claim_deadline      timestamptz NOT NULL,
    attempt_id          text REFERENCES attempts (id),
    created_at          timestamptz NOT NULL DEFAULT now(),
    woken_at            timestamptz,
    claimed_at          timestamptz,
    ended_at            timestamptz,
    stopped_at          timestamptz,
    CHECK ((status = 'ended') = (ended_at IS NOT NULL)),
    CHECK ((status = 'claimed') <= (attempt_id IS NOT NULL))
);
-- At most one dispatch per job is live at a time.
CREATE UNIQUE INDEX dispatches_one_live_per_job ON dispatches (job_id) WHERE status <> 'ended';
CREATE INDEX dispatches_live ON dispatches (executor_config_id) WHERE status <> 'ended';

ALTER TABLE attempts ADD COLUMN dispatch_id text REFERENCES dispatches (id);

ALTER TABLE outbox DROP CONSTRAINT outbox_kind_check;
ALTER TABLE outbox ADD CONSTRAINT outbox_kind_check CHECK (kind IN ('webhook', 'route_sync', 'keys_sync', 'wake', 'stop'));

-- A setup test may name the executor its sample must run on (after Connect
-- RunPod or Modal): its job is routed there and nowhere else.
ALTER TABLE setup_tests ADD COLUMN executor text CHECK (executor IN ('builtin', 'agents', 'modal', 'runpod'));
ALTER TABLE setup_tests DROP CONSTRAINT setup_tests_trigger_check;
ALTER TABLE setup_tests ADD CONSTRAINT setup_tests_trigger_check
    CHECK (trigger IN ('first_boot', 'urls_saved', 'manual', 'cli', 'executor_connected'));

-- +goose Down
DELETE FROM setup_tests WHERE trigger = 'executor_connected';
ALTER TABLE setup_tests DROP CONSTRAINT setup_tests_trigger_check;
ALTER TABLE setup_tests ADD CONSTRAINT setup_tests_trigger_check CHECK (trigger IN ('first_boot', 'urls_saved', 'manual', 'cli'));
ALTER TABLE setup_tests DROP COLUMN executor;
DELETE FROM outbox WHERE kind IN ('wake', 'stop');
ALTER TABLE outbox DROP CONSTRAINT outbox_kind_check;
ALTER TABLE outbox ADD CONSTRAINT outbox_kind_check CHECK (kind IN ('webhook', 'route_sync', 'keys_sync'));
ALTER TABLE attempts DROP COLUMN dispatch_id;
DROP TABLE dispatches;
DROP TABLE executor_configs;
