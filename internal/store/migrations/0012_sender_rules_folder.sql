-- +goose Up
-- A sender rule can move the sender's mail to a folder of its own (MAI-162): verdict 'move'
-- with the folder named here. The name is used on every mailbox, as a rule's move action is,
-- and a mailbox that lacks the folder gets it on the first live move. NULL for the other verdicts.
ALTER TABLE sender_rules ADD COLUMN folder TEXT;

-- +goose Down
DELETE FROM sender_rules WHERE verdict = 'move';
ALTER TABLE sender_rules DROP COLUMN folder;
