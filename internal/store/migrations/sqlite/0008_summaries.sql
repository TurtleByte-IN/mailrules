-- +goose Up
-- The summary email: one row per scheduled summary, keyed by the time it was due, so a
-- restart or a second check of the schedule finds the row and never sends it twice. A row is
-- `sending` while one attempt runs (a stop in the middle leaves it so, and it is not tried
-- again: it may have gone out), `sent` once the mail server took it, and `failed` after an
-- attempt that did not get through; a failed one is tried again a few times. A test summary
-- sent from Settings is not recorded here.
CREATE TABLE summaries (
  id              INTEGER PRIMARY KEY,
  user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  due_at          INTEGER NOT NULL,        -- the scheduled time it is for
  status          TEXT NOT NULL,           -- sending | sent | failed
  attempts        INTEGER NOT NULL DEFAULT 0,
  last_attempt_at INTEGER NOT NULL,
  period_start    INTEGER,                 -- what a sent one covered; the next one starts at period_end
  period_end      INTEGER,
  sent_at         INTEGER,
  UNIQUE (user_id, due_at)
);

-- +goose Down
DROP TABLE summaries;
