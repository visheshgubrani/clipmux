-- Fallback limits (P4-T03, D055; docs/architecture.md "Routing and
-- fallback"): a provider takes a job only while it has room under its
-- concurrency and monthly spend limits and isn't backing off after failed
-- wakes.

-- +goose Up
ALTER TABLE executor_configs
    ADD COLUMN wake_failures integer NOT NULL DEFAULT 0 CHECK (wake_failures >= 0),
    ADD COLUMN backoff_until timestamptz;

-- What a dispatch holds against the monthly limit until it ends: its rate
-- times this many seconds, the job's estimated run (job_estimated_seconds).
ALTER TABLE dispatches ADD COLUMN reserved_seconds double precision NOT NULL DEFAULT 0 CHECK (reserved_seconds >= 0);

-- How long a provider's machine is assumed to need for a job, before it has
-- run: an hour per 4 GiB of source, at least 15 minutes and at most the
-- machine's 6 h 15 min lifetime. A source of unknown size (an import)
-- counts as one hour.
-- +goose StatementBegin
CREATE FUNCTION job_estimated_seconds(job_asset_id text) RETURNS double precision LANGUAGE sql STABLE AS $$
    SELECT least(22500, greatest(900, coalesce(
        (SELECT u.declared_size FROM uploads u WHERE u.asset_id = job_asset_id ORDER BY u.created_at DESC LIMIT 1),
        (SELECT g.size FROM assets s JOIN agent_sources g ON g.id = s.agent_source_id WHERE s.id = job_asset_id),
        4294967296)::double precision / 4294967296 * 3600))
$$;
-- +goose StatementEnd

-- A provider's spend this calendar month (UTC), in cents. A machine costs
-- its rate from when it was woken until it was stopped, or until now while
-- it may still run. A dispatch that hasn't ended counts at least its
-- reservation, so the limit holds before a machine has run long enough to
-- show its cost.
-- +goose StatementBegin
CREATE FUNCTION provider_month_cents(config_id text) RETURNS double precision LANGUAGE sql STABLE AS $$
    SELECT coalesce(sum(d.rate_cents_per_hour * greatest(
        extract(epoch FROM CASE
            WHEN d.stopped_at IS NOT NULL THEN d.stopped_at
            WHEN d.instance_id IS NOT NULL THEN now()
            ELSE d.ended_at END - d.woken_at) / 3600,
        CASE WHEN d.status <> 'ended' THEN d.reserved_seconds / 3600 ELSE 0 END)), 0)
    FROM dispatches d
    WHERE d.executor_config_id = config_id
      AND d.created_at >= date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'
$$;
-- +goose StatementEnd

-- Whether a provider can take this job now: not backing off, fewer
-- machines than max_concurrent (a machine counts until it is stopped), and
-- the month's spend plus this job's reservation within the limit.
-- +goose StatementBegin
CREATE FUNCTION provider_has_room(config_id text, job_asset_id text) RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT (c.backoff_until IS NULL OR c.backoff_until <= now())
       AND (SELECT count(*) FROM dispatches d
            WHERE d.executor_config_id = c.id
              AND (d.status <> 'ended' OR (d.instance_id IS NOT NULL AND d.stopped_at IS NULL))) < c.max_concurrent
       AND provider_month_cents(c.id) + c.rate_cents_per_hour * job_estimated_seconds(job_asset_id) / 3600
           <= c.monthly_limit_cents
    FROM executor_configs c WHERE c.id = config_id
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION provider_has_room(text, text);
DROP FUNCTION provider_month_cents(text);
DROP FUNCTION job_estimated_seconds(text);
ALTER TABLE dispatches DROP COLUMN reserved_seconds;
ALTER TABLE executor_configs DROP COLUMN backoff_until, DROP COLUMN wake_failures;
