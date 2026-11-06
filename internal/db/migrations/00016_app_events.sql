-- clipmux listen reads an app's webhook events in order (P3-T07).

-- +goose Up
CREATE INDEX outbox_app_webhooks ON outbox (app_id, created_at, id) WHERE kind = 'webhook';

-- +goose Down
DROP INDEX outbox_app_webhooks;
