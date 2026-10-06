-- +goose Up
-- A cleanup Sort skips emails that are no longer where its check found them (MAI-44); the
-- batch records how many so the screen can say "N skipped: no longer in the folder".
ALTER TABLE batches ADD COLUMN skipped INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE batches DROP COLUMN skipped;
