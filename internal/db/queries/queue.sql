-- Leased job queue (docs/architecture.md, "Compute" and "Leases").
-- Lock order everywhere: job, then attempt, then asset.

-- name: CreateJob :one
-- Worker jobs start routed: a pinned job to its agent, a setup test's
-- sample to the executor that test names, any other to the project's
-- primary executor. The router may move a queued one later.
INSERT INTO jobs (id, project_id, asset_id, kind, pinned_agent_id, track_id, routed_to, route_reason)
SELECT sqlc.arg(id), p.id, sqlc.arg(asset_id), sqlc.arg(kind)::text, sqlc.narg(pinned_agent_id)::text, sqlc.narg(track_id)::text,
       CASE WHEN sqlc.arg(kind)::text NOT IN ('transcode', 'reencode', 'upload_source', 'transcribe') THEN NULL
            WHEN sqlc.narg(pinned_agent_id)::text IS NOT NULL THEN 'agents'
            ELSE coalesce(t.executor, p.primary_executor) END,
       CASE WHEN sqlc.arg(kind)::text NOT IN ('transcode', 'reencode', 'upload_source', 'transcribe') THEN NULL
            WHEN sqlc.narg(pinned_agent_id)::text IS NOT NULL THEN 'pinned'
            WHEN t.executor IS NOT NULL THEN 'setup_test'
            ELSE 'primary' END
FROM projects p
LEFT JOIN setup_tests t ON t.asset_id = sqlc.arg(asset_id) AND t.executor IS NOT NULL
WHERE p.id = sqlc.arg(project_id)
RETURNING *;

-- name: LockNextClaimableJob :one
-- Worker work only, of the kinds the worker asked for: import, move,
-- delete and chapters run in the control plane; upload_source is always
-- pinned to the PC that holds the file. A job goes to a worker whose class
-- it is routed to (pinned jobs are decided by the pin alone), and
-- job_claimable_by applies the per-worker rules.
SELECT * FROM jobs
WHERE jobs.project_id = sqlc.arg(project_id)
  AND jobs.status = 'queued'
  AND available_at <= now()
  AND kind = ANY (sqlc.arg(kinds)::text[])
  AND kind IN ('transcode', 'reencode', 'upload_source', 'transcribe')
  AND (pinned_agent_id IS NOT NULL OR routed_to = ANY (sqlc.arg(classes)::text[]))
  AND job_claimable_by(kind, asset_id, pinned_agent_id, sqlc.narg(agent_id)::text, sqlc.arg(local_only)::boolean,
                       sqlc.arg(untrusted)::boolean, sqlc.narg(free_disk_bytes)::bigint, sqlc.arg(speech)::boolean)
ORDER BY available_at, id
LIMIT 1
FOR UPDATE SKIP LOCKED;

-- name: NextAttemptNumber :one
SELECT (coalesce(max(attempt_number), 0) + 1)::integer FROM attempts WHERE job_id = $1;

-- name: LatestAttemptNumber :one
SELECT coalesce(max(attempt_number), 0)::integer FROM attempts WHERE job_id = $1;

-- name: CreateOutput :one
INSERT INTO outputs (id, project_id, asset_id, key_version)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: CreateAttempt :one
INSERT INTO attempts (id, project_id, job_id, asset_id, attempt_number, output_id, executor, agent_id,
                      token_hash, lease_expires_at, worker_info, route_reason, dispatch_id, input_output_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now() + make_interval(secs => sqlc.arg(lease_seconds)::double precision), $10, $11, $12,
        sqlc.narg(input_output_id))
RETURNING *;

-- name: StartJob :exec
UPDATE jobs
SET status = 'running', phase = NULL, progress = NULL, started_at = coalesce(started_at, now())
WHERE id = $1;

-- name: GetAttempt :one
SELECT * FROM attempts WHERE id = $1;

-- name: GetAttemptByTokenHash :one
SELECT * FROM attempts WHERE token_hash = $1;

-- name: GetJob :one
SELECT * FROM jobs WHERE id = $1;

-- name: LockJob :one
SELECT * FROM jobs WHERE id = $1 FOR UPDATE;

-- name: LockAttempt :one
SELECT sqlc.embed(attempts), (lease_expires_at < now())::boolean AS lease_expired
FROM attempts WHERE id = $1 FOR UPDATE;

-- name: RenewLease :exec
UPDATE attempts
SET status = 'running', last_heartbeat_at = now(),
    lease_expires_at = now() + make_interval(secs => sqlc.arg(lease_seconds)::double precision),
    phase = sqlc.narg(phase), progress = sqlc.narg(progress)
WHERE id = sqlc.arg(id);

-- name: SetJobRunningProgress :exec
UPDATE jobs SET status = 'running', phase = $2, progress = $3 WHERE id = $1;

-- name: ListExpiredLeases :many
SELECT id FROM attempts
WHERE status = 'running' AND lease_expires_at < now()
ORDER BY lease_expires_at
LIMIT $1;

-- name: SetAttemptStatus :exec
UPDATE attempts
SET status = $2, failure_kind = $3, error_code = $4,
    finished_at = CASE WHEN $2 IN ('committed', 'failed') THEN now() ELSE finished_at END
WHERE id = $1;

-- name: CountJobLoss :one
UPDATE jobs SET lost_count = lost_count + 1 WHERE id = $1 RETURNING lost_count;

-- name: CountJobFailure :one
UPDATE jobs SET failed_count = failed_count + 1 WHERE id = $1 RETURNING failed_count;

-- name: RequeueJob :exec
UPDATE jobs
SET status = 'queued', phase = NULL, progress = NULL,
    available_at = now() + make_interval(secs => sqlc.arg(delay_seconds)::double precision)
WHERE id = sqlc.arg(id);

-- name: FailJob :exec
UPDATE jobs SET status = 'failed', error_code = $2, finished_at = now() WHERE id = $1;

-- name: CancelJob :exec
UPDATE jobs SET status = 'cancelled', finished_at = now() WHERE id = $1 AND status IN ('queued', 'running');

-- name: LockAsset :one
SELECT * FROM assets WHERE id = $1 FOR UPDATE;

-- name: MarkAssetErrored :one
UPDATE assets
SET status = 'errored', error_code = $2, version = version + 1, updated_at = now()
WHERE id = $1
RETURNING version;

-- name: InsertOutbox :exec
INSERT INTO outbox (id, project_id, kind, app_id, asset_id, payload)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: ReserveSignedPaths :one
-- Counts paths against the attempt's budget; no row means the budget is spent.
UPDATE attempts SET signed_paths = signed_paths + sqlc.arg(n)::integer
WHERE id = sqlc.arg(id) AND signed_paths + sqlc.arg(n)::integer <= sqlc.arg(budget)::integer
RETURNING signed_paths;

-- name: DeleteAttempt :exec
DELETE FROM attempts WHERE id = $1;

-- name: DeleteWritingOutput :exec
DELETE FROM outputs WHERE id = $1 AND status = 'writing';

-- name: ListRoutableJobs :many
-- Queued worker jobs the router may move: pinned jobs never move, and a
-- job still backing off isn't routed until it could be claimed.
SELECT id FROM jobs
WHERE project_id = $1 AND status = 'queued' AND kind IN ('transcode', 'reencode', 'transcribe')
  AND pinned_agent_id IS NULL AND available_at <= now()
ORDER BY available_at, id
LIMIT $2;

-- name: LockRoutableJob :one
SELECT * FROM jobs
WHERE id = $1 AND status = 'queued' AND pinned_agent_id IS NULL AND available_at <= now()
FOR UPDATE SKIP LOCKED;

-- name: JobRouteFacts :one
-- What the router needs to place one job: the project's routing, whether
-- each pull executor has an online worker that could claim the job, whether
-- the job's asset may go to a fallback, and how long the job has waited.
SELECT j.kind, p.primary_executor, p.fallback_executor, p.queue_age_fallback_minutes,
       coalesce(p.builtin_seen_at > now() - make_interval(secs => sqlc.arg(online_seconds)::double precision)
                AND job_claimable_by(j.kind, j.asset_id, j.pinned_agent_id, NULL, false, false, p.builtin_free_disk_bytes,
                                     p.builtin_speech),
                false)::boolean AS builtin_up,
       EXISTS (
           SELECT 1 FROM agents g
           WHERE g.project_id = j.project_id AND g.status = 'approved'
             AND g.last_seen_at > now() - make_interval(secs => sqlc.arg(online_seconds)::double precision)
             AND job_claimable_by(j.kind, j.asset_id, j.pinned_agent_id, g.id, g.scope = 'local', NOT g.sandboxed,
                                  nullif((g.capabilities ->> 'free_disk_bytes')::bigint, 0),
                                  jsonb_typeof(g.capabilities -> 'speech') = 'object')
       )::boolean AS agents_up,
       -- Fallback is for missing capacity, never for a failing file: every
       -- failed attempt of this job's family on this asset must have failed
       -- for a reason that isn't the file's. A failed transcription says
       -- nothing about an encode, nor the reverse.
       NOT EXISTS (
           SELECT 1 FROM attempts a JOIN jobs fj ON fj.id = a.job_id
           WHERE a.asset_id = j.asset_id AND a.status = 'failed'
             AND (fj.kind IN ('transcribe', 'chapters')) = (j.kind IN ('transcribe', 'chapters'))
             AND NOT (coalesce(a.error_code, '') = ANY (sqlc.arg(capacity_codes)::text[])
                      OR a.error_code LIKE 'wake\_%')
       )::boolean AS fallback_eligible,
       extract(epoch FROM now() - j.available_at)::double precision AS waited_seconds,
       -- A provider takes work once connected and verified by a setup test,
       -- if its machines' disk fits the job. Its image ships whisper and its
       -- setup test proves it, so it counts as able to transcribe.
       coalesce((SELECT c.verified_at IS NOT NULL
                        AND job_claimable_by(j.kind, j.asset_id, j.pinned_agent_id, NULL, false, false, c.disk_bytes, true)
                 FROM executor_configs c WHERE c.project_id = j.project_id AND c.kind = 'modal'), false)::boolean AS modal_up,
       coalesce((SELECT c.verified_at IS NOT NULL
                        AND job_claimable_by(j.kind, j.asset_id, j.pinned_agent_id, NULL, false, false, c.disk_bytes, true)
                 FROM executor_configs c WHERE c.project_id = j.project_id AND c.kind = 'runpod'), false)::boolean AS runpod_up,
       -- Room under the provider's limits and outside its backoff (D055).
       coalesce((SELECT provider_has_room(c.id, j.asset_id)
                 FROM executor_configs c WHERE c.project_id = j.project_id AND c.kind = 'modal'), false)::boolean AS modal_room,
       coalesce((SELECT provider_has_room(c.id, j.asset_id)
                 FROM executor_configs c WHERE c.project_id = j.project_id AND c.kind = 'runpod'), false)::boolean AS runpod_room,
       -- A setup test's sample runs where its test says, and nowhere else.
       coalesce((SELECT t.executor FROM setup_tests t WHERE t.asset_id = j.asset_id AND t.executor IS NOT NULL LIMIT 1), '')::text AS forced_executor,
       EXISTS (SELECT 1 FROM executor_configs c WHERE c.project_id = j.project_id
                 AND c.kind = (SELECT t.executor FROM setup_tests t WHERE t.asset_id = j.asset_id LIMIT 1))::boolean AS forced_configured,
       -- A woken machine keeps its job until it claims or its deadline ends
       -- the dispatch.
       EXISTS (SELECT 1 FROM dispatches d WHERE d.job_id = j.id AND d.status <> 'ended')::boolean AS has_live_dispatch
FROM jobs j JOIN projects p ON p.id = j.project_id
WHERE j.id = sqlc.arg(job_id);

-- name: SetJobRoute :exec
UPDATE jobs SET routed_to = $2, route_reason = $3 WHERE id = $1;

-- name: TouchBuiltinWorker :exec
-- Coalesced like agents: a write at most every 10 s, unless the reported
-- free disk or speech changed. A heartbeat reports neither (NULL).
UPDATE projects
SET builtin_seen_at = now(),
    builtin_free_disk_bytes = coalesce(sqlc.narg(free_disk_bytes)::bigint, builtin_free_disk_bytes),
    builtin_speech = coalesce(sqlc.narg(speech)::boolean, builtin_speech)
WHERE id = sqlc.arg(id)
  AND (builtin_seen_at IS NULL OR builtin_seen_at < now() - interval '10 seconds'
       OR builtin_free_disk_bytes IS DISTINCT FROM coalesce(sqlc.narg(free_disk_bytes)::bigint, builtin_free_disk_bytes)
       OR builtin_speech IS DISTINCT FROM coalesce(sqlc.narg(speech)::boolean, builtin_speech));

-- name: LockDispatchedJob :one
-- The one job a dispatch key may claim, while it is still queued and routed
-- to that provider.
SELECT * FROM jobs
WHERE id = sqlc.arg(id) AND status = 'queued' AND available_at <= now() AND routed_to = sqlc.arg(kind)::text
FOR UPDATE SKIP LOCKED;

-- name: LockWakingDispatch :one
SELECT * FROM dispatches WHERE id = $1 AND status = 'waking' AND claim_deadline > now() FOR UPDATE;

-- name: ClaimDispatch :exec
UPDATE dispatches SET status = 'claimed', attempt_id = $2, claimed_at = now() WHERE id = $1;

-- name: UnclaimDispatch :exec
-- An undelivered claim (Release) gives the dispatch its job back.
UPDATE dispatches SET status = 'waking', attempt_id = NULL, claimed_at = NULL WHERE id = $1 AND status = 'claimed';

-- name: EnqueueSourceUpload :execrows
-- In the commit of a local source's encode: if its push asked for
-- --upload-source, the PC holding the file uploads it next (D059).
INSERT INTO jobs (id, project_id, asset_id, kind, pinned_agent_id, routed_to, route_reason)
SELECT sqlc.arg(id), s.project_id, s.id, 'upload_source', g.agent_id, 'agents', 'pinned'
FROM assets s JOIN agent_sources g ON g.id = s.agent_source_id
WHERE s.id = sqlc.arg(asset_id) AND g.upload_source AND s.status <> 'deleted';

-- name: SetAttemptSourceMultipart :exec
UPDATE attempts SET source_multipart_id = $2, source_part_size = $3 WHERE id = $1;

-- name: GetAssetAgentSourceSize :one
SELECT g.size FROM assets s JOIN agent_sources g ON g.id = s.agent_source_id WHERE s.id = $1;

-- name: MoveSourceToStorage :exec
-- The PC's upload landed: the source is in storage now, and the asset is
-- no longer tied to that PC (D059).
UPDATE assets SET source_uri = $2, agent_source_id = NULL, source_kept = true WHERE id = $1;
