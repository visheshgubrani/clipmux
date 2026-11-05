-- Upload activity (docs/architecture.md, "Abandoned uploads"): a newly
-- stored part counts as activity, so the sweeper remembers how many parts it
-- last saw.

-- +goose Up
ALTER TABLE uploads ADD COLUMN parts_seen integer NOT NULL DEFAULT 0 CHECK (parts_seen >= 0);

-- +goose Down
ALTER TABLE uploads DROP COLUMN parts_seen;
