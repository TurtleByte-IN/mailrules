-- +goose Up
-- When company sign-in stops for a team that lost it (MAI-258), as
-- sqlite/0035_company_sign_in_ends.sql describes it. plans keeps its row security and
-- grants (0024): new columns need none of their own.
ALTER TABLE plans ADD COLUMN company_sign_in_held BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE plans ADD COLUMN company_sign_in_ends_at BIGINT;

-- +goose Down
ALTER TABLE plans DROP COLUMN company_sign_in_ends_at;
ALTER TABLE plans DROP COLUMN company_sign_in_held;
