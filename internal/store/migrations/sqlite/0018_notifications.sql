-- +goose Up
-- Rule notifications through a module (MAI-189). A tenant's destinations are where notices
-- go: a webhook or a Slack incoming webhook. The address, and a webhook's signing secret,
-- are sealed under the master key like a mailbox password (secret_enc, dek_enc; empty =
-- deleted with the sign-in of the tenant's only member); url_hint is the address cut short,
-- the only form of it the API returns.
CREATE TABLE notify_destinations (
  id         INTEGER PRIMARY KEY,
  tenant_id  INTEGER NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  kind       TEXT NOT NULL,                -- webhook | slack
  url_hint   TEXT NOT NULL,
  secret_enc BLOB NOT NULL,                -- sealed JSON {"url", "secret"}
  dek_enc    BLOB NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX notify_destinations_tenant ON notify_destinations(tenant_id);

-- The destinations a rule tells when it acts live: a JSON array of notify_destinations ids.
ALTER TABLE rules ADD COLUMN notify TEXT NOT NULL DEFAULT '[]';

-- The outbox: one notice per destination for every email a rule with destinations acted on
-- live, written in the transaction that records the rule's last action on it, so a notice
-- exists exactly when the actions do and a restart loses none. The delivery worker hands
-- each to the module until the module takes it (handed_at), then blanks payload (the
-- sender and subject); the row stays for the delivery log until the retention job deletes
-- it after 30 days.
CREATE TABLE notices (
  id             INTEGER PRIMARY KEY,
  tenant_id      INTEGER NOT NULL,
  destination_id INTEGER NOT NULL REFERENCES notify_destinations(id) ON DELETE CASCADE,
  account_id     INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  message_id     INTEGER NOT NULL,         -- the Activity id the notice links to
  rule_id        INTEGER REFERENCES rules(id) ON DELETE SET NULL,
  rule_name      TEXT NOT NULL,
  payload        TEXT,                     -- JSON: mailbox, actions, folder, from, from_name, subject; NULL once handed over
  created_at     INTEGER NOT NULL,
  attempts       INTEGER NOT NULL DEFAULT 0,
  next_at        INTEGER NOT NULL,         -- when the worker next hands it over
  handed_at      INTEGER                   -- when the module took it; NULL = waiting
);
CREATE INDEX notices_waiting ON notices(next_at) WHERE handed_at IS NULL;
CREATE INDEX notices_tenant ON notices(tenant_id, created_at);

-- What the module reported of each delivery, for Settings' Recent deliveries.
CREATE TABLE notify_deliveries (
  id             INTEGER PRIMARY KEY,
  tenant_id      INTEGER NOT NULL,
  destination_id INTEGER NOT NULL REFERENCES notify_destinations(id) ON DELETE CASCADE,
  notice_id      INTEGER NOT NULL,
  rule_id        INTEGER REFERENCES rules(id) ON DELETE SET NULL,
  rule_name      TEXT NOT NULL,
  status         TEXT NOT NULL,            -- sent | failed
  reason         TEXT NOT NULL DEFAULT '',
  created_at     INTEGER NOT NULL
);
CREATE INDEX notify_deliveries_destination ON notify_deliveries(destination_id, created_at);
CREATE INDEX notify_deliveries_tenant ON notify_deliveries(tenant_id, created_at);

-- +goose Down
DROP TABLE notify_deliveries;
DROP TABLE notices;
ALTER TABLE rules DROP COLUMN notify;
DROP TABLE notify_destinations;
