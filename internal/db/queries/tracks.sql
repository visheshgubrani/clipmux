-- Tracks (P5-T13; docs/plans/P5-production.md, "Tracks"): subtitles and
-- chapters, per asset. Lock order: job, then attempt, then asset, then
-- track.

-- name: CreateTrack :one
INSERT INTO tracks (id, project_id, asset_id, kind)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetTrack :one
SELECT * FROM tracks WHERE id = $1;

-- name: LockTrack :one
SELECT * FROM tracks WHERE id = $1 FOR UPDATE;

-- name: ListTracksForAsset :many
SELECT * FROM tracks WHERE asset_id = $1 ORDER BY created_at, id;

-- name: ListTracksToTranscribe :many
-- Subtitles tracks of the asset still being made with no transcription
-- queued or running: an output just committed, so each can start.
SELECT t.* FROM tracks t
WHERE t.asset_id = $1 AND t.kind = 'subtitles' AND t.status = 'preparing'
  AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.track_id = t.id AND j.status IN ('queued', 'running'))
ORDER BY t.created_at, t.id
FOR UPDATE OF t;

-- name: ErrorTrack :one
-- A track being made fails; one already errored, ready or deleted is left
-- as it is.
UPDATE tracks SET status = 'errored', error_code = $2, version = version + 1, updated_at = now()
WHERE id = $1 AND status IN ('preparing', 'publishing')
RETURNING *;

-- name: ErrorPreparingTracksOfKind :many
-- The cascade: chapters can't be made once their subtitles failed.
UPDATE tracks SET status = 'errored', error_code = $3, version = version + 1, updated_at = now()
WHERE asset_id = $1 AND kind = $2 AND status = 'preparing'
RETURNING *;

-- name: MarkTracksDeleted :many
-- error_code goes too: only an errored track keeps one (tracks_check).
UPDATE tracks SET status = 'deleted', deleted_at = now(), retired_at = NULL, error_code = NULL, version = version + 1, updated_at = now()
WHERE asset_id = $1 AND status <> 'deleted'
RETURNING id;

-- name: DeleteTranscriptsForAsset :execrows
DELETE FROM transcripts WHERE asset_id = $1;

-- name: ListStaleTranscriptions :many
-- Transcriptions no worker could take within the wait: queued since they
-- were created, never claimed.
SELECT j.id FROM jobs j
WHERE j.project_id = sqlc.arg(project_id) AND j.kind = 'transcribe' AND j.status = 'queued'
  AND j.created_at < now() - make_interval(secs => sqlc.arg(wait_seconds)::double precision)
  AND NOT EXISTS (SELECT 1 FROM attempts a WHERE a.job_id = j.id)
ORDER BY j.created_at, j.id
LIMIT sqlc.arg(max_jobs);

-- name: BumpAssetForTrack :exec
-- A track's change shows in the asset's view (D020).
UPDATE assets SET version = version + 1, updated_at = now() WHERE id = $1;

-- name: InsertTranscript :exec
INSERT INTO transcripts (track_id, project_id, asset_id, body) VALUES ($1, $2, $3, $4);

-- name: PublishTrack :one
-- A transcription's VTT is stored: the track waits for its routes.
UPDATE tracks
SET status = 'publishing', language = $2, name = $3, duration_ms = $4, key_version = $5, output_id = $6,
    version = version + 1, updated_at = now()
WHERE id = $1 AND status = 'preparing'
RETURNING *;

-- name: GetTranscript :one
SELECT * FROM transcripts WHERE track_id = $1;

-- name: GetAIConnection :one
SELECT * FROM ai_connections WHERE project_id = $1 AND purpose = $2;

-- name: UpsertAIConnection :one
-- Connect saves a verified connection, replacing the project's one for
-- that purpose; the key is sealed for this row's ID.
INSERT INTO ai_connections (id, project_id, purpose, provider, base_url, model, response_format, key_sealed, verified_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())
ON CONFLICT (project_id, purpose) DO UPDATE
SET provider = EXCLUDED.provider, base_url = EXCLUDED.base_url, model = EXCLUDED.model,
    response_format = EXCLUDED.response_format, key_sealed = EXCLUDED.key_sealed,
    verified_at = now(), updated_at = now()
RETURNING *;

-- name: DeleteAIConnection :execrows
DELETE FROM ai_connections WHERE project_id = $1 AND purpose = $2;

-- name: GetSpeechMode :one
SELECT speech_mode FROM projects WHERE id = $1;

-- name: SetSpeechMode :exec
UPDATE projects SET speech_mode = $2 WHERE id = $1;

-- name: ListServableTracks :many
-- The tracks delivery may serve for an asset: stored (publishing), ready,
-- or retired within the window; a route record carries exactly these.
SELECT id, kind, status, language, name, duration_ms, key_version FROM tracks
WHERE asset_id = $1 AND status IN ('publishing', 'ready', 'retired')
ORDER BY id;

-- name: ListServableTracksForProject :many
SELECT t.asset_id, t.id, t.kind, t.status, t.language, t.name, t.duration_ms, t.key_version FROM tracks t
WHERE t.project_id = $1 AND t.status IN ('publishing', 'ready', 'retired')
ORDER BY t.asset_id, t.id;

-- name: CountPendingRoutesForTrack :one
SELECT count(*) FROM outbox
WHERE kind = 'route_sync' AND done_at IS NULL AND asset_id = sqlc.arg(asset_id)
  AND payload->>'track_id' = sqlc.arg(track_id)::text;

-- name: ListPublishingTracks :many
SELECT id FROM tracks WHERE asset_id = $1 AND status = 'publishing' ORDER BY id;

-- name: MarkTrackReady :one
UPDATE tracks SET status = 'ready', ready_at = now(), version = version + 1, updated_at = now()
WHERE id = $1 AND status = 'publishing'
RETURNING *;

-- name: RetireReadyTracksOfKind :many
-- A newer track of the kind became ready: the old ones stay servable for
-- the retire window, so a session holding an old master still loads them.
UPDATE tracks SET status = 'retired', retired_at = now(), version = version + 1, updated_at = now()
WHERE asset_id = $1 AND kind = $2 AND status = 'ready' AND id <> sqlc.arg(keep)::text
RETURNING id;

-- name: ListPurgeableRetiredTracks :many
-- Retired tracks past the same window as retired outputs (D070).
SELECT t.id, t.asset_id, t.project_id, a.app_id FROM tracks t
JOIN assets a ON a.id = t.asset_id
JOIN projects p ON p.id = t.project_id
WHERE t.status = 'retired'
  AND t.retired_at < sqlc.arg(now)::timestamptz
      - greatest(interval '7 days', make_interval(secs => p.max_token_lifetime_seconds)) - interval '1 hour'
ORDER BY t.retired_at, t.id
LIMIT sqlc.arg(max_rows);

-- name: PurgeTrack :execrows
-- Its file goes next: it stops being servable first.
UPDATE tracks SET status = 'deleted', deleted_at = now(), retired_at = NULL, version = version + 1, updated_at = now()
WHERE id = $1 AND status = 'retired';

-- name: SetAssetDuration :exec
-- The current output's duration, set as it commits.
UPDATE assets SET duration_seconds = $2 WHERE id = $1;

-- name: ListTracksForAssets :many
-- The tracks an asset view shows, for a page of assets at once (D072).
SELECT * FROM tracks WHERE asset_id = ANY (sqlc.arg(asset_ids)::text[]) AND status NOT IN ('retired', 'deleted')
ORDER BY asset_id, created_at, id;

-- name: DeleteTrack :one
UPDATE tracks SET status = 'deleted', deleted_at = now(), retired_at = NULL, error_code = NULL, version = version + 1, updated_at = now()
WHERE id = $1 AND status <> 'deleted'
RETURNING *;

-- name: DeleteTranscript :exec
DELETE FROM transcripts WHERE track_id = $1;

-- name: GetTrackOfKind :one
-- An asset's newest track of a kind in one of the states given.
SELECT * FROM tracks WHERE asset_id = $1 AND kind = $2 AND status = ANY (sqlc.arg(statuses)::text[])
ORDER BY created_at DESC, id DESC LIMIT 1;

-- name: PublishChaptersTrack :one
-- A chapters job's VTT is stored: the track waits for its routes.
UPDATE tracks
SET status = 'publishing', language = $2, name = $3, duration_ms = $4, key_version = $5, output_id = $6,
    chapters = $7, version = version + 1, updated_at = now()
WHERE id = $1 AND status = 'preparing' AND kind = 'chapters'
RETURNING *;

-- name: ProjectCanTranscribe :one
-- Whether something can transcribe right now: the built-in worker or an
-- online PC that reported speech, or a connected speech API in a mode that
-- uses it. The setup test asks for subtitles only then.
SELECT ((p.builtin_speech AND coalesce(p.builtin_seen_at > now() - make_interval(secs => sqlc.arg(online_seconds)::double precision), false))
    OR EXISTS (SELECT 1 FROM agents g WHERE g.project_id = p.id AND g.status = 'approved'
               AND g.last_seen_at > now() - make_interval(secs => sqlc.arg(online_seconds)::double precision)
               AND jsonb_typeof(g.capabilities -> 'speech') = 'object')
    OR (p.speech_mode <> 'local' AND EXISTS (SELECT 1 FROM ai_connections c WHERE c.project_id = p.id
               AND c.purpose = 'speech' AND c.verified_at IS NOT NULL)))::boolean
FROM projects p WHERE p.id = sqlc.arg(project_id);
