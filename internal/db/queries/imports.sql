-- Import from URL (P2-T06).

-- name: CreateImportedAsset :one
INSERT INTO assets (id, project_id, app_id, status, storage_id, media_storage_id, passthrough, title, renditions)
VALUES ($1, $2, $3, 'processing', $4, coalesce(media_target($2), $4), $5, $6, $7)
RETURNING *;

-- name: CreateImportJob :one
INSERT INTO jobs (id, project_id, asset_id, kind, import_url)
VALUES ($1, $2, $3, 'import', $4)
RETURNING *;

-- name: ClaimImportJob :one
-- serve fetches imports itself; a claim leases the job for the fetch.
UPDATE jobs SET status = 'running', started_at = coalesce(started_at, now()),
    available_at = now() + make_interval(secs => sqlc.arg(lease_seconds)::double precision)
WHERE id = (
    SELECT c.id FROM jobs c
    WHERE c.kind = 'import' AND c.status IN ('queued', 'running') AND c.available_at <= now()
    ORDER BY c.available_at, c.id
    LIMIT 1
    FOR UPDATE SKIP LOCKED)
RETURNING *;

-- name: RetryImportJob :exec
UPDATE jobs SET status = 'queued', failed_count = failed_count + 1, error_code = sqlc.arg(error_code),
    available_at = now() + make_interval(secs => sqlc.arg(delay_seconds)::double precision)
WHERE id = sqlc.arg(id);

-- name: SetAssetSource :exec
UPDATE assets SET source_uri = $2, source_kept = true, updated_at = now() WHERE id = $1;
