-- +goose Up
-- Tenants (MAI-133). A tenant owns rules, sender rules, settings and the usage ledger; each
-- mailbox (accounts row) belongs to its owner (accounts.user_id, who added it) in that
-- owner's tenant, and is private to the owner until `shared` is set, when everyone in the
-- tenant sees it. Self-host is tenant 1, made here, and every existing row moves into it.
-- The tenant_id columns added with ALTER TABLE carry no REFERENCES: SQLite refuses a
-- foreign key with a non-NULL default on ADD COLUMN while foreign keys are on, so the store
-- sets them from the owning user or account and nothing else writes them. Isolation rests on
-- the store's queries (store.Viewer); Postgres with row-level security is MAI-150.
CREATE TABLE tenants (
  id          INTEGER PRIMARY KEY,
  provider    TEXT,                       -- the sign-in service that made it; NULL = self-host
  external_id TEXT,                       -- that service's organisation id
  created_at  INTEGER NOT NULL,
  UNIQUE (provider, external_id)
);
INSERT INTO tenants (id, provider, external_id, created_at) VALUES (1, NULL, NULL, CAST(strftime('%s', 'now') AS INTEGER));

ALTER TABLE users ADD COLUMN tenant_id INTEGER NOT NULL DEFAULT 1;
CREATE INDEX users_tenant ON users(tenant_id);

-- A person signed in through a sign-in service: the user it maps to. Such a user has an
-- empty password_hash, which password sign-in and password change refuse.
CREATE TABLE identities (
  provider TEXT NOT NULL,
  subject  TEXT NOT NULL,                  -- the service's stable user id
  user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  PRIMARY KEY (provider, subject)
);

ALTER TABLE accounts ADD COLUMN tenant_id INTEGER NOT NULL DEFAULT 1;
ALTER TABLE accounts ADD COLUMN shared INTEGER NOT NULL DEFAULT 0;  -- 1 = everyone in the tenant sees it
CREATE INDEX accounts_tenant ON accounts(tenant_id);

ALTER TABLE rules ADD COLUMN tenant_id INTEGER NOT NULL DEFAULT 1;  -- user_id stays as the author
CREATE INDEX rules_tenant ON rules(tenant_id, priority);

ALTER TABLE batches ADD COLUMN tenant_id INTEGER NOT NULL DEFAULT 1;
CREATE INDEX batches_tenant ON batches(tenant_id, kind, created_at);

CREATE TABLE sender_rules_new (
  id         INTEGER PRIMARY KEY,
  tenant_id  INTEGER NOT NULL,
  user_id    INTEGER NOT NULL REFERENCES users(id),  -- who made it
  match_type TEXT NOT NULL,
  value      TEXT NOT NULL,
  rule_id    INTEGER REFERENCES rules(id) ON DELETE CASCADE,
  verdict    TEXT NOT NULL,
  source     TEXT NOT NULL,
  hits       INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  UNIQUE (tenant_id, match_type, value)
);
INSERT INTO sender_rules_new (id, tenant_id, user_id, match_type, value, rule_id, verdict, source, hits, created_at)
  SELECT id, 1, user_id, match_type, value, rule_id, verdict, source, hits, created_at FROM sender_rules;
DROP TABLE sender_rules;
ALTER TABLE sender_rules_new RENAME TO sender_rules;

CREATE TABLE settings_new (
  tenant_id INTEGER NOT NULL,
  key       TEXT NOT NULL,
  value     TEXT NOT NULL,                 -- JSON
  PRIMARY KEY (tenant_id, key)
);
INSERT INTO settings_new (tenant_id, key, value) SELECT 1, key, value FROM settings;
DROP TABLE settings;
ALTER TABLE settings_new RENAME TO settings;

CREATE TABLE usage_daily_new (
  tenant_id  INTEGER NOT NULL,
  operator   INTEGER NOT NULL DEFAULT 0,   -- 1 = made with a key the operator holds (cloud), for billing
  day        TEXT NOT NULL,
  provider   TEXT NOT NULL, model TEXT NOT NULL,
  purpose    TEXT NOT NULL,
  calls      INTEGER NOT NULL, tokens_in INTEGER NOT NULL, tokens_out INTEGER NOT NULL,
  cost_usd   REAL NOT NULL,
  PRIMARY KEY (tenant_id, operator, day, provider, model, purpose)
);
INSERT INTO usage_daily_new (tenant_id, operator, day, provider, model, purpose, calls, tokens_in, tokens_out, cost_usd)
  SELECT 1, 0, day, provider, model, purpose, calls, tokens_in, tokens_out, cost_usd FROM usage_daily;
DROP TABLE usage_daily;
ALTER TABLE usage_daily_new RENAME TO usage_daily;

-- +goose Down
-- Back to one implicit tenant: tenant 1's settings are kept, and the ledger and sender
-- rules of every tenant are folded together (the first sender rule for an address wins).
CREATE TABLE usage_daily_old (
  day       TEXT NOT NULL,
  provider  TEXT NOT NULL, model TEXT NOT NULL,
  purpose   TEXT NOT NULL,
  calls     INTEGER NOT NULL, tokens_in INTEGER NOT NULL, tokens_out INTEGER NOT NULL,
  cost_usd  REAL NOT NULL,
  PRIMARY KEY (day, provider, model, purpose)
);
INSERT INTO usage_daily_old (day, provider, model, purpose, calls, tokens_in, tokens_out, cost_usd)
  SELECT day, provider, model, purpose, SUM(calls), SUM(tokens_in), SUM(tokens_out), SUM(cost_usd)
  FROM usage_daily GROUP BY day, provider, model, purpose;
DROP TABLE usage_daily;
ALTER TABLE usage_daily_old RENAME TO usage_daily;

CREATE TABLE settings_old (key TEXT PRIMARY KEY, value TEXT NOT NULL);
INSERT INTO settings_old (key, value) SELECT key, value FROM settings WHERE tenant_id = 1;
DROP TABLE settings;
ALTER TABLE settings_old RENAME TO settings;

CREATE TABLE sender_rules_old (
  id         INTEGER PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id),
  match_type TEXT NOT NULL,
  value      TEXT NOT NULL,
  rule_id    INTEGER REFERENCES rules(id) ON DELETE CASCADE,
  verdict    TEXT NOT NULL,
  source     TEXT NOT NULL,
  hits       INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  UNIQUE (user_id, match_type, value)
);
INSERT OR IGNORE INTO sender_rules_old (id, user_id, match_type, value, rule_id, verdict, source, hits, created_at)
  SELECT id, user_id, match_type, value, rule_id, verdict, source, hits, created_at FROM sender_rules ORDER BY id;
DROP TABLE sender_rules;
ALTER TABLE sender_rules_old RENAME TO sender_rules;

DROP INDEX batches_tenant;
ALTER TABLE batches DROP COLUMN tenant_id;
DROP INDEX rules_tenant;
ALTER TABLE rules DROP COLUMN tenant_id;
DROP INDEX accounts_tenant;
ALTER TABLE accounts DROP COLUMN shared;
ALTER TABLE accounts DROP COLUMN tenant_id;
DROP TABLE identities;
DROP INDEX users_tenant;
ALTER TABLE users DROP COLUMN tenant_id;
DROP TABLE tenants;
