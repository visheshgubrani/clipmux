-- Import from URL (docs/architecture.md, "Integration API", P2-T06). serve
-- fetches the URL itself, through a client that reaches only public
-- addresses, so workers never claim import jobs.

-- +goose Up
ALTER TABLE jobs
    ADD COLUMN import_url text,
    ADD CONSTRAINT jobs_import_has_url CHECK (kind <> 'import' OR import_url IS NOT NULL);

-- +goose Down
ALTER TABLE jobs DROP CONSTRAINT jobs_import_has_url, DROP COLUMN import_url;
