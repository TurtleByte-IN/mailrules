-- +goose Up
-- Seats follow the team (MAI-247). An invitation past a Team plan's paid seats buys one more
-- at once; a seat freed (a member who left, an invitation revoked, withdrawn or expired) is
-- given back. The transaction that frees it queues a check of the tenant's seats here, when
-- its plan has seats; the seat worker asks the billing module for the seats the tenant then
-- uses, never fewer than the plan's least, when that is fewer than it pays for, and deletes
-- the check once the module agrees, or tries again later. One per tenant: version goes up
-- each time it is queued again, so one queued while the worker handled it is not lost.
CREATE TABLE seat_checks (
  tenant_id  INTEGER PRIMARY KEY,
  version    INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,           -- when it was first queued
  attempts   INTEGER NOT NULL DEFAULT 0, -- the billing module's refusals so far
  next_at    INTEGER NOT NULL            -- when the worker next handles it
);
CREATE INDEX seat_checks_next ON seat_checks(next_at);

-- +goose Down
DROP TABLE seat_checks;
