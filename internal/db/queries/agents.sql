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

-- name: CreateAgentSource :one
INSERT INTO agent_sources (id, project_id, agent_id, size, upload_source) VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetAttachableAgentSource :one
-- A source an asset may take: in the project, not yet attached, from an
-- agent that isn't revoked.
SELECT g.*, a.status AS agent_status FROM agent_sources g
JOIN agents a ON a.id = g.agent_id
WHERE g.project_id = $1 AND g.id = $2
  AND NOT EXISTS (SELECT 1 FROM assets s WHERE s.agent_source_id = g.id);

-- name: CreateLocalAsset :one
INSERT INTO assets (id, project_id, app_id, status, storage_id, media_storage_id, source_uri, agent_source_id, passthrough, title)
VALUES ($1, $2, $3, 'processing', $4, coalesce(media_target($2), $4), $5, $6, $7, $8)
RETURNING *;

-- name: GetAssetAgentSource :one
SELECT g.*, a.status AS agent_status FROM assets s
JOIN agent_sources g ON g.id = s.agent_source_id
JOIN agents a ON a.id = g.agent_id
WHERE s.id = $1;

-- name: ListGoneAgentSources :many
-- Which of an agent's sources it may forget: unknown, someone else's,
-- attached to a deleted asset, or never attached a day after registering.
SELECT t.sid::text FROM unnest(sqlc.arg(ids)::text[]) AS t (sid)
WHERE NOT EXISTS (
    SELECT 1 FROM agent_sources g
    LEFT JOIN assets s ON s.agent_source_id = g.id
    WHERE g.id = t.sid AND g.agent_id = sqlc.arg(agent_id)
      AND (s.status IS DISTINCT FROM 'deleted')
      AND (s.id IS NOT NULL OR g.created_at > now() - interval '1 day'))
ORDER BY 1;

-- name: ListStrandedPinnedJobs :many
-- Queued jobs pinned to an agent that was revoked, or hasn't been seen for
-- the given time: nothing else may run them, so they end.
SELECT j.id FROM jobs j
JOIN agents a ON a.id = j.pinned_agent_id
WHERE j.status = 'queued' AND j.pinned_agent_id IS NOT NULL
  AND (a.status = 'revoked' OR coalesce(a.last_seen_at, a.created_at) < now() - make_interval(secs => sqlc.arg(unseen_seconds)::double precision))
LIMIT sqlc.arg(max_jobs);
