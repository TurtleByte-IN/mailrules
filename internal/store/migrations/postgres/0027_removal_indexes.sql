-- +goose Up
-- The indexes sqlite/0027_removal_indexes.sql adds, for the same reason (MAI-214): each
-- foreign key check or cascade from a deleted message or decision otherwise scans the whole
-- referencing table, so removing a mailbox took time growing with the square of its mail.
CREATE INDEX actions_decision ON actions(decision_id);
CREATE INDEX corrections_message ON corrections(message_id);
CREATE INDEX moves_message ON moves(message_id);

-- +goose Down
DROP INDEX moves_message;
DROP INDEX corrections_message;
DROP INDEX actions_decision;
