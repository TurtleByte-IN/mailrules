-- +goose Up
-- The Team plan (MAI-234): roles, invitations, the audit log and seats.
--
-- Every user has a role in their tenant: owner (billing, members, everything), admin
-- (mailboxes, shared rules, members, settings) or member (their own mailboxes, using the
-- shared rules). Everyone who exists now becomes an owner, so nothing anyone can do today
-- stops working: a tenant of one, self-host's admin, and every person of a tenant that
-- already has several. New users get their role where they are made (store.SignInIdentity).
ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'member' CHECK (role IN ('owner', 'admin', 'member'));
UPDATE users SET role = 'owner';

-- An invitation to join a tenant, by email, with the role the person gets. A module of the
-- build sends it (ext.Team.Invite) and keeps its own reference to it in record. It holds a
-- seat until the person joins (their user takes the seat), it is revoked, or it expires at
-- expires_at. accepted_at is when the sign-in service said the person accepted it. One per
-- address and tenant. invited_by is who sent it; no foreign key: the inviter may leave.
CREATE TABLE invitations (
  id          INTEGER PRIMARY KEY,
  tenant_id   INTEGER NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  email       TEXT NOT NULL,
  role        TEXT NOT NULL CHECK (role IN ('owner', 'admin', 'member')),
  invited_by  INTEGER,
  record      TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL,
  accepted_at INTEGER
);
CREATE UNIQUE INDEX invitations_email ON invitations(tenant_id, lower(email));

-- Who changed what in a tenant: its rules, sender rules, mailboxes, members, invitations,
-- settings and plan, each row written in the transaction of the change it records. It holds
-- names and addresses, never email content or a secret. actor is who did it as they were
-- then (an email, or the service that did it), actor_id their user while there is one.
-- Rows are never changed, with one exception: when a person is forgotten or removed from
-- the team, their address in actor and target becomes 'removed member #<user id>'
-- (store.forget). Only the retention job deletes rows, after AuditDays.
CREATE TABLE audit_log (
  id        INTEGER PRIMARY KEY,
  tenant_id INTEGER NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  at        INTEGER NOT NULL,
  actor_id  INTEGER,
  actor     TEXT NOT NULL,
  action    TEXT NOT NULL,
  target    TEXT NOT NULL DEFAULT '',
  detail    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX audit_log_tenant ON audit_log(tenant_id, at);
-- +goose StatementBegin
CREATE TRIGGER audit_log_append_only BEFORE UPDATE ON audit_log
WHEN NOT (NEW.id = OLD.id AND NEW.tenant_id = OLD.tenant_id AND NEW.at = OLD.at AND NEW.actor_id IS OLD.actor_id
  AND NEW.action = OLD.action AND NEW.detail = OLD.detail
  AND (NEW.actor = OLD.actor OR substr(NEW.actor, 1, 16) = 'removed member #')
  AND (NEW.target = OLD.target OR substr(NEW.target, 1, 16) = 'removed member #'))
BEGIN
  SELECT RAISE(ABORT, 'the audit log is append-only');
END;
-- +goose StatementEnd

-- The seats a tenant on a plan sold by the seat (the Team plan) bought; 0 for any other.
ALTER TABLE plans ADD COLUMN seats INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE plans DROP COLUMN seats;
DROP TRIGGER audit_log_append_only;
DROP TABLE audit_log;
DROP TABLE invitations;
ALTER TABLE users DROP COLUMN role;
