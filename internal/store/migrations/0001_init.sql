-- +goose Up
-- SQLite. Tenants arrive in 0011 (tenant_id on users, accounts, rules, sender_rules,
-- settings, usage_daily and batches); Postgres with row-level security is MAI-150.
-- Timestamps are unix seconds everywhere.
CREATE TABLE users (
  id            INTEGER PRIMARY KEY,
  email         TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,              -- argon2id encoded string
  created_at    INTEGER NOT NULL
);

CREATE TABLE sessions (
  token_hash TEXT PRIMARY KEY,              -- hex SHA-256 of the cookie token
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL               -- slides forward on use
);
CREATE INDEX sessions_expiry ON sessions(expires_at);

CREATE TABLE accounts (
  id            INTEGER PRIMARY KEY,
  user_id       INTEGER NOT NULL REFERENCES users(id),
  label         TEXT NOT NULL,
  preset        TEXT NOT NULL,              -- icloud | fastmail | yahoo | zoho | generic
  host          TEXT NOT NULL,
  port          INTEGER NOT NULL DEFAULT 993,
  tls_mode      TEXT NOT NULL DEFAULT 'implicit',  -- implicit | starttls
  username      TEXT NOT NULL,
  secret_enc    BLOB NOT NULL,              -- envelope-encrypted app password or token
  dek_enc       BLOB NOT NULL,              -- per-account data key, wrapped by master key
  watch_folder  TEXT NOT NULL DEFAULT 'INBOX',
  status        TEXT NOT NULL DEFAULT 'new', -- new | live | reconnecting | auth_failed | error | paused
  last_error    TEXT,
  last_event_at INTEGER,
  capabilities  TEXT,                       -- JSON array from CAPABILITY
  created_at    INTEGER NOT NULL
);

CREATE TABLE folders (
  account_id   INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  name         TEXT NOT NULL,
  delimiter    TEXT,
  special_use  TEXT,                        -- \Junk \Trash \Archive \Sent \Drafts or NULL
  uidvalidity  INTEGER,
  last_uid     INTEGER NOT NULL DEFAULT 0,  -- highest UID processed (watch folder)
  PRIMARY KEY (account_id, name)
);

CREATE TABLE rules (
  id             INTEGER PRIMARY KEY,
  user_id        INTEGER NOT NULL REFERENCES users(id),
  account_id     INTEGER REFERENCES accounts(id) ON DELETE CASCADE,  -- NULL = all accounts
  name           TEXT NOT NULL,
  said           TEXT,                      -- user's original wording
  intent         TEXT,                      -- optimized plain-English intent; NULL = condition-only
  conditions     TEXT NOT NULL DEFAULT '{}',-- JSON condition tree
  exceptions     TEXT NOT NULL DEFAULT '{}',-- JSON condition tree ("unless")
  actions        TEXT NOT NULL,             -- JSON array of actions
  priority       INTEGER NOT NULL,          -- lower runs first
  stack          INTEGER NOT NULL DEFAULT 0,-- 1 = also apply after a match
  model          TEXT,                      -- per-rule decider override
  min_confidence REAL,
  enabled        INTEGER NOT NULL DEFAULT 1,
  version        INTEGER NOT NULL DEFAULT 1,
  created_at     INTEGER NOT NULL,
  updated_at     INTEGER NOT NULL
);

CREATE TABLE messages (
  id             INTEGER PRIMARY KEY,
  account_id     INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  folder         TEXT NOT NULL,
  uidvalidity    INTEGER NOT NULL,
  uid            INTEGER NOT NULL,
  message_id     TEXT,                      -- RFC 5322 Message-ID
  from_addr      TEXT, from_domain TEXT, to_addrs TEXT, subject TEXT,
  from_name      TEXT,                      -- the From display name, shown on the Senders screen
  snippet        TEXT,                      -- first 200 chars, purged after retention
  received_at    INTEGER,
  list_id        TEXT,
  has_attachment INTEGER NOT NULL DEFAULT 0,
  size           INTEGER,
  signals        TEXT,                      -- JSON: bulk, noreply, dmarc, replied_before, list_unsubscribe ...
  state          TEXT NOT NULL DEFAULT 'new', -- new | decided | acted | review | skipped | error
  attempts       INTEGER NOT NULL DEFAULT 0, -- retries made after a failure
  next_attempt_at INTEGER,                  -- when the retry job runs it again; NULL = not waiting
  cur_folder     TEXT,                      -- where the executor last left it (a move, or an undo);
  cur_uidvalidity INTEGER,                  --   NULL = still where it arrived. Mail showing up at
  cur_uid        INTEGER,                   --   this place in the watch folder is not new mail
  created_at     INTEGER NOT NULL,
  UNIQUE (account_id, folder, uidvalidity, uid)
);
CREATE INDEX messages_msgid ON messages(account_id, message_id);
CREATE INDEX messages_state ON messages(state, created_at);
CREATE INDEX messages_current ON messages(account_id, cur_folder, cur_uid);

CREATE TABLE decisions (
  id          INTEGER PRIMARY KEY,
  message_id  INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  stage       TEXT NOT NULL,                -- sender | condition | decider | fallback | none
  rule_id     INTEGER REFERENCES rules(id) ON DELETE SET NULL,
  rule_name   TEXT,                         -- the rule's name when decided; survives deleting the rule
  rule_version INTEGER,
  confidence  REAL,
  reason      TEXT,
  model       TEXT,
  tokens_in   INTEGER, tokens_out INTEGER,
  cost_usd    REAL, latency_ms INTEGER,
  probabilities TEXT,                       -- JSON {"<rule id>": p, "0": p for none}: what the decision model gave each candidate; NULL when it gives none
  created_at  INTEGER NOT NULL
);
CREATE INDEX decisions_message ON decisions(message_id);  -- "the latest decision of a message" is asked for every feed row

CREATE TABLE batches (
  id         INTEGER PRIMARY KEY,
  kind       TEXT NOT NULL,                 -- live | cleanup | review | correction | undo
  status     TEXT NOT NULL,                 -- running | done | failed | undone
  total      INTEGER, done INTEGER NOT NULL DEFAULT 0,
  account_id INTEGER REFERENCES accounts(id) ON DELETE SET NULL,  -- cleanup: the mailbox it sorted
  folder     TEXT,                          -- cleanup: the folder
  since      INTEGER,                       -- cleanup: only mail received from this time on; NULL = all of it
  tokens     INTEGER NOT NULL DEFAULT 0,    -- cleanup: model tokens used so far, in and out
  cost_usd   REAL NOT NULL DEFAULT 0,       -- cleanup: model cost so far
  created_at INTEGER NOT NULL
);

CREATE TABLE actions (
  id          INTEGER PRIMARY KEY,
  decision_id INTEGER REFERENCES decisions(id),
  batch_id    INTEGER REFERENCES batches(id),
  message_id  INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  account_id  INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  kind        TEXT NOT NULL,                -- move | trash | archive | junk | flag | unflag | read | unread | keep | review
  params      TEXT,                         -- JSON, e.g. {"folder":"Food"}
  before      TEXT NOT NULL,                -- JSON {folder, uidvalidity, uid, flags[]}
  after       TEXT,                         -- JSON {folder, uidvalidity, uid, flags[]}
  status      TEXT NOT NULL,                -- done | dry_run | failed | undone
  error       TEXT,
  created_at  INTEGER NOT NULL,
  undone_at   INTEGER
);
CREATE INDEX actions_batch ON actions(batch_id);
CREATE INDEX actions_message ON actions(message_id);

CREATE TABLE corrections (
  id            INTEGER PRIMARY KEY,
  message_id    INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  wrong_rule_id INTEGER REFERENCES rules(id) ON DELETE SET NULL,
  right_rule_id INTEGER REFERENCES rules(id) ON DELETE SET NULL,  -- NULL = keep in inbox
  example       TEXT NOT NULL,              -- JSON summary used as a few-shot example
  kind          TEXT NOT NULL DEFAULT 'correction',  -- correction | review: fixed from the feed, or answered in Needs review
  created_at    INTEGER NOT NULL
);

CREATE TABLE sender_rules (
  id         INTEGER PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id),
  match_type TEXT NOT NULL,                 -- address | domain
  value      TEXT NOT NULL,
  rule_id    INTEGER REFERENCES rules(id) ON DELETE CASCADE,  -- route to this rule's actions
  verdict    TEXT NOT NULL,                 -- route | keep | block
  source     TEXT NOT NULL,                 -- user | learned
  hits       INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  UNIQUE (user_id, match_type, value)
);

CREATE TABLE contacts (                     -- addresses the user has sent mail to
  account_id   INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  address      TEXT NOT NULL,
  last_sent_at INTEGER,
  PRIMARY KEY (account_id, address)
);

CREATE TABLE usage_daily (
  day       TEXT NOT NULL,                  -- YYYY-MM-DD UTC
  provider  TEXT NOT NULL, model TEXT NOT NULL,
  purpose   TEXT NOT NULL,                  -- decide | escalate | compose | test
  calls     INTEGER NOT NULL, tokens_in INTEGER NOT NULL, tokens_out INTEGER NOT NULL,
  cost_usd  REAL NOT NULL,
  PRIMARY KEY (day, provider, model, purpose)
);

CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);  -- JSON values
