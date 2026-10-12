-- +goose Up
-- The plan of each tenant of a build that sells plans (MAI-210), as sqlite/0024_plans.sql
-- describes it, kept to its tenant like every table (0023).
CREATE TABLE plans (
  tenant_id       BIGINT PRIMARY KEY DEFAULT mailrules_tenant() REFERENCES tenants(id) ON DELETE CASCADE,
  plan            TEXT NOT NULL,              -- personal | pro | ended
  interval        TEXT NOT NULL DEFAULT '',   -- month | year | '' (none)
  status          TEXT NOT NULL DEFAULT '',   -- active | past_due | cancels_at_period_end | ''
  period_end      BIGINT,                     -- end of the paid period; NULL = not known
  record          TEXT NOT NULL DEFAULT '',
  ended_at        BIGINT,
  warned          BIGINT NOT NULL DEFAULT 0,
  warned_at       BIGINT,                     -- when the latest warning was sent; NULL = none
  secrets_deleted BOOLEAN NOT NULL DEFAULT FALSE,
  updated_at      BIGINT NOT NULL
);
ALTER TABLE plans ENABLE ROW LEVEL SECURITY;
ALTER TABLE plans FORCE ROW LEVEL SECURITY;
CREATE POLICY mailrules_tenant ON plans USING (tenant_id = mailrules_tenant()) WITH CHECK (tenant_id = mailrules_tenant());
GRANT SELECT, INSERT, UPDATE, DELETE ON plans TO mailrules_app, mailrules_worker;

-- +goose Down
DROP TABLE plans;
