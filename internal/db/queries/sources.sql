-- The source mirror (T29.7, D146).

-- name: ListSourcesToMirror :many
-- Committed sources on a store, not yet copied to the backup target, oldest
-- first. A source on a PC agent is that agent's, and a source not kept is gone
-- once the asset is encoded.
SELECT id, source_uri FROM assets
WHERE source_backed_up_at IS NULL
  AND status IN ('processing', 'ready', 'errored')
  AND source_kept
  AND source_uri IS NOT NULL
  AND agent_source_id IS NULL
  AND NOT (id = ANY (sqlc.arg(exclude)::text[]))
ORDER BY created_at, id
LIMIT sqlc.arg(max_rows);

-- name: MarkSourceBackedUp :exec
-- Whatever the asset's status now: one deleted while it was copied is
-- tombstoned by the next pass.
UPDATE assets SET source_backed_up_at = now() WHERE id = $1;

-- name: ListSourcesToTombstone :many
SELECT id FROM assets
WHERE status = 'deleted' AND source_backed_up_at IS NOT NULL AND source_tombstoned_at IS NULL
ORDER BY created_at, id
LIMIT sqlc.arg(max_rows);

-- name: MarkSourceTombstoned :exec
UPDATE assets SET source_tombstoned_at = now() WHERE id = $1;

-- name: CountSourcesToMirror :one
SELECT count(*) FROM assets
WHERE source_backed_up_at IS NULL
  AND status IN ('processing', 'ready', 'errored')
  AND source_kept
  AND source_uri IS NOT NULL
  AND agent_source_id IS NULL;

-- name: ListAssetsToRestoreSources :many
-- Live assets with a source on a store: what restore-sources may bring back.
SELECT a.id, a.project_id, a.app_id, a.source_uri, a.status,
       COALESCE(o.prefix, '')::text AS output_prefix
FROM assets a
LEFT JOIN outputs o ON o.id = a.current_output_id
WHERE a.status IN ('processing', 'ready', 'errored')
  AND a.source_kept
  AND a.source_uri IS NOT NULL
  AND a.agent_source_id IS NULL
  AND (sqlc.arg(asset_id)::text = '' OR a.id = sqlc.arg(asset_id)::text)
ORDER BY a.created_at, a.id;
