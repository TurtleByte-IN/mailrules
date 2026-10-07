-- +goose Up
-- The Activity feed is grouped by day and ordered by when MailRules acted on an email, the
-- time its row shows (MAI-74): acted_at is the time of the email's latest decision, or when
-- MailRules first saw it while none is made yet. A cleanup that decides old mail today puts
-- it under today. The index serves the feed's order and its cursor.
ALTER TABLE messages ADD COLUMN acted_at INTEGER NOT NULL DEFAULT 0;
UPDATE messages SET acted_at = COALESCE(
  (SELECT d.created_at FROM decisions d WHERE d.id = (SELECT MAX(id) FROM decisions WHERE message_id = messages.id)),
  created_at);
CREATE INDEX messages_acted ON messages(acted_at, id);

-- +goose Down
DROP INDEX messages_acted;
ALTER TABLE messages DROP COLUMN acted_at;
