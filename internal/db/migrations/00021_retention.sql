-- Retention and webhook health (P5-T06, D069): rows past their windows are
-- pruned, a failing endpoint stops being tried for a while, an endpoint's
-- secret can be rotated in two steps, and a spent one-time key is dropped.

-- +goose Up
-- The breaker: consecutive failed tries, and until when the endpoint is
-- skipped. A skipped delivery isn't charged a try.
ALTER TABLE webhook_endpoints
    ADD COLUMN consecutive_failures integer NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0),
    ADD COLUMN circuit_open_until   timestamptz,
    -- A new secret, sealed, waiting for the receiver to accept it before it
    -- signs anything.
    ADD COLUMN pending_secret_encrypted bytea,
    ADD COLUMN pending_secret_at        timestamptz,
    ADD CONSTRAINT webhook_endpoints_pending_secret CHECK ((pending_secret_encrypted IS NULL) = (pending_secret_at IS NULL));

-- A dispatch's sealed key is needed only while its wake may be retried.
ALTER TABLE dispatches ALTER COLUMN key_sealed DROP NOT NULL;

-- What pruning looks for.
CREATE INDEX webhook_deliveries_finished ON webhook_deliveries (finished_at) WHERE status <> 'pending';
CREATE INDEX webhook_deliveries_event ON webhook_deliveries (event_id);
CREATE INDEX outbox_done ON outbox (kind, done_at) WHERE done_at IS NOT NULL;
CREATE INDEX idempotency_keys_created ON idempotency_keys (created_at);

-- +goose Down
DROP INDEX idempotency_keys_created;
DROP INDEX outbox_done;
DROP INDEX webhook_deliveries_event;
DROP INDEX webhook_deliveries_finished;
UPDATE dispatches SET key_sealed = '\x00'::bytea WHERE key_sealed IS NULL;
ALTER TABLE dispatches ALTER COLUMN key_sealed SET NOT NULL;
ALTER TABLE webhook_endpoints
    DROP CONSTRAINT webhook_endpoints_pending_secret,
    DROP COLUMN pending_secret_at,
    DROP COLUMN pending_secret_encrypted,
    DROP COLUMN circuit_open_until,
    DROP COLUMN consecutive_failures;
