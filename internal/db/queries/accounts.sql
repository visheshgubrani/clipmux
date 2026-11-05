-- The owner account and its dashboard sessions.

-- name: OwnerExists :one
SELECT EXISTS (SELECT 1 FROM owners WHERE project_id = $1);

-- name: CreateOwner :one
INSERT INTO owners (id, project_id, email, password_hash) VALUES ($1, $2, $3, $4)
ON CONFLICT (project_id) DO NOTHING
RETURNING *;

-- name: GetOwnerByEmail :one
SELECT * FROM owners WHERE project_id = sqlc.arg(project_id) AND lower(email) = lower(sqlc.arg(email));

-- name: GetOwner :one
SELECT * FROM owners WHERE project_id = $1;

-- name: SetOwnerPassword :exec
UPDATE owners SET password_hash = $2, password_changed_at = now() WHERE id = $1;

-- name: CreateOwnerSession :exec
INSERT INTO owner_sessions (token_hash, project_id, owner_id, expires_at)
VALUES ($1, $2, $3, now() + make_interval(secs => sqlc.arg(lifetime_seconds)::double precision));

-- name: GetOwnerSession :one
-- A session ends at its expiry, or after idle_seconds without a request.
SELECT s.owner_id, s.project_id, o.email, (s.last_seen_at < now() - interval '1 minute')::boolean AS stale
FROM owner_sessions s JOIN owners o ON o.id = s.owner_id
WHERE s.token_hash = sqlc.arg(token_hash) AND s.expires_at > now()
  AND s.last_seen_at > now() - make_interval(secs => sqlc.arg(idle_seconds)::double precision);

-- name: TouchOwnerSession :exec
UPDATE owner_sessions SET last_seen_at = now() WHERE token_hash = $1;

-- name: DeleteOwnerSession :exec
DELETE FROM owner_sessions WHERE token_hash = $1;

-- name: DeleteOwnerSessions :execrows
DELETE FROM owner_sessions WHERE owner_id = $1;
