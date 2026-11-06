-- Routing (P4-T01, D053; docs/architecture.md "Routing and fallback"): a
-- primary executor and an optional fallback per project, media jobs routed
-- while queued, and liveness for the built-in worker.

-- +goose Up
ALTER TABLE projects
    -- workers: the built-in worker and PCs compete (the behaviour before
    -- routing); agents: PCs only; or one executor.
    ADD COLUMN primary_executor text NOT NULL DEFAULT 'workers'
        CHECK (primary_executor IN ('workers', 'agents', 'builtin', 'modal', 'runpod')),
    ADD COLUMN fallback_executor text
        CHECK (fallback_executor IN ('builtin', 'modal', 'runpod')),
    -- Off unless set: a job waiting this long for a busy primary goes to the
    -- fallback.
    ADD COLUMN queue_age_fallback_minutes integer
        CHECK (queue_age_fallback_minutes BETWEEN 1 AND 1440),
    -- The built-in worker's last claim or heartbeat, and the free disk its
    -- last claim reported, so the router can tell whether it could run a job.
    ADD COLUMN builtin_seen_at timestamptz,
    ADD COLUMN builtin_free_disk_bytes bigint,
    -- A fallback is something the primary doesn't already include.
    ADD CONSTRAINT projects_fallback_differs CHECK (
        fallback_executor IS NULL OR (fallback_executor <> primary_executor
            AND NOT (fallback_executor = 'builtin' AND primary_executor = 'workers'))),
    ADD CONSTRAINT projects_queue_age_needs_fallback CHECK (
        queue_age_fallback_minutes IS NULL OR fallback_executor IS NOT NULL);

-- routed_to now names where a queued media job is routed, not who claimed
-- it (attempts.executor says that). Existing jobs keep today's behaviour.
UPDATE jobs SET routed_to = NULL, route_reason = NULL WHERE kind NOT IN ('transcode', 'reencode');
UPDATE jobs SET
    routed_to = CASE WHEN pinned_agent_id IS NOT NULL THEN 'agents' ELSE 'workers' END,
    route_reason = CASE WHEN pinned_agent_id IS NOT NULL THEN 'pinned' ELSE 'primary' END
WHERE kind IN ('transcode', 'reencode');
ALTER TABLE jobs
    ADD CONSTRAINT jobs_routed_to_check CHECK (routed_to IN ('workers', 'agents', 'builtin', 'modal', 'runpod')),
    ADD CONSTRAINT jobs_media_routed CHECK ((routed_to IS NOT NULL) = (kind IN ('transcode', 'reencode')));

-- Why the attempt's job was routed where it ran, copied at claim.
ALTER TABLE attempts ADD COLUMN route_reason text;

-- Whether a worker could take a job: the rules the claim applies, shared
-- with the router so "the primary is available" means "a worker that is
-- online could claim this". Pinned jobs go only to their agent; local-scoped
-- agents take only pinned jobs; an untrusted (unsandboxed) agent takes only
-- jobs pinned to it and the owner's own uploads (the built-in Dashboard
-- app's assets). A worker that reports its free disk takes only sources
-- that fit: three times an upload's size (source, encodes, packaged output),
-- twice a source already on the agent. Unknown sizes (imports) and
-- unreported disk pass.
-- +goose StatementBegin
CREATE FUNCTION job_claimable_by(
    job_asset_id text, job_pinned_agent_id text,
    worker_agent_id text, worker_local_only boolean, worker_untrusted boolean, worker_free_disk bigint
) RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT (coalesce(job_pinned_agent_id = worker_agent_id, false)
            OR (job_pinned_agent_id IS NULL AND NOT worker_local_only
                AND (NOT worker_untrusted OR EXISTS (
                    SELECT 1 FROM assets s JOIN apps p ON p.id = s.app_id
                    WHERE s.id = job_asset_id AND p.is_builtin))))
       AND (worker_free_disk IS NULL OR coalesce(
            (SELECT 3 * u.declared_size FROM uploads u WHERE u.asset_id = job_asset_id ORDER BY u.created_at DESC LIMIT 1),
            (SELECT 2 * g.size FROM assets s JOIN agent_sources g ON g.id = s.agent_source_id WHERE s.id = job_asset_id),
            0) <= worker_free_disk)
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION job_claimable_by(text, text, text, boolean, boolean, bigint);
ALTER TABLE attempts DROP COLUMN route_reason;
ALTER TABLE jobs DROP CONSTRAINT jobs_media_routed, DROP CONSTRAINT jobs_routed_to_check;
UPDATE jobs SET routed_to = NULL, route_reason = NULL WHERE status = 'queued';
ALTER TABLE projects
    DROP CONSTRAINT projects_queue_age_needs_fallback,
    DROP CONSTRAINT projects_fallback_differs,
    DROP COLUMN builtin_free_disk_bytes,
    DROP COLUMN builtin_seen_at,
    DROP COLUMN queue_age_fallback_minutes,
    DROP COLUMN fallback_executor,
    DROP COLUMN primary_executor;
