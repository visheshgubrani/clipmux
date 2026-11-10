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

-- name: ListPurgeableRetired :many
-- Retired outputs past the window a viewer mid-session may still use: the
-- larger of 7 days and the project's longest token, plus an hour.
SELECT o.id, o.asset_id, o.prefix, o.project_id, a.app_id FROM outputs o
JOIN projects p ON p.id = o.project_id
JOIN assets a ON a.id = o.asset_id
WHERE o.status = 'retired' AND o.purged_at IS NULL
  AND o.retired_at < sqlc.arg(now)::timestamptz
      - greatest(interval '7 days', make_interval(secs => p.max_token_lifetime_seconds)) - interval '1 hour'
ORDER BY o.retired_at, o.id
LIMIT sqlc.arg(max_rows);

-- name: ListPurgeableAbandoned :many
-- Writing outputs no attempt can still write: their attempt was replaced by
-- a newer one, or their job ended, at least grace ago. That outlasts the
-- 15-minute PUT URLs it was signed, so a stale worker can't recreate a file.
SELECT o.id, o.asset_id, o.prefix FROM outputs o
JOIN attempts a ON a.output_id = o.id
JOIN jobs j ON j.id = a.job_id
WHERE o.status = 'writing' AND o.purged_at IS NULL
  AND (EXISTS (SELECT 1 FROM attempts n WHERE n.job_id = a.job_id AND n.attempt_number > a.attempt_number
                 AND n.started_at < sqlc.arg(before))
       OR (j.status IN ('succeeded', 'failed', 'cancelled') AND coalesce(j.finished_at, j.created_at) < sqlc.arg(before)))
ORDER BY o.created_at, o.id
LIMIT sqlc.arg(max_rows);

-- name: MarkOutputPurged :execrows
UPDATE outputs SET purged_at = now() WHERE id = $1 AND purged_at IS NULL;

-- name: GetRetentionCursor :one
SELECT value FROM retention_cursors WHERE project_id = $1 AND name = $2;

-- name: SetRetentionCursor :exec
INSERT INTO retention_cursors (project_id, name, value) VALUES ($1, $2, $3)
ON CONFLICT (project_id, name) DO UPDATE SET value = excluded.value, updated_at = now();

-- name: ListAssetsForSweep :many
SELECT id, project_id, app_id FROM assets WHERE id > $1 AND status <> 'deleted' ORDER BY id LIMIT $2;

-- name: KnownOutputs :many
-- An asset's outputs whose files may still be needed.
SELECT id FROM outputs WHERE asset_id = $1 AND purged_at IS NULL;

-- name: PrunePlaybackHourly :execrows
DELETE FROM playback_hourly WHERE (project_id, source, hour, asset_id, playback_id) IN (
    SELECT h.project_id, h.source, h.hour, h.asset_id, h.playback_id FROM playback_hourly h
    WHERE h.hour < sqlc.arg(before) LIMIT sqlc.arg(max_rows));

-- name: PrunePlaybackDaily :execrows
DELETE FROM playback_daily WHERE (project_id, source, day, asset_id, playback_id) IN (
    SELECT d.project_id, d.source, d.day, d.asset_id, d.playback_id FROM playback_daily d
    WHERE d.day < sqlc.arg(before)::date LIMIT sqlc.arg(max_rows));

-- name: PrunePlaybackViewerDays :execrows
DELETE FROM playback_viewer_days WHERE (project_id, day, app_id, viewer) IN (
    SELECT v.project_id, v.day, v.app_id, v.viewer FROM playback_viewer_days v
    WHERE v.day < sqlc.arg(before)::date LIMIT sqlc.arg(max_rows));

-- name: PrunePlaybackViewers :execrows
DELETE FROM playback_viewers WHERE (project_id, day, asset_id, viewer) IN (
    SELECT v.project_id, v.day, v.asset_id, v.viewer FROM playback_viewers v
    WHERE v.day < sqlc.arg(before)::date LIMIT sqlc.arg(max_rows));
