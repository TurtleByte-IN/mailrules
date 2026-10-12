-- +goose Up
-- Emails the person moved out of the folder MailRules filed them in (MAI-176), for a build
-- whose module asks to know (ext.Moves). Each filing action leaves at most one row: the
-- watcher writes it the first time it finds the email gone from where the action left it.
-- chosen_folder is where the email was found ('' = in no folder), and person says whether
-- that was the person filing it somewhere that counts (the Inbox or an ordinary folder)
-- rather than deleting, archiving or MailRules itself. Only those are handed to the module:
-- handed_at is set at once for the others. The rows go after 30 days (store.MoveDays).
CREATE TABLE moves (
  id            INTEGER PRIMARY KEY,
  tenant_id     INTEGER NOT NULL,
  account_id    INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  message_id    INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  action_id     INTEGER NOT NULL UNIQUE,  -- the filing action
  from_addr     TEXT NOT NULL,
  rule_id       INTEGER REFERENCES rules(id) ON DELETE SET NULL,
  rule_name     TEXT NOT NULL DEFAULT '',
  filed_folder  TEXT NOT NULL,
  chosen_folder TEXT NOT NULL,
  inbox         INTEGER NOT NULL DEFAULT 0, -- chosen_folder is the mailbox's watched folder
  person        INTEGER NOT NULL DEFAULT 0,
  filed_at      INTEGER NOT NULL,
  created_at    INTEGER NOT NULL,           -- when the watcher found it moved
  attempts      INTEGER NOT NULL DEFAULT 0,
  next_at       INTEGER NOT NULL,
  handed_at     INTEGER
);
CREATE INDEX moves_waiting ON moves(next_at) WHERE handed_at IS NULL;
CREATE INDEX moves_sender ON moves(tenant_id, from_addr);

-- The questions Activity asks once per sender and destination, when the module asks
-- (ext.Questions.Ask): "You moved 2 emails from this sender to Inbox. Always keep them in
-- Inbox?" target is '' for the Inbox, else the folder. An answered question (yes or no) is
-- kept, so the same sender and destination is never asked about again.
CREATE TABLE move_questions (
  id          INTEGER PRIMARY KEY,
  tenant_id   INTEGER NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  account_id  INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE, -- the mailbox of the latest move
  move_id     INTEGER NOT NULL,             -- the latest move asked about
  from_addr   TEXT NOT NULL,
  target      TEXT NOT NULL,
  emails      INTEGER NOT NULL,
  status      TEXT NOT NULL DEFAULT 'open', -- open | yes | no
  asked_at    INTEGER NOT NULL,
  answered_at INTEGER,
  answered_by INTEGER,
  UNIQUE (tenant_id, from_addr, target)
);

-- The watcher reads one mailbox's recent actions on every sweep.
CREATE INDEX actions_account_created ON actions(account_id, created_at);

-- +goose Down
DROP INDEX actions_account_created;
DROP TABLE move_questions;
DROP TABLE moves;
