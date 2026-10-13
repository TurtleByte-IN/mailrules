-- +goose Up
-- Draft replies through a module (MAI-233). A rule may have a reply drafted each time it acts
-- on an email live: a JSON object {"instruction", "read_previous"}; NULL = none.
ALTER TABLE rules ADD COLUMN draft TEXT;

-- The outbox: one job for every email such a rule acted on live, written in the transaction
-- that records the rule's last action on it, so a job exists exactly when the actions do and
-- a restart loses none. The drafting worker hands each to the module and saves what it
-- writes in the mailbox's Drafts folder, then deletes the job; the action it records (kind
-- draft) is what remains. draft_key makes the draft's Message-ID, the same on every attempt,
-- so a draft saved before a crash is found again rather than saved twice. The instruction is
-- the rule's as it stood when it acted.
CREATE TABLE draft_jobs (
  id            INTEGER PRIMARY KEY,
  tenant_id     INTEGER NOT NULL,
  account_id    INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  message_id    INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  decision_id   INTEGER NOT NULL,
  batch_id      INTEGER NOT NULL,
  rule_id       INTEGER REFERENCES rules(id) ON DELETE SET NULL,
  rule_name     TEXT NOT NULL,
  instruction   TEXT NOT NULL DEFAULT '',
  read_previous BOOLEAN NOT NULL DEFAULT FALSE,
  draft_key     TEXT NOT NULL,
  created_at    INTEGER NOT NULL,             -- when the rule acted
  attempts      INTEGER NOT NULL DEFAULT 0,   -- hand-overs that failed so far
  next_at       INTEGER NOT NULL              -- when the worker next tries it
);
CREATE INDEX draft_jobs_next ON draft_jobs(next_at);
CREATE INDEX draft_jobs_message ON draft_jobs(message_id);

-- +goose Down
DROP TABLE draft_jobs;
ALTER TABLE rules DROP COLUMN draft;
