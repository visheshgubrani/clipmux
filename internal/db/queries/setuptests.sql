-- The setup test.

-- name: QueueSetupTest :one
-- Nothing is returned while another test is queued or running.
INSERT INTO setup_tests (id, project_id, trigger, executor) VALUES ($1, $2, $3, $4)
ON CONFLICT (project_id) WHERE status IN ('queued', 'running') DO NOTHING
RETURNING *;

-- name: GetActiveSetupTest :one
SELECT * FROM setup_tests WHERE project_id = $1 AND status IN ('queued', 'running');

-- name: ClaimSetupTest :one
UPDATE setup_tests SET status = 'running', started_at = now()
WHERE id = (SELECT c.id FROM setup_tests c WHERE c.project_id = $1 AND c.status = 'queued' ORDER BY c.created_at LIMIT 1 FOR UPDATE SKIP LOCKED)
RETURNING *;

-- name: SetSetupTestAsset :exec
UPDATE setup_tests SET asset_id = $2 WHERE id = $1;

-- name: FinishSetupTest :exec
UPDATE setup_tests SET status = $2, checks = $3, finished_at = now() WHERE id = $1;

-- name: GetSetupTest :one
SELECT * FROM setup_tests WHERE id = $1;

-- name: LatestSetupTest :one
SELECT * FROM setup_tests WHERE project_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1;

-- name: CountSetupTests :one
SELECT count(*) FROM setup_tests WHERE project_id = $1;

-- name: ListEarlierSetupTestAssets :many
-- Earlier samples, deleted when a new test starts so they don't pile up.
SELECT DISTINCT a.id FROM setup_tests t JOIN assets a ON a.id = t.asset_id
WHERE t.project_id = $1 AND t.id <> $2 AND a.status <> 'deleted';
