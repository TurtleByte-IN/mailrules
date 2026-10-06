# Live smoke checklist

A manual check against real mailboxes, run before each release. The automated tests use an in-memory IMAP server; this is the only place real iCloud and real Fastmail behaviour is checked.

Run the whole list twice: once with an iCloud account and once with a Fastmail account. Use test mailboxes, not ones you depend on. Each needs an app-specific password, and a second address to send test mail from.

Use the binary or image you are about to release, with a fresh data directory. Steps name the screen to use; where the web UI is not built in, the endpoint in brackets does the same thing (see `docs/api.md`).

Record the result at the bottom. Any unticked box blocks the release.

## 1. Start

- [ ] A fresh install starts, `/healthz` and `/readyz` answer 200, and the log has no errors.
- [ ] The log line `listening` shows `dry_run: true`.
- [ ] First-run setup creates the admin account and signs in [`POST /api/auth/setup`].

## 2. Dry-run

- [ ] **Add the account** with its app password. It reaches the status Live, and its folders are listed [`POST /api/accounts`, `GET /api/accounts/{id}/folders`].
- [ ] **Arrival is detected within seconds.** Send a mail from the second address. It appears in the activity feed within 10 seconds, without reloading the page [`GET /api/activity`, `GET /api/events`].
- [ ] **Mail stays unread.** In the provider's own webmail, that message is still unread and still in the inbox.
- [ ] **A condition rule would move mail.** Create a rule from conditions only (for example: sender is the second address, move to a folder named `Smoke`). Send another mail. The feed shows the rule and the move it would make, marked as dry-run.
- [ ] **Nothing changed.** In webmail the message is still in the inbox, unread, and the folder `Smoke` has no new mail.
- [ ] **Needs review.** Send a mail no rule clearly covers (or remove the model key and send one that needs a model). It shows up in Needs review with a reason; resolving it there records the choice [`GET /api/review`, `POST /api/review/{message_id}/resolve`].
- [ ] **Cleanup preview.** Preview a cleanup of the inbox. The counts per outcome add up to the number of messages selected, and nothing in the mailbox changes [`POST /api/cleanup/preview`].
- [ ] **Cleanup run in dry-run.** Run it. The batch finishes, and nothing in the mailbox changes [`POST /api/cleanup/run`].

## 3. Live

Switch dry-run off in Settings [`PATCH /api/settings {"dry_run": false}`, or `mailrules dry-run off`]. No restart.

- [ ] **A condition rule moves mail.** Send a mail that matches the rule. Within 10 seconds it is in `Smoke` in webmail, and it is still unread.
- [ ] **Undo restores folder and flags.** Before undoing, note the message's flags in webmail (unread, flagged or not). Undo the action in the feed [`POST /api/actions/{id}/undo`]. The message is back in the inbox with the same flags.
- [ ] **Nothing else was touched.** No other message moved, and the Trash and Junk folders have nothing new.
- [ ] **Network loss.** Send one mail, then cut the daemon's network for 5 minutes (unplug, or block outbound port 993). The account shows Reconnecting. Send two more mails from the second address during the outage. Restore the network.
  - [ ] The account returns to Live by itself.
  - [ ] All three mails appear in the feed exactly once each: none missing, none twice.
  - [ ] Each was acted on once (one move per message in the action log).
- [ ] **Revoked password.** Revoke the app password at the provider. Within a few minutes the account shows the auth-failed state (`auth_failed`) and stops: the log shows no further login attempts.
  - [ ] Entering a new app password and reconnecting brings it back to Live [`PATCH /api/accounts/{id} {"password": "..."}`, then `POST /api/accounts/{id}/reconnect`], and mail that arrived meanwhile is processed once.
- [ ] **Cleanup run, live.** Preview, then run a cleanup on a folder with a known number of messages. Progress moves, the batch finishes, and the messages are where the preview said they would go.
- [ ] **Cleanup undo.** Undo the whole batch [`POST /api/batches/{id}/undo`]. Every message is back in its original folder with its original flags.

## 4. Afterwards

- [ ] The log contains no password, no API key and no email body (search it for the app password and for a phrase from a test mail).
- [ ] Deleting the account removes it and its history [`DELETE /api/accounts/{id}`].
- [ ] Revoke the test app passwords.

## Result

| | iCloud | Fastmail |
| --- | --- | --- |
| Date | | |
| Version (`mailrules version`) | | |
| Run as (binary, Docker, systemd) | | |
| All boxes ticked | | |
| Notes | | |
