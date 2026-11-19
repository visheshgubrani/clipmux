-- What a failed attempt's worker said went wrong, beside its error_code:
-- the tool's first error lines, at most 1000 characters, with any URL's
-- query string removed. The dashboard shows it on a failed video (bug hunt
-- F18); before this the message was dropped and only the code was kept.

-- +goose Up
ALTER TABLE attempts ADD COLUMN error_message text
    CONSTRAINT attempts_error_message_length CHECK (char_length(error_message) <= 1000);

-- +goose Down
ALTER TABLE attempts DROP COLUMN error_message;
