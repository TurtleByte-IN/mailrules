-- +goose Up
-- When company sign-in stops for a team that lost it (MAI-258). company_sign_in_held is
-- whether the tenant could use company sign-in the last time the daemon checked its plan
-- against the build's terms; company_sign_in_ends_at is the end of the paid period in force
-- when it lost it (or the day the plan ended), fixed however often the plan renews, and
-- NULL while it holds it or never did. Both are the daemon's (store.CheckCompanySignIn),
-- never the billing module's: a report (SetPlan) leaves them as they are.
ALTER TABLE plans ADD COLUMN company_sign_in_held INTEGER NOT NULL DEFAULT 0;
ALTER TABLE plans ADD COLUMN company_sign_in_ends_at INTEGER;

-- +goose Down
ALTER TABLE plans DROP COLUMN company_sign_in_ends_at;
ALTER TABLE plans DROP COLUMN company_sign_in_held;
