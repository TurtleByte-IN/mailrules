-- +goose Up
-- A mailbox a module connects with one-click sign-in (MAI-156) logs in with OAuth (IMAP
-- AUTHENTICATE XOAUTH2): before every login the module turns the secret stored for it, a
-- refresh token kept in secret_enc like an app password, into an access token. 1 marks such
-- a mailbox; the module is the one that signs in to the mailbox's preset (gmail, outlook).
ALTER TABLE accounts ADD COLUMN oauth INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE accounts DROP COLUMN oauth;
