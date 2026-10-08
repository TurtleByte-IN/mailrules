-- +goose Up
-- A manual run over several mailboxes (MAI-43) is one batch per mailbox, started together. They
-- share a run_id, the id of the run's first batch, so the screen can show them as one run with a
-- line per mailbox and undo them together. NULL on every other batch, a single-mailbox cleanup
-- included.
ALTER TABLE batches ADD COLUMN run_id INTEGER;

-- +goose Down
ALTER TABLE batches DROP COLUMN run_id;
