-- The speech API (P5-T14b, D075): a project may send transcriptions to an
-- OpenAI-compatible speech API instead of, or after, its own workers. serve
-- runs those itself, as the executor class speech_api.

-- +goose Up
-- local: whisper on the project's executors; api: every transcription to
-- the connected API; local_then_api: the API as transcriptions' fallback.
ALTER TABLE projects ADD COLUMN speech_mode text NOT NULL DEFAULT 'local'
    CHECK (speech_mode IN ('local', 'api', 'local_then_api'));
ALTER TABLE jobs DROP CONSTRAINT jobs_routed_to_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_routed_to_check
    CHECK (routed_to IN ('workers', 'agents', 'builtin', 'modal', 'runpod', 'speech_api'));
-- Only a transcription can go to the speech API.
ALTER TABLE jobs ADD CONSTRAINT jobs_speech_api_transcribes CHECK (routed_to IS DISTINCT FROM 'speech_api' OR kind = 'transcribe');
ALTER TABLE attempts DROP CONSTRAINT attempts_executor_check;
ALTER TABLE attempts ADD CONSTRAINT attempts_executor_check
    CHECK (executor IN ('builtin', 'agent', 'modal', 'runpod', 'speech_api'));

-- +goose Down
DELETE FROM attempts WHERE executor = 'speech_api';
UPDATE jobs j SET routed_to = p.primary_executor, route_reason = 'primary'
FROM projects p WHERE p.id = j.project_id AND j.routed_to = 'speech_api';
ALTER TABLE attempts DROP CONSTRAINT attempts_executor_check;
ALTER TABLE attempts ADD CONSTRAINT attempts_executor_check CHECK (executor IN ('builtin', 'agent', 'modal', 'runpod'));
ALTER TABLE jobs DROP CONSTRAINT jobs_speech_api_transcribes;
ALTER TABLE jobs DROP CONSTRAINT jobs_routed_to_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_routed_to_check CHECK (routed_to IN ('workers', 'agents', 'builtin', 'modal', 'runpod'));
ALTER TABLE projects DROP COLUMN speech_mode;
