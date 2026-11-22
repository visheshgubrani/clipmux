-- name: AddOriginPlayback :execrows
-- One flush of a replica's counts: added to what other replicas and
-- earlier flushes wrote. The arrays are zipped (equal lengths), one row
-- per key; a key appears once per flush.
INSERT INTO playback_hourly AS h (project_id, source, hour, app_id, asset_id, playback_id, views, watch_seconds, bytes)
SELECT u.project_id, 'origin', u.hour, u.app_id, u.asset_id, u.playback_id, u.views, u.watch_seconds, u.bytes
FROM (SELECT unnest(sqlc.arg(project_ids)::text[]) AS project_id, unnest(sqlc.arg(hours)::timestamptz[]) AS hour,
    unnest(sqlc.arg(app_ids)::text[]) AS app_id, unnest(sqlc.arg(asset_ids)::text[]) AS asset_id,
    unnest(sqlc.arg(playback_ids)::text[]) AS playback_id, unnest(sqlc.arg(views)::bigint[]) AS views,
    unnest(sqlc.arg(watch_seconds)::bigint[]) AS watch_seconds, unnest(sqlc.arg(bytes)::bigint[]) AS bytes) u
ON CONFLICT (project_id, source, hour, asset_id, playback_id) DO UPDATE SET
    views = h.views + excluded.views,
    watch_seconds = h.watch_seconds + excluded.watch_seconds,
    bytes = h.bytes + excluded.bytes;

-- name: AddOriginPlaybackDaily :execrows
-- The same flush rolled up by UTC day.
INSERT INTO playback_daily AS d (project_id, source, day, app_id, asset_id, playback_id, views, watch_seconds, bytes)
SELECT u.project_id, 'origin', (u.hour AT TIME ZONE 'UTC')::date, u.app_id, u.asset_id, u.playback_id,
    sum(u.views), sum(u.watch_seconds), sum(u.bytes)
FROM (SELECT unnest(sqlc.arg(project_ids)::text[]) AS project_id, unnest(sqlc.arg(hours)::timestamptz[]) AS hour,
    unnest(sqlc.arg(app_ids)::text[]) AS app_id, unnest(sqlc.arg(asset_ids)::text[]) AS asset_id,
    unnest(sqlc.arg(playback_ids)::text[]) AS playback_id, unnest(sqlc.arg(views)::bigint[]) AS views,
    unnest(sqlc.arg(watch_seconds)::bigint[]) AS watch_seconds, unnest(sqlc.arg(bytes)::bigint[]) AS bytes) u
GROUP BY u.project_id, 3, u.app_id, u.asset_id, u.playback_id
ON CONFLICT (project_id, source, day, asset_id, playback_id) DO UPDATE SET
    views = d.views + excluded.views,
    watch_seconds = d.watch_seconds + excluded.watch_seconds,
    bytes = d.bytes + excluded.bytes;

-- name: AddPlaybackViewers :execrows
INSERT INTO playback_viewers (project_id, day, asset_id, viewer, app_id)
SELECT u.project_id, u.day, u.asset_id, u.viewer, u.app_id
FROM (SELECT unnest(sqlc.arg(project_ids)::text[]) AS project_id, unnest(sqlc.arg(days)::date[]) AS day,
    unnest(sqlc.arg(asset_ids)::text[]) AS asset_id, unnest(sqlc.arg(viewers)::bytea[]) AS viewer,
    unnest(sqlc.arg(app_ids)::text[]) AS app_id) u
ON CONFLICT DO NOTHING;

-- name: AddPlaybackViewerDays :execrows
-- The same viewers once per app per day, whatever they watched.
INSERT INTO playback_viewer_days (project_id, day, app_id, viewer)
SELECT DISTINCT u.project_id, u.day, u.app_id, u.viewer
FROM (SELECT unnest(sqlc.arg(project_ids)::text[]) AS project_id, unnest(sqlc.arg(days)::date[]) AS day,
    unnest(sqlc.arg(viewers)::bytea[]) AS viewer, unnest(sqlc.arg(app_ids)::text[]) AS app_id) u
ON CONFLICT DO NOTHING;

-- name: AddPlaybackViewersHourly :execrows
INSERT INTO playback_viewers_hourly (project_id, hour, asset_id, viewer, app_id)
SELECT u.project_id, u.hour, u.asset_id, u.viewer, u.app_id
FROM (SELECT unnest(sqlc.arg(project_ids)::text[]) AS project_id, unnest(sqlc.arg(hours)::timestamptz[]) AS hour,
    unnest(sqlc.arg(asset_ids)::text[]) AS asset_id, unnest(sqlc.arg(viewers)::bytea[]) AS viewer,
    unnest(sqlc.arg(app_ids)::text[]) AS app_id) u
ON CONFLICT DO NOTHING;

-- name: AddPlaybackViewerHours :execrows
INSERT INTO playback_viewer_hours (project_id, hour, app_id, viewer)
SELECT DISTINCT u.project_id, u.hour, u.app_id, u.viewer
FROM (SELECT unnest(sqlc.arg(project_ids)::text[]) AS project_id, unnest(sqlc.arg(hours)::timestamptz[]) AS hour,
    unnest(sqlc.arg(viewers)::bytea[]) AS viewer, unnest(sqlc.arg(app_ids)::text[]) AS app_id) u
ON CONFLICT DO NOTHING;

-- name: GetAnalyticsKey :one
SELECT analytics_key_sealed FROM projects WHERE id = $1;

-- name: SetAnalyticsKeyIfUnset :exec
-- Another serve may have set one first; the caller reads back what won.
UPDATE projects SET analytics_key_sealed = sqlc.arg(sealed) WHERE id = sqlc.arg(id) AND analytics_key_sealed IS NULL;

-- The reads below sum every source. Hourly ones serve the 24-hour range,
-- daily ones the others; the caller zero-fills and splits off the
-- previous range.

-- name: HourlySeries :many
SELECT hour AS bucket, sum(views)::bigint AS views, sum(watch_seconds)::bigint AS watch_seconds, sum(bytes)::bigint AS bytes
FROM playback_hourly
WHERE project_id = sqlc.arg(project_id) AND hour >= sqlc.arg(start) AND hour < sqlc.arg(stop)
    AND (sqlc.narg(app_id)::text IS NULL OR app_id = sqlc.narg(app_id))
GROUP BY hour ORDER BY hour;

-- name: AssetHourlySeries :many
SELECT hour AS bucket, sum(views)::bigint AS views, sum(watch_seconds)::bigint AS watch_seconds, sum(bytes)::bigint AS bytes
FROM playback_hourly
WHERE project_id = sqlc.arg(project_id) AND asset_id = sqlc.arg(asset_id) AND hour >= sqlc.arg(start) AND hour < sqlc.arg(stop)
GROUP BY hour ORDER BY hour;

-- name: DailySeries :many
SELECT day, sum(views)::bigint AS views, sum(watch_seconds)::bigint AS watch_seconds, sum(bytes)::bigint AS bytes
FROM playback_daily
WHERE project_id = sqlc.arg(project_id) AND day >= sqlc.arg(start)::date AND day < sqlc.arg(stop)::date
    AND (sqlc.narg(app_id)::text IS NULL OR app_id = sqlc.narg(app_id))
GROUP BY day ORDER BY day;

-- name: AssetDailySeries :many
SELECT day, sum(views)::bigint AS views, sum(watch_seconds)::bigint AS watch_seconds, sum(bytes)::bigint AS bytes
FROM playback_daily
WHERE project_id = sqlc.arg(project_id) AND asset_id = sqlc.arg(asset_id) AND day >= sqlc.arg(start)::date AND day < sqlc.arg(stop)::date
GROUP BY day ORDER BY day;

-- name: CountViewers :one
-- Distinct viewers on the current and the previous days, for a project or
-- one app. GROUP BY hashes where count(DISTINCT) would sort.
SELECT
    (SELECT count(*) FROM (SELECT vd.viewer FROM playback_viewer_days vd
        WHERE vd.project_id = sqlc.arg(project_id) AND vd.day >= sqlc.arg(cur_from)::date AND vd.day <= sqlc.arg(cur_to)::date
            AND (sqlc.narg(app_id)::text IS NULL OR vd.app_id = sqlc.narg(app_id))
        GROUP BY vd.viewer) c) AS current,
    (SELECT count(*) FROM (SELECT vd.viewer FROM playback_viewer_days vd
        WHERE vd.project_id = sqlc.arg(project_id) AND vd.day >= sqlc.arg(prev_from)::date AND vd.day <= sqlc.arg(prev_to)::date
            AND (sqlc.narg(app_id)::text IS NULL OR vd.app_id = sqlc.narg(app_id))
        GROUP BY vd.viewer) p) AS previous;

-- name: CountAssetViewers :one
SELECT
    (SELECT count(*) FROM (SELECT vw.viewer FROM playback_viewers vw
        WHERE vw.project_id = sqlc.arg(project_id) AND vw.asset_id = sqlc.arg(asset_id)
            AND vw.day >= sqlc.arg(cur_from)::date AND vw.day <= sqlc.arg(cur_to)::date
        GROUP BY vw.viewer) c) AS current,
    (SELECT count(*) FROM (SELECT vw.viewer FROM playback_viewers vw
        WHERE vw.project_id = sqlc.arg(project_id) AND vw.asset_id = sqlc.arg(asset_id)
            AND vw.day >= sqlc.arg(prev_from)::date AND vw.day <= sqlc.arg(prev_to)::date
        GROUP BY vw.viewer) p) AS previous;

-- name: CountViewersHourly :one
SELECT
    (SELECT count(*) FROM (SELECT vh.viewer FROM playback_viewer_hours vh
        WHERE vh.project_id = sqlc.arg(project_id) AND vh.hour >= sqlc.arg(cur_from) AND vh.hour < sqlc.arg(cur_to)
            AND (sqlc.narg(app_id)::text IS NULL OR vh.app_id = sqlc.narg(app_id))
        GROUP BY vh.viewer) c) AS current,
    (SELECT count(*) FROM (SELECT vh.viewer FROM playback_viewer_hours vh
        WHERE vh.project_id = sqlc.arg(project_id) AND vh.hour >= sqlc.arg(prev_from) AND vh.hour < sqlc.arg(prev_to)
            AND (sqlc.narg(app_id)::text IS NULL OR vh.app_id = sqlc.narg(app_id))
        GROUP BY vh.viewer) p) AS previous;

-- name: CountAssetViewersHourly :one
SELECT
    (SELECT count(*) FROM (SELECT vh.viewer FROM playback_viewers_hourly vh
        WHERE vh.project_id = sqlc.arg(project_id) AND vh.asset_id = sqlc.arg(asset_id)
            AND vh.hour >= sqlc.arg(cur_from) AND vh.hour < sqlc.arg(cur_to)
        GROUP BY vh.viewer) c) AS current,
    (SELECT count(*) FROM (SELECT vh.viewer FROM playback_viewers_hourly vh
        WHERE vh.project_id = sqlc.arg(project_id) AND vh.asset_id = sqlc.arg(asset_id)
            AND vh.hour >= sqlc.arg(prev_from) AND vh.hour < sqlc.arg(prev_to)
        GROUP BY vh.viewer) p) AS previous;

-- name: TopAssetsHourly :many
-- The most viewed live assets in a 24-hour range (or by bytes when sampled views read zero).
SELECT h.asset_id, a.title, sum(h.views)::bigint AS views, sum(h.watch_seconds)::bigint AS watch_seconds,
    sum(h.bytes)::bigint AS bytes
FROM playback_hourly h
JOIN assets a ON a.id = h.asset_id AND a.project_id = h.project_id AND a.status <> 'deleted'
WHERE h.project_id = sqlc.arg(project_id) AND h.hour >= sqlc.arg(start) AND h.hour < sqlc.arg(stop)
    AND (sqlc.narg(app_id)::text IS NULL OR h.app_id = sqlc.narg(app_id))
GROUP BY h.asset_id, a.title
HAVING sum(h.views) > 0 OR sum(h.bytes) > 0
ORDER BY sum(h.views) DESC, sum(h.bytes) DESC, h.asset_id
LIMIT 10;

-- name: TopAssetsDaily :many
SELECT d.asset_id, a.title, sum(d.views)::bigint AS views, sum(d.watch_seconds)::bigint AS watch_seconds,
    sum(d.bytes)::bigint AS bytes
FROM playback_daily d
JOIN assets a ON a.id = d.asset_id AND a.project_id = d.project_id AND a.status <> 'deleted'
WHERE d.project_id = sqlc.arg(project_id) AND d.day >= sqlc.arg(start)::date AND d.day < sqlc.arg(stop)::date
    AND (sqlc.narg(app_id)::text IS NULL OR d.app_id = sqlc.narg(app_id))
GROUP BY d.asset_id, a.title
HAVING sum(d.views) > 0 OR sum(d.bytes) > 0
ORDER BY sum(d.views) DESC, sum(d.bytes) DESC, d.asset_id
LIMIT 10;

-- name: StorageTotals :one
SELECT
    coalesce((SELECT sum(a.source_bytes) FROM assets a
        WHERE a.project_id = sqlc.arg(project_id) AND (sqlc.narg(app_id)::text IS NULL OR a.app_id = sqlc.narg(app_id))
          AND a.source_kept AND a.agent_source_id IS NULL AND a.deleted_at IS NULL), 0)::bigint AS source_bytes,
    (coalesce((SELECT sum(o.bytes) FROM outputs o JOIN assets a ON a.id = o.asset_id
        WHERE a.project_id = sqlc.arg(project_id) AND (sqlc.narg(app_id)::text IS NULL OR a.app_id = sqlc.narg(app_id))
          AND o.status IN ('committed', 'retired') AND o.purged_at IS NULL AND a.deleted_at IS NULL), 0)
     + coalesce((SELECT sum(t.bytes) FROM tracks t JOIN assets a ON a.id = t.asset_id
        WHERE a.project_id = sqlc.arg(project_id) AND (sqlc.narg(app_id)::text IS NULL OR a.app_id = sqlc.narg(app_id))
          AND t.status IN ('publishing', 'ready', 'retired') AND a.deleted_at IS NULL), 0))::bigint AS media_bytes;

-- name: AssetStorageTotals :one
SELECT
    coalesce((SELECT a.source_bytes FROM assets a
        WHERE a.project_id = sqlc.arg(project_id) AND a.id = sqlc.arg(asset_id)
          AND a.source_kept AND a.agent_source_id IS NULL AND a.deleted_at IS NULL), 0)::bigint AS source_bytes,
    (coalesce((SELECT sum(o.bytes) FROM outputs o JOIN assets a ON a.id = o.asset_id
        WHERE a.project_id = sqlc.arg(project_id) AND a.id = sqlc.arg(asset_id)
          AND o.status IN ('committed', 'retired') AND o.purged_at IS NULL AND a.deleted_at IS NULL), 0)
     + coalesce((SELECT sum(t.bytes) FROM tracks t JOIN assets a ON a.id = t.asset_id
        WHERE a.project_id = sqlc.arg(project_id) AND a.id = sqlc.arg(asset_id)
          AND t.status IN ('publishing', 'ready', 'retired') AND a.deleted_at IS NULL), 0))::bigint AS media_bytes;

-- name: GetAnalyticsAsset :one
SELECT app_id FROM assets WHERE project_id = $1 AND id = $2;

-- Edge analytics (P7-T05).

-- name: GetEdgeProjectInfo :one
SELECT p.analytics_install_id,
       (cc.project_id IS NOT NULL)::boolean AS cloudflare_connected,
       coalesce(cc.account_id, '')::text AS account_id,
       cc.connected_at,
       coalesce(cc.worker_version, '')::text AS worker_version,
       coalesce(cc.analytics_tagged, false)::boolean AS analytics_tagged,
       cc.analytics_legacy_until
FROM projects p
LEFT JOIN cloudflare_connections cc ON cc.project_id = p.id
WHERE p.id = $1;

-- name: SaveEdgeAnalytics :exec
-- A new token keeps the watermark: reconnecting doesn't pull again.
INSERT INTO edge_analytics (project_id, account_id, token_sealed, pulled_through)
VALUES (sqlc.arg(project_id), sqlc.arg(account_id), sqlc.arg(token_sealed), sqlc.arg(pulled_through))
ON CONFLICT (project_id) DO UPDATE SET
    account_id = excluded.account_id, token_sealed = excluded.token_sealed, last_error = NULL;

-- name: GetEdgeAnalytics :one
SELECT * FROM edge_analytics WHERE project_id = $1;

-- name: DeleteEdgeAnalytics :execrows
DELETE FROM edge_analytics WHERE project_id = $1;

-- name: EdgeAnalyticsConnected :one
SELECT EXISTS (SELECT 1 FROM edge_analytics WHERE project_id = $1);

-- name: MarkEdgePulled :exec
UPDATE edge_analytics SET pulled_through = sqlc.arg(pulled_through), last_pull_at = now(), last_error = NULL
WHERE project_id = sqlc.arg(project_id);

-- name: MarkEdgeFailed :exec
UPDATE edge_analytics SET last_pull_at = now(), last_error = sqlc.arg(last_error) WHERE project_id = sqlc.arg(project_id);

-- name: DeleteEdgeHourly :execrows
DELETE FROM playback_hourly
WHERE project_id = sqlc.arg(project_id) AND source = 'edge' AND hour >= sqlc.arg(start) AND hour < sqlc.arg(stop);

-- name: AddEdgeHourly :execrows
-- The pass's absolute totals for its hours, after DeleteEdgeHourly.
INSERT INTO playback_hourly (project_id, source, hour, app_id, asset_id, playback_id, views, watch_seconds, bytes)
SELECT sqlc.arg(project_id), 'edge', u.hour, u.app_id, u.asset_id, u.playback_id, u.views, u.watch_seconds, u.bytes
FROM (SELECT unnest(sqlc.arg(hours)::timestamptz[]) AS hour,
    unnest(sqlc.arg(app_ids)::text[]) AS app_id, unnest(sqlc.arg(asset_ids)::text[]) AS asset_id,
    unnest(sqlc.arg(playback_ids)::text[]) AS playback_id, unnest(sqlc.arg(views)::bigint[]) AS views,
    unnest(sqlc.arg(watch_seconds)::bigint[]) AS watch_seconds, unnest(sqlc.arg(bytes)::bigint[]) AS bytes) u;

-- name: DeleteEdgeDaily :execrows
DELETE FROM playback_daily
WHERE project_id = sqlc.arg(project_id) AND source = 'edge' AND day >= sqlc.arg(start)::date AND day < sqlc.arg(stop)::date;

-- name: AddEdgeDaily :execrows
-- Rebuilds the edge daily rows of [start, stop) from the edge hourly rows,
-- which hold every hour of those days, after DeleteEdgeDaily.
INSERT INTO playback_daily (project_id, source, day, app_id, asset_id, playback_id, views, watch_seconds, bytes)
SELECT h.project_id, 'edge', (h.hour AT TIME ZONE 'UTC')::date, h.app_id, h.asset_id, h.playback_id,
    sum(h.views), sum(h.watch_seconds), sum(h.bytes)
FROM playback_hourly h
WHERE h.project_id = sqlc.arg(project_id) AND h.source = 'edge'
    AND (h.hour AT TIME ZONE 'UTC')::date >= sqlc.arg(start)::date AND (h.hour AT TIME ZONE 'UTC')::date < sqlc.arg(stop)::date
GROUP BY h.project_id, 3, h.app_id, h.asset_id, h.playback_id;
