-- A provider's own bill (Modal's billing report): what it has charged this
-- month for ClipMux's app, up to the last complete hour it reported. The
-- spend shown and the monthly limit use it, with ClipMux's estimate only for
-- the time since: the estimate prices every machine at the dearest GPU
-- allowed and counts until the reaper sees it stop, so on a real month it
-- was twice the bill. A provider that can't report one (a plan without
-- billing reports) leaves these null and keeps the estimate.

-- +goose Up
ALTER TABLE executor_configs
    ADD COLUMN billed_month_cents bigint CHECK (billed_month_cents >= 0),
    ADD COLUMN billed_through timestamptz,
    ADD COLUMN billed_checked_at timestamptz;

-- The estimate of a provider's spend this calendar month (UTC) from since
-- on, in cents: each machine's rate over the part of its run after since,
-- and at least its reservation while its dispatch hasn't ended. With since
-- at the month's start it is provider_month_cents as 00019 defined it.
-- +goose StatementBegin
CREATE FUNCTION provider_estimate_cents(config_id text, since timestamptz) RETURNS double precision LANGUAGE sql STABLE AS $$
    SELECT coalesce(sum(d.rate_cents_per_hour * greatest(
        greatest(extract(epoch FROM CASE
            WHEN d.stopped_at IS NOT NULL THEN d.stopped_at
            WHEN d.instance_id IS NOT NULL THEN now()
            -- A machine never woken has no start: no cost, as in 00019
            -- (greatest() would skip its null and start it at since).
            ELSE d.ended_at END - CASE WHEN d.woken_at < since THEN since ELSE d.woken_at END), 0) / 3600,
        CASE WHEN d.status <> 'ended' THEN d.reserved_seconds / 3600 ELSE 0 END)), 0)
    FROM dispatches d
    WHERE d.executor_config_id = config_id
      AND d.created_at >= date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'
$$;
-- +goose StatementEnd

-- A provider's spend this month, in cents: its bill up to billed_through
-- plus the estimate since, when the bill is this month's; otherwise the
-- estimate for the whole month.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION provider_month_cents(config_id text) RETURNS double precision LANGUAGE sql STABLE AS $$
    SELECT CASE
        WHEN c.billed_through >= date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'
            THEN c.billed_month_cents + provider_estimate_cents(c.id, c.billed_through)
        ELSE provider_estimate_cents(c.id, '-infinity') END
    FROM executor_configs c WHERE c.id = config_id
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION provider_month_cents(config_id text) RETURNS double precision LANGUAGE sql STABLE AS $$
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
DROP FUNCTION provider_estimate_cents(text, timestamptz);
ALTER TABLE executor_configs DROP COLUMN billed_checked_at, DROP COLUMN billed_through, DROP COLUMN billed_month_cents;
