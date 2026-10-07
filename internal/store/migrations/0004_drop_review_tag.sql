-- +goose Up
-- An email waiting in Needs review is no longer tagged with the IMAP keyword $MailRulesReview
-- (MAI-62): below its threshold nothing touches the mailbox. The rows that recorded the tag
-- are dropped, so no list, count or undo has to tell them apart from a rule's actions. The
-- keyword already on messages in the mailbox is left there.
DELETE FROM actions WHERE kind = 'review';

-- +goose Down
-- The dropped rows are not brought back.
SELECT 1;
