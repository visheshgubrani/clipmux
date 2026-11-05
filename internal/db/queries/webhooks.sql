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
