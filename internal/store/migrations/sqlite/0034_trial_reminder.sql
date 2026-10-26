-- +goose Up
-- The trial-ending reminder (MAI-288): when the end-of-plan job emailed the tenant's owners
-- that its trial ends in 3 days. NULL = not sent; once set it is never sent again, across
-- ticks and restarts.
ALTER TABLE tenants ADD COLUMN trial_reminded_at INTEGER;

-- +goose Down
ALTER TABLE tenants DROP COLUMN trial_reminded_at;
