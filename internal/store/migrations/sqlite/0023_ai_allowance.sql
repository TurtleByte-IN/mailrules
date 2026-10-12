-- +goose Up
-- The monthly allowance of AI-sorted emails a module sets per tenant (MAI-188): how many
-- emails a model decided for the tenant on keys the operator holds, per calendar month
-- (UTC, YYYY-MM), as the usage ledger keeps UTC days. Emails decided on the tenant's own
-- keys are not counted. A cleanup check counts each email it asked a model about; live
-- sorting counts an email once, with its decision (messages.metered), so a message tried
-- again after a failure is not counted twice.
CREATE TABLE ai_emails (
  tenant_id INTEGER NOT NULL,
  month     TEXT NOT NULL,
  emails    INTEGER NOT NULL,
  PRIMARY KEY (tenant_id, month)
);

ALTER TABLE messages ADD COLUMN metered INTEGER NOT NULL DEFAULT 0; -- 1 = counted in ai_emails

-- The one-time first-cleanup grant a module may give a tenant (ext.Limits.FirstCleanupEmails):
-- how many of it the tenant's cleanup checks have used. Emails a cleanup check has a model
-- decide on the operator's keys draw on it first, then on the month's ai_emails. It never
-- resets; live sorting never draws on it.
CREATE TABLE first_cleanup (
  tenant_id INTEGER PRIMARY KEY,
  emails    INTEGER NOT NULL
);

-- +goose Down
DROP TABLE first_cleanup;
ALTER TABLE messages DROP COLUMN metered;
DROP TABLE ai_emails;
