-- +goose Up
-- Each tenant's data key (MAI-150), as sqlite/0025_tenant_keys.sql describes it. The row is
-- the tenant's own, under its policy, like the tenant.
ALTER TABLE tenants ADD COLUMN dek_enc BYTEA;
ALTER TABLE tenants ADD COLUMN kek_id TEXT NOT NULL DEFAULT '';
-- The definer functions read who a tenant is, never its key.
REVOKE SELECT ON tenants FROM mailrules_definer;
GRANT SELECT (id, provider, external_id, created_at) ON tenants TO mailrules_definer;

-- +goose Down
REVOKE SELECT (id, provider, external_id, created_at) ON tenants FROM mailrules_definer;
GRANT SELECT ON tenants TO mailrules_definer;
ALTER TABLE tenants DROP COLUMN kek_id;
ALTER TABLE tenants DROP COLUMN dek_enc;
