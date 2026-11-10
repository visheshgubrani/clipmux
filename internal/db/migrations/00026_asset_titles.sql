-- Asset titles (P6-T12, D099): an optional name for people, up to 200
-- characters, that the library searches with the passthrough. It is not
-- unique and carries no meaning for delivery.

-- +goose Up
ALTER TABLE assets ADD COLUMN title text CHECK (char_length(title) BETWEEN 1 AND 200);

-- +goose Down
ALTER TABLE assets DROP COLUMN title;
