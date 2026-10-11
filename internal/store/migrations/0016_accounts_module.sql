-- +goose Up
-- A mailbox a module added (ext.Host.AddMailbox, MAI-174): its server is the module's
-- choice, not the person's, so the hosted build connects to it wherever it is, even on the
-- operator's own network (such as the operator's Proton Mail Bridge). A server the person
-- names must be on the public internet there. Every one-click mailbox so far was added by
-- a module.
ALTER TABLE accounts ADD COLUMN module INTEGER NOT NULL DEFAULT 0;
UPDATE accounts SET module = 1 WHERE oauth = 1;

-- +goose Down
ALTER TABLE accounts DROP COLUMN module;
