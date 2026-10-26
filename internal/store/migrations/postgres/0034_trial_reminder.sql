-- +goose Up
-- The trial-ending reminder (MAI-288), as sqlite/0034_trial_reminder.sql describes it.
-- tenants keeps its row security and grants (0023): a new column needs none of its own.
ALTER TABLE tenants ADD COLUMN trial_reminded_at BIGINT;

-- +goose Down
ALTER TABLE tenants DROP COLUMN trial_reminded_at;
