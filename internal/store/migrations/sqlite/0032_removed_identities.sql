-- +goose Up
-- A person removed from a team (MAI-253). Removing them lets go of their identity at once
-- (store.RemoveMember), so that signing in again under another organisation gives them an
-- account of their own instead of tenant_mismatch. This row remembers that the identity
-- (provider, subject) was removed from the tenant, so that signing in under the team's
-- organisation again joins the team only with a new invitation (store.SignInIdentity). It
-- goes when they join again, or with the tenant.
CREATE TABLE removed_identities (
  tenant_id  INTEGER NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  provider   TEXT NOT NULL,
  subject    TEXT NOT NULL,
  removed_at INTEGER NOT NULL,
  PRIMARY KEY (tenant_id, provider, subject)
);

-- +goose Down
DROP TABLE removed_identities;
