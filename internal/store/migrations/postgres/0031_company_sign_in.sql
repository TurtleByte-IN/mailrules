-- +goose Up
-- Company sign-in (MAI-246), as sqlite/0031_company_sign_in.sql describes it. plans keeps
-- its row security and grants (0024): a new column needs none of its own.
ALTER TABLE plans ADD COLUMN company_sign_in BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose Down
ALTER TABLE plans DROP COLUMN company_sign_in;
