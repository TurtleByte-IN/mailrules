-- +goose Up
-- Company sign-in (MAI-246): whether the company sign-in add-on is on a Team tenant's
-- subscription, as the billing module reports it. A Team tenant may use company sign-in
-- with enough paid seats (seats), or with the add-on. 0 for any other plan.
ALTER TABLE plans ADD COLUMN company_sign_in INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE plans DROP COLUMN company_sign_in;
