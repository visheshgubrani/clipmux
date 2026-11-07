-- Webhook endpoints and deliveries (docs/architecture.md, "Webhooks").

-- name: CreateWebhookEndpoint :one
INSERT INTO webhook_endpoints (id, project_id, app_id, url, secret_encrypted)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: LockPendingWebhookEvents :many
SELECT id, project_id, app_id FROM outbox
WHERE kind = 'webhook' AND done_at IS NULL AND available_at <= now()
ORDER BY created_at, id
LIMIT $1
FOR UPDATE SKIP LOCKED;

-- name: ListEnabledEndpointsForApp :many
SELECT id FROM webhook_endpoints WHERE app_id = $1 AND disabled_at IS NULL ORDER BY created_at, id;

-- name: InsertWebhookDelivery :exec
INSERT INTO webhook_deliveries (id, project_id, event_id, endpoint_id, next_attempt_at, give_up_at)
VALUES ($1, $2, $3, $4, sqlc.arg(now), sqlc.arg(give_up_at))
ON CONFLICT (event_id, endpoint_id) DO NOTHING;

-- name: ClaimWebhookDeliveries :many
-- Takes due deliveries and leases them: next_attempt_at moves to lease_until,
-- so a sender that dies mid-request is retried once the lease passes.
UPDATE webhook_deliveries d
SET attempts = d.attempts + 1, next_attempt_at = sqlc.arg(lease_until)
FROM outbox e, webhook_endpoints w, apps a
WHERE d.id IN (
        SELECT p.id FROM webhook_deliveries p
        WHERE p.status = 'pending' AND p.next_attempt_at <= sqlc.arg(now)
          -- an endpoint whose breaker is open is skipped, not charged a try
          AND NOT EXISTS (SELECT 1 FROM webhook_endpoints o
                          WHERE o.id = p.endpoint_id AND o.circuit_open_until > sqlc.arg(now))
        ORDER BY p.next_attempt_at, p.id
        LIMIT sqlc.arg(max_deliveries)
        FOR UPDATE SKIP LOCKED)
  AND e.id = d.event_id AND w.id = d.endpoint_id AND a.id = w.app_id
RETURNING d.id, d.attempts, d.give_up_at, d.endpoint_id, e.id AS event_id, (e.payload->>'type')::text AS event_type,
          e.payload, w.url, w.secret_encrypted,
          (w.disabled_at IS NOT NULL)::boolean AS endpoint_disabled, (a.disabled_at IS NOT NULL)::boolean AS app_disabled;

-- name: FinishWebhookDelivery :execrows
-- Fenced on attempts: a sender whose lease passed can't overwrite a newer try.
UPDATE webhook_deliveries
SET status = sqlc.arg(status), next_attempt_at = NULL, finished_at = sqlc.arg(now),
    last_status = sqlc.narg(last_status), last_error = sqlc.narg(last_error)
WHERE id = sqlc.arg(id) AND attempts = sqlc.arg(attempts) AND status = 'pending';

-- name: RetryWebhookDelivery :execrows
UPDATE webhook_deliveries
SET next_attempt_at = sqlc.arg(next_attempt_at), last_status = sqlc.narg(last_status), last_error = sqlc.narg(last_error)
WHERE id = sqlc.arg(id) AND attempts = sqlc.arg(attempts) AND status = 'pending';

-- name: WebhookEndpointSucceeded :exec
UPDATE webhook_endpoints SET consecutive_failures = 0, circuit_open_until = NULL
WHERE id = $1 AND (consecutive_failures <> 0 OR circuit_open_until IS NOT NULL);

-- name: WebhookEndpointFailed :one
-- After breaker_after failures in a row the endpoint is skipped for a
-- minute, doubling with each further failure to at most an hour.
UPDATE webhook_endpoints SET
    consecutive_failures = consecutive_failures + 1,
    circuit_open_until = CASE WHEN consecutive_failures + 1 >= sqlc.arg(breaker_after)::integer
        THEN sqlc.arg(now)::timestamptz + least(interval '1 hour',
             interval '1 minute' * power(2, consecutive_failures + 1 - sqlc.arg(breaker_after)::integer))
        ELSE circuit_open_until END
WHERE id = sqlc.arg(id)
RETURNING consecutive_failures, circuit_open_until;

-- name: GetWebhookEndpoint :one
SELECT * FROM webhook_endpoints WHERE id = $1;

-- name: SetPendingWebhookSecret :execrows
UPDATE webhook_endpoints SET pending_secret_encrypted = $2, pending_secret_at = now()
WHERE id = $1 AND disabled_at IS NULL;

-- name: ActivateWebhookSecret :execrows
UPDATE webhook_endpoints SET secret_encrypted = $2, pending_secret_encrypted = NULL, pending_secret_at = NULL
WHERE id = $1 AND pending_secret_encrypted IS NOT NULL;

-- name: ListEndpointDeliveries :many
SELECT d.id, d.event_id, (e.payload->>'type')::text AS event_type, d.status, d.attempts, d.last_status,
       d.last_error, d.created_at, d.finished_at, d.next_attempt_at
FROM webhook_deliveries d JOIN outbox e ON e.id = d.event_id
WHERE d.endpoint_id = $1
ORDER BY d.created_at DESC, d.id DESC
LIMIT 100;

-- name: RedeliverWebhook :one
-- Only a delivery that gave up can be sent again, with a fresh window.
UPDATE webhook_deliveries SET status = 'pending', attempts = 0, next_attempt_at = sqlc.arg(now),
    give_up_at = sqlc.arg(give_up_at), finished_at = NULL, last_status = NULL, last_error = NULL
WHERE id = sqlc.arg(id) AND project_id = sqlc.arg(project_id) AND status = 'failed'
RETURNING id, endpoint_id;
