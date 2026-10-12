-- +goose Up
-- The plan of each tenant of a build that sells plans (MAI-210): what the billing module last
-- reported, kept by the daemon. A tenant with no row is in its trial, which runs TrialDays
-- from tenants.created_at; once that is over with no plan bought, the plan has ended.
-- record is the module's own (the billing service's customer and subscription ids), never
-- shown. ended_at is when the plan ended, set by the end-of-plan job or by the module (a
-- trial or a period that runs out ends at its own time); NULL while a plan runs. warned,
-- warned_at and secrets_deleted are the steps the end-of-plan job has taken since: the
-- warning emails sent (0 to 3) and when the latest went, and whether the mailbox passwords
-- and tokens were deleted. A plan bought again clears them.
CREATE TABLE plans (
  tenant_id       INTEGER PRIMARY KEY,
  plan            TEXT NOT NULL,              -- personal | pro | ended
  interval        TEXT NOT NULL DEFAULT '',   -- month | year | '' (none)
  status          TEXT NOT NULL DEFAULT '',   -- active | past_due | cancels_at_period_end | ''
  period_end      INTEGER,                    -- end of the paid period; NULL = not known
  record          TEXT NOT NULL DEFAULT '',
  ended_at        INTEGER,
  warned          INTEGER NOT NULL DEFAULT 0,
  warned_at       INTEGER,                    -- when the latest warning was sent; NULL = none
  secrets_deleted INTEGER NOT NULL DEFAULT 0,
  updated_at      INTEGER NOT NULL
);

-- +goose Down
DROP TABLE plans;
