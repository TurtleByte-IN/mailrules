-- +goose Up
-- One-click unsubscribe (MAI-105). An email whose headers offer an RFC 8058 one-click link
-- (an https List-Unsubscribe and List-Unsubscribe-Post: List-Unsubscribe=One-Click) keeps
-- that link, so Senders can unsubscribe from the newest one; NULL when it offers none. The
-- link goes with the row: the retention job, a forgotten person and a removed mailbox take it.
ALTER TABLE messages ADD COLUMN unsubscribe_url TEXT;

-- Senders a mailbox was unsubscribed from, by address: done once per mailbox, so the row
-- shows it after a reload. host is the site that took it; the link itself is not kept here.
CREATE TABLE unsubscribes (
  account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  address    TEXT NOT NULL,
  host       TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (account_id, address)
);

-- +goose Down
DROP TABLE unsubscribes;
ALTER TABLE messages DROP COLUMN unsubscribe_url;
