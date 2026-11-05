-- name: GetCloudflareConnection :one
SELECT * FROM cloudflare_connections WHERE project_id = $1;

-- name: SaveCloudflareConnection :one
INSERT INTO cloudflare_connections (project_id, account_id, zone_id, hostname, bucket, script_name, token_id, storage_id, worker_version)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (project_id) DO UPDATE SET
    account_id = EXCLUDED.account_id, zone_id = EXCLUDED.zone_id, hostname = EXCLUDED.hostname,
    bucket = EXCLUDED.bucket, script_name = EXCLUDED.script_name, token_id = EXCLUDED.token_id,
    storage_id = EXCLUDED.storage_id, worker_version = EXCLUDED.worker_version, deployed_at = now()
RETURNING *;

-- name: FindStorageConfigByBucket :one
SELECT * FROM storage_configs
WHERE project_id = $1 AND kind = 'r2' AND config->>'bucket' = sqlc.arg(bucket)::text AND config->>'account_id' = sqlc.arg(account_id)::text
ORDER BY created_at DESC
LIMIT 1;
