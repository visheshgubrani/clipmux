-- Counts for /metrics (P5-T03). Each is read at most every 10 s.

-- name: MetricActiveJobs :many
SELECT kind, status, coalesce(routed_to, '')::text AS routed_to, coalesce(route_reason, '')::text AS route_reason,
       count(*)::bigint AS n
FROM jobs WHERE status IN ('queued', 'running')
GROUP BY 1, 2, 3, 4;

-- name: MetricOldestQueued :many
SELECT kind, extract(epoch FROM now() - min(available_at))::double precision AS seconds
FROM jobs WHERE status = 'queued' AND available_at <= now()
GROUP BY kind;

-- name: MetricFinishedAttempts :many
SELECT executor, status, coalesce(error_code, '')::text AS error_code, count(*)::bigint AS n
FROM attempts WHERE status <> 'running'
GROUP BY 1, 2, 3;

-- name: MetricPendingOutbox :many
SELECT kind, count(*)::bigint AS n,
       extract(epoch FROM now() - min(created_at))::double precision AS oldest_seconds
FROM outbox WHERE done_at IS NULL
GROUP BY kind;

-- name: MetricWebhookDeliveries :many
SELECT status, count(*)::bigint AS n FROM webhook_deliveries GROUP BY status;

-- name: MetricDispatches :many
SELECT kind, status, count(*)::bigint AS n FROM dispatches GROUP BY 1, 2;

-- name: MetricWorkersOnline :one
SELECT (SELECT count(*) FROM agents
        WHERE status = 'approved' AND last_seen_at > now() - make_interval(secs => sqlc.arg(online_seconds)::double precision))::bigint AS agents,
       (SELECT count(*) FROM projects
        WHERE builtin_seen_at > now() - make_interval(secs => sqlc.arg(online_seconds)::double precision))::bigint AS builtin;
