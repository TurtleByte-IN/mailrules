-- +goose Up
-- An email moved back into the watched folder outside MailRules gets a new UID, and before
-- MAI-77 it was taken for new mail: a second row, decided again, so an email in Needs
-- review could wait there twice. When two rows of an account carry the same Message-ID and
-- both wait in Needs review, the older one leaves the queue as `skipped`, the state of an
-- email that is no longer where its row saw it. Nothing is deleted: the row keeps its
-- decision and history, and the email stays where it is.
UPDATE messages SET state = 'skipped', next_attempt_at = NULL
WHERE state = 'review' AND message_id IS NOT NULL AND message_id <> ''
  AND EXISTS (SELECT 1 FROM messages n
              WHERE n.account_id = messages.account_id AND n.message_id = messages.message_id
                AND n.id > messages.id AND n.state = 'review');

-- +goose Down
-- The rows stay as they are.
SELECT 1;
