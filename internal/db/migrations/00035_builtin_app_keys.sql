-- The built-in (Dashboard) app takes no API keys, and an unsandboxed agent
-- takes no URL import (D123). Its "owner jobs" were the Dashboard app's,
-- which API keys and imports from anywhere could create (security review,
-- finding 3). Keys the app already had are revoked; the routes that issue
-- keys now refuse it.

-- +goose Up
UPDATE api_keys k SET revoked_at = now()
FROM apps a WHERE a.id = k.app_id AND a.is_builtin AND k.revoked_at IS NULL;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION job_claimable_by(
    job_kind text, job_asset_id text, job_pinned_agent_id text,
    worker_agent_id text, worker_local_only boolean, worker_untrusted boolean, worker_free_disk bigint,
    worker_speech boolean
) RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT (coalesce(job_pinned_agent_id = worker_agent_id, false)
            OR (job_pinned_agent_id IS NULL AND NOT worker_local_only
                AND (NOT worker_untrusted OR (EXISTS (
                    SELECT 1 FROM assets s JOIN apps p ON p.id = s.app_id
                    WHERE s.id = job_asset_id AND p.is_builtin)
                  AND NOT EXISTS (
                    SELECT 1 FROM jobs i WHERE i.asset_id = job_asset_id AND i.kind = 'import')))))
       AND (job_kind <> 'transcribe' OR worker_speech)
       AND (worker_free_disk IS NULL OR CASE WHEN job_kind = 'transcribe' THEN
                1073741824 + coalesce((SELECT ceil(s.duration_seconds * 65536)::bigint FROM assets s WHERE s.id = job_asset_id), 0)
            ELSE coalesce(
                (SELECT 3 * u.declared_size FROM uploads u WHERE u.asset_id = job_asset_id ORDER BY u.created_at DESC LIMIT 1),
                (SELECT 2 * g.size FROM assets s JOIN agent_sources g ON g.id = s.agent_source_id WHERE s.id = job_asset_id),
                0) END <= worker_free_disk)
$$;
-- +goose StatementEnd

-- +goose Down
-- The revoked keys stay revoked: un-revoking a key is never safe.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION job_claimable_by(
    job_kind text, job_asset_id text, job_pinned_agent_id text,
    worker_agent_id text, worker_local_only boolean, worker_untrusted boolean, worker_free_disk bigint,
    worker_speech boolean
) RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT (coalesce(job_pinned_agent_id = worker_agent_id, false)
            OR (job_pinned_agent_id IS NULL AND NOT worker_local_only
                AND (NOT worker_untrusted OR EXISTS (
                    SELECT 1 FROM assets s JOIN apps p ON p.id = s.app_id
                    WHERE s.id = job_asset_id AND p.is_builtin))))
       AND (job_kind <> 'transcribe' OR worker_speech)
       AND (worker_free_disk IS NULL OR CASE WHEN job_kind = 'transcribe' THEN
                1073741824 + coalesce((SELECT ceil(s.duration_seconds * 65536)::bigint FROM assets s WHERE s.id = job_asset_id), 0)
            ELSE coalesce(
                (SELECT 3 * u.declared_size FROM uploads u WHERE u.asset_id = job_asset_id ORDER BY u.created_at DESC LIMIT 1),
                (SELECT 2 * g.size FROM assets s JOIN agent_sources g ON g.id = s.agent_source_id WHERE s.id = job_asset_id),
                0) END <= worker_free_disk)
$$;
-- +goose StatementEnd
