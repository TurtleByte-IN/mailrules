-- +goose Up
-- A mailbox a module's sign-in form added logs in with a password the person never saw
-- (Proton Mail Bridge's), so when the server refuses it the mailbox waits in
-- reconnect_needed, for the person to sign in through the form again, not in auth_failed
-- for a new app password (MAI-175). Move the ones already stopped there.
UPDATE accounts SET status = 'reconnect_needed' WHERE module = 1 AND status = 'auth_failed';

-- +goose Down
UPDATE accounts SET status = 'auth_failed' WHERE module = 1 AND oauth = 0 AND status = 'reconnect_needed';
