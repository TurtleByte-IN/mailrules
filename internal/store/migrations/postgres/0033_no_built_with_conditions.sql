-- +goose Up
-- The placeholder Build with conditions saved as `said` (MAI-261), as
-- sqlite/0033_no_built_with_conditions.sql describes it. FORCE RLS applies to the owner too
-- (0023), and no tenant is set while migrating, so the policy would hide every row: the
-- owner sets FORCE aside for these updates, inside the migration's transaction.
ALTER TABLE rules NO FORCE ROW LEVEL SECURITY;
UPDATE rules SET said = NULL WHERE said = 'Built with conditions';
UPDATE rules SET said = NULLIF(substr(said, 23), '') WHERE starts_with(said, 'Built with conditions' || chr(10));
ALTER TABLE rules FORCE ROW LEVEL SECURITY;

-- +goose Down
-- Nothing to restore: the placeholder was never the owner's words.
