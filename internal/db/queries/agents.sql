-- name: CreatePairing :one
INSERT INTO agent_pairings (id, project_id, code_hash, poll_token_hash, status, name, hostname, platform, ip, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now() + make_interval(secs => sqlc.arg(ttl_seconds)::double precision))
RETURNING *;

-- name: CountLivePairingRequests :one
-- Agent-started pairings still waiting for the owner, which bound how many
-- an unauthenticated caller can open.
SELECT count(*) FROM agent_pairings
WHERE project_id = $1 AND status = 'pending' AND expires_at > now();

-- name: LockLivePairingByCode :one
SELECT * FROM agent_pairings
WHERE code_hash = $1 AND status IN ('pending', 'approved') AND expires_at > now()
FOR UPDATE;

-- name: GetPairingByCode :one
SELECT * FROM agent_pairings WHERE project_id = $1 AND code_hash = $2;

-- name: LockPairingByPollToken :one
SELECT *, (expires_at <= now())::boolean AS expired FROM agent_pairings
WHERE poll_token_hash = $1
FOR UPDATE;

-- name: SetPairingStatus :exec
UPDATE agent_pairings SET status = $2 WHERE id = $1;

-- name: ConsumePairing :exec
UPDATE agent_pairings SET status = 'consumed', agent_id = $2 WHERE id = $1;

-- name: CreateAgent :one
INSERT INTO agents (id, project_id, name, key_hash, status, hostname, platform, last_ip, last_seen_at, approved_at)
VALUES ($1, $2, $3, $4, 'approved', $5, $6, $7, now(), now())
RETURNING *;

-- name: GetAgentByKeyHash :one
SELECT * FROM agents WHERE key_hash = $1;

-- name: GetAgent :one
SELECT * FROM agents WHERE project_id = $1 AND id = $2;

-- name: ListAgents :many
SELECT * FROM agents WHERE project_id = $1
ORDER BY (status = 'revoked'), created_at DESC, id;

-- name: TouchAgent :exec
-- Coalesced: a heartbeat every 15 s writes at most once per 10 s per agent.
UPDATE agents SET last_seen_at = now(), last_ip = coalesce(sqlc.narg(ip), last_ip)
WHERE id = sqlc.arg(id) AND (last_seen_at IS NULL OR last_seen_at < now() - interval '10 seconds'
    OR last_ip IS DISTINCT FROM coalesce(sqlc.narg(ip), last_ip));

-- name: UpdateAgentReport :exec
UPDATE agents SET capabilities = $2, sandboxed = $3 WHERE id = $1;

-- name: UpdateAgent :one
UPDATE agents SET name = coalesce(sqlc.narg(name), name), scope = coalesce(sqlc.narg(scope), scope)
WHERE project_id = sqlc.arg(project_id) AND id = sqlc.arg(id) AND status <> 'revoked'
RETURNING *;

-- name: RevokeAgent :one
UPDATE agents SET status = 'revoked', revoked_at = now()
WHERE project_id = $1 AND id = $2 AND status <> 'revoked'
RETURNING *;

-- name: ListAgentLiveAttempts :many
-- An agent's attempts that may still write: running or lost, and the latest
-- of a running job.
SELECT a.id, a.job_id FROM attempts a
JOIN jobs j ON j.id = a.job_id
WHERE a.agent_id = $1 AND a.status IN ('running', 'lost') AND j.status = 'running'
  AND a.attempt_number = (SELECT max(attempt_number) FROM attempts WHERE job_id = a.job_id);

-- name: CountAgentOutputs :one
-- Current outputs an agent produced, which the owner may want re-encoded
-- after revoking it.
SELECT count(*) FROM assets s
JOIN attempts a ON a.output_id = s.current_output_id
WHERE a.agent_id = $1 AND s.status <> 'deleted';
