-- +goose Up
-- Removed identities (MAI-253), as sqlite/0032_removed_identities.sql describes them, kept to
-- their tenant like every table (0023).
CREATE TABLE removed_identities (
  tenant_id  BIGINT NOT NULL DEFAULT mailrules_tenant() REFERENCES tenants(id) ON DELETE CASCADE,
  provider   TEXT NOT NULL,
  subject    TEXT NOT NULL,
  removed_at BIGINT NOT NULL,
  PRIMARY KEY (tenant_id, provider, subject)
);

ALTER TABLE removed_identities ENABLE ROW LEVEL SECURITY;
ALTER TABLE removed_identities FORCE ROW LEVEL SECURITY;
CREATE POLICY mailrules_tenant ON removed_identities USING (tenant_id = mailrules_tenant()) WITH CHECK (tenant_id = mailrules_tenant());
GRANT SELECT, INSERT, UPDATE, DELETE ON removed_identities TO mailrules_app, mailrules_worker;

-- +goose Down
DROP TABLE removed_identities;
