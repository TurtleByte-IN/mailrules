-- +goose Up
-- Removing a mailbox no longer deletes the rules made for it (MAI-132): such a rule is kept,
-- switched off and marked here, and cannot be switched on again until it is given a mailbox,
-- or All mailboxes, or new conditions, which clears the mark.
ALTER TABLE rules ADD COLUMN mailbox_removed INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE rules DROP COLUMN mailbox_removed;
