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
UPDATE tracks SET status = 'deleted', deleted_at = now(), retired_at = NULL, version = version + 1, updated_at = now()
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
