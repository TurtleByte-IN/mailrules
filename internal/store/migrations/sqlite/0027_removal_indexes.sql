-- +goose Up
-- Deleting a message makes the database find the corrections and moves that reference it,
-- and deleting a decision the actions that reference it. Without an index on the
-- referencing column every one of those lookups read the whole table, so removing a mailbox
-- (or the retention job deleting old messages) took time growing with the square of its
-- messages, all of it holding the write lock: 10,000 messages kept it for 5.7 s, past the
-- 5 s every other writer waits, and 50,000 for over 3 minutes (MAI-214).
CREATE INDEX actions_decision ON actions(decision_id);
CREATE INDEX corrections_message ON corrections(message_id);
CREATE INDEX moves_message ON moves(message_id);

-- +goose Down
DROP INDEX moves_message;
DROP INDEX corrections_message;
DROP INDEX actions_decision;
