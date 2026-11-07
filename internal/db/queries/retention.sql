-- Retention (P5-T06, D069). Each rule deletes at most max_rows rows past its
-- window; the caller repeats it until a pass comes up short.

-- name: PruneIdempotencyKeys :execrows
DELETE FROM idempotency_keys WHERE (app_id, key) IN (
    SELECT i.app_id, i.key FROM idempotency_keys i
    WHERE i.created_at < sqlc.arg(before) LIMIT sqlc.arg(max_rows));

-- name: PruneOwnerSessions :execrows
-- Expired, or idle past the idle limit: such a session can never be used.
DELETE FROM owner_sessions WHERE token_hash IN (
    SELECT s.token_hash FROM owner_sessions s
    WHERE s.expires_at < sqlc.arg(now) OR s.last_seen_at < sqlc.arg(idle_before) LIMIT sqlc.arg(max_rows));

-- name: PruneAgentPairings :execrows
DELETE FROM agent_pairings WHERE id IN (
    SELECT p.id FROM agent_pairings p WHERE p.expires_at < sqlc.arg(before) LIMIT sqlc.arg(max_rows));

-- name: PruneWebhookDeliveries :execrows
DELETE FROM webhook_deliveries WHERE id IN (
    SELECT d.id FROM webhook_deliveries d
    WHERE d.status <> 'pending' AND d.finished_at < sqlc.arg(before) LIMIT sqlc.arg(max_rows));

-- name: PruneWebhookEvents :execrows
-- An event goes once fanned out long enough ago and none of its deliveries
-- is left; GET /v1/events and clipmux listen read it until then.
DELETE FROM outbox WHERE id IN (
    SELECT o.id FROM outbox o
    WHERE o.kind = 'webhook' AND o.done_at < sqlc.arg(before)
      AND NOT EXISTS (SELECT 1 FROM webhook_deliveries d WHERE d.event_id = o.id)
    LIMIT sqlc.arg(max_rows));

-- name: PruneDoneOutbox :execrows
DELETE FROM outbox WHERE id IN (
    SELECT o.id FROM outbox o WHERE o.kind <> 'webhook' AND o.done_at < sqlc.arg(before) LIMIT sqlc.arg(max_rows));

-- name: PruneSetupTests :execrows
-- Keeps the newest keep finished tests, which the dashboard shows.
DELETE FROM setup_tests WHERE id IN (
    SELECT t.id FROM setup_tests t WHERE t.status IN ('passed', 'failed')
    ORDER BY t.created_at DESC, t.id DESC OFFSET sqlc.arg(keep) LIMIT sqlc.arg(max_rows));

-- name: ForgetSpentDispatchKeys :execrows
UPDATE dispatches SET key_sealed = NULL WHERE id IN (
    SELECT x.id FROM dispatches x WHERE x.status = 'ended' AND x.key_sealed IS NOT NULL LIMIT sqlc.arg(max_rows));

-- name: ListStuckOutbox :many
-- Rows still pending after the given time: a publication or a stop that
-- keeps failing.
SELECT id, kind, attempts, coalesce(last_error, '')::text AS last_error, created_at, available_at
FROM outbox WHERE done_at IS NULL AND kind <> 'webhook' AND created_at < sqlc.arg(before)
ORDER BY created_at, id LIMIT 100;

-- name: RetryOutboxNow :execrows
UPDATE outbox SET available_at = now() WHERE id = $1 AND done_at IS NULL;

