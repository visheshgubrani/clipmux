-- Uploads and encoded files live apart (D117). An asset's source,
-- uploads/{asset}/…, stays on storage_id, where Storage sent it; its
-- outputs and tracks, media/{asset}/…, live on media_storage_id: the
-- Cloudflare delivery bucket when the Worker delivers, otherwise the same
-- store as the source. Both are fixed at creation and change only by a move.

-- +goose Up
ALTER TABLE assets ADD COLUMN media_storage_id text REFERENCES storage_configs (id);
UPDATE assets SET media_storage_id = storage_id;
ALTER TABLE assets ALTER COLUMN media_storage_id SET NOT NULL;

-- An insert that names no media store keeps the encoded files with the
-- source, as built-in delivery does. The create queries name it.
-- +goose StatementBegin
CREATE FUNCTION assets_media_with_source() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.media_storage_id := coalesce(NEW.media_storage_id, NEW.storage_id);
    RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER assets_media_with_source BEFORE INSERT ON assets
    FOR EACH ROW EXECUTE FUNCTION assets_media_with_source();

-- Where a new asset's encoded files go: the delivery bucket while the
-- Worker delivers, otherwise the default store, with the uploads.
-- +goose StatementBegin
CREATE FUNCTION media_target(project text) RETURNS text LANGUAGE sql STABLE AS $$
    SELECT CASE WHEN p.delivery = 'worker' THEN p.delivery_storage_id
                ELSE (SELECT c.id FROM storage_configs c WHERE c.project_id = p.id AND c.is_default) END
    FROM projects p WHERE p.id = project
$$;
-- +goose StatementEnd

-- A move carries each part to its own store: the source to the uploads
-- default, the encoded files to the media target. Left NULL, as on moves
-- queued before this, the encoded files move with the source.
ALTER TABLE jobs
    ADD COLUMN source_media_storage_id text REFERENCES storage_configs (id),
    ADD COLUMN target_media_storage_id text REFERENCES storage_configs (id);

-- +goose Down
-- Going back needs every asset whole on one store again: move them first.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM assets WHERE media_storage_id <> storage_id) THEN
        RAISE EXCEPTION 'assets keep their encoded files apart from their source; move them back first';
    END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE jobs DROP COLUMN target_media_storage_id, DROP COLUMN source_media_storage_id;
DROP FUNCTION media_target(text);
DROP TRIGGER assets_media_with_source ON assets;
DROP FUNCTION assets_media_with_source();
ALTER TABLE assets DROP COLUMN media_storage_id;
