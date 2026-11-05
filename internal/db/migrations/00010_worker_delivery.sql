-- Cloudflare Worker delivery (docs/architecture.md, "Delivery", P2-T02).
-- A project serves through the built-in handler or the Worker. The Worker
-- reads route records and _keys.json from one bucket, delivery_storage_id;
-- assets on any other store are fetched from origin.

-- +goose Up
ALTER TABLE projects
    ADD COLUMN delivery text NOT NULL DEFAULT 'builtin' CHECK (delivery IN ('builtin', 'worker')),
    ADD COLUMN delivery_storage_id text REFERENCES storage_configs (id),
    ADD CONSTRAINT projects_worker_has_bucket CHECK (delivery = 'builtin' OR delivery_storage_id IS NOT NULL);

-- keys_sync rewrites _keys.json; its payload is empty.
ALTER TABLE outbox DROP CONSTRAINT outbox_kind_check;
ALTER TABLE outbox ADD CONSTRAINT outbox_kind_check CHECK (kind IN ('webhook', 'route_sync', 'keys_sync'));

-- +goose Down
DELETE FROM outbox WHERE kind = 'keys_sync';
ALTER TABLE outbox DROP CONSTRAINT outbox_kind_check;
ALTER TABLE outbox ADD CONSTRAINT outbox_kind_check CHECK (kind IN ('webhook', 'route_sync'));
ALTER TABLE projects DROP CONSTRAINT projects_worker_has_bucket,
    DROP COLUMN delivery_storage_id,
    DROP COLUMN delivery;
