-- +goose Up
-- The mailboxes a module connected whose secret the daemon let go of (MAI-180): removed, or
-- their owner forgotten. Each row is written in the transaction that removes the mailbox or
-- deletes its secret, so a restart loses none, and is handed to the module that connected
-- the mailbox (ext.Module.MailboxRemoved) until it takes it: then the row, and the copy of
-- the secret it holds, are deleted. secret_enc and dek_enc are the mailbox's own, copied as
-- sealed (under account_id, as in accounts). A row the module never takes is deleted after
-- 30 days. No foreign key: the mailbox, its user and its tenant may all be gone. The id is
-- what the module tells hand-overs apart by, so AUTOINCREMENT: never reused once deleted.
CREATE TABLE removed_mailboxes (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id INTEGER NOT NULL,
  preset     TEXT NOT NULL,
  oauth      INTEGER NOT NULL,
  username   TEXT NOT NULL,
  secret_enc BLOB NOT NULL,
  dek_enc    BLOB NOT NULL,
  created_at INTEGER NOT NULL,
  attempts   INTEGER NOT NULL DEFAULT 0,
  next_at    INTEGER NOT NULL          -- when the worker next hands it over
);
CREATE INDEX removed_mailboxes_next ON removed_mailboxes(next_at);

-- +goose Down
DROP TABLE removed_mailboxes;
