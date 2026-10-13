-- +goose Up
-- Seat checks (MAI-247), as sqlite/0030_seat_checks.sql describes them, kept to their tenant
-- like every table (0023).
CREATE TABLE seat_checks (
  tenant_id  BIGINT PRIMARY KEY DEFAULT mailrules_tenant() REFERENCES tenants(id) ON DELETE CASCADE,
  version    BIGINT NOT NULL DEFAULT 0,
  created_at BIGINT NOT NULL,
  attempts   BIGINT NOT NULL DEFAULT 0,
  next_at    BIGINT NOT NULL
);
CREATE INDEX seat_checks_next ON seat_checks(next_at);

ALTER TABLE seat_checks ENABLE ROW LEVEL SECURITY;
ALTER TABLE seat_checks FORCE ROW LEVEL SECURITY;
CREATE POLICY mailrules_tenant ON seat_checks USING (tenant_id = mailrules_tenant()) WITH CHECK (tenant_id = mailrules_tenant());
GRANT SELECT, INSERT, UPDATE, DELETE ON seat_checks TO mailrules_app, mailrules_worker;
-- The seat worker finds the tenants with checks waiting through mailrules_tenants('seats').
CREATE POLICY mailrules_definer ON seat_checks FOR SELECT TO mailrules_definer USING (true);
GRANT SELECT ON seat_checks TO mailrules_definer;

-- mailrules_tenants learns the kind 'seats'; replacing it keeps its owner and grants. The
-- function is the definer's, so it is replaced as the definer, which needs CREATE on the
-- schema for that moment only (as 0023 made it).
GRANT CREATE ON SCHEMA public TO mailrules_definer;
SET ROLE mailrules_definer;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION mailrules_tenants(kind text) RETURNS SETOF bigint
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
  CASE kind
    WHEN 'all' THEN RETURN QUERY SELECT id FROM tenants ORDER BY id;
    WHEN 'notices' THEN RETURN QUERY SELECT DISTINCT tenant_id FROM notices WHERE handed_at IS NULL ORDER BY 1;
    WHEN 'moves' THEN RETURN QUERY SELECT DISTINCT tenant_id FROM moves WHERE handed_at IS NULL ORDER BY 1;
    WHEN 'removed' THEN RETURN QUERY SELECT DISTINCT tenant_id FROM removed_mailboxes ORDER BY 1;
    WHEN 'forgotten' THEN RETURN QUERY SELECT DISTINCT tenant_id FROM forgotten ORDER BY 1;
    WHEN 'drafts' THEN RETURN QUERY SELECT DISTINCT tenant_id FROM draft_jobs ORDER BY 1;
    WHEN 'seats' THEN RETURN QUERY SELECT tenant_id FROM seat_checks ORDER BY 1;
    ELSE RAISE EXCEPTION 'mailrules_tenants: unknown kind %', kind;
  END CASE;
END
$$;
-- +goose StatementEnd
RESET ROLE;
REVOKE CREATE ON SCHEMA public FROM mailrules_definer;

-- +goose Down
GRANT CREATE ON SCHEMA public TO mailrules_definer;
SET ROLE mailrules_definer;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION mailrules_tenants(kind text) RETURNS SETOF bigint
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
  CASE kind
    WHEN 'all' THEN RETURN QUERY SELECT id FROM tenants ORDER BY id;
    WHEN 'notices' THEN RETURN QUERY SELECT DISTINCT tenant_id FROM notices WHERE handed_at IS NULL ORDER BY 1;
    WHEN 'moves' THEN RETURN QUERY SELECT DISTINCT tenant_id FROM moves WHERE handed_at IS NULL ORDER BY 1;
    WHEN 'removed' THEN RETURN QUERY SELECT DISTINCT tenant_id FROM removed_mailboxes ORDER BY 1;
    WHEN 'forgotten' THEN RETURN QUERY SELECT DISTINCT tenant_id FROM forgotten ORDER BY 1;
    WHEN 'drafts' THEN RETURN QUERY SELECT DISTINCT tenant_id FROM draft_jobs ORDER BY 1;
    ELSE RAISE EXCEPTION 'mailrules_tenants: unknown kind %', kind;
  END CASE;
END
$$;
-- +goose StatementEnd
RESET ROLE;
REVOKE CREATE ON SCHEMA public FROM mailrules_definer;
DROP TABLE seat_checks;
