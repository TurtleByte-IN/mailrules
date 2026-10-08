# Live smoke checklist

A manual check against real mailboxes, run before each release. The automated tests use an in-memory IMAP server; this is the only place real iCloud and real Fastmail behaviour is checked.

Run the whole list twice: once with an iCloud account and once with a Fastmail account. Use test mailboxes, not ones you depend on. Each needs an app-specific password, and a second address to send test mail from.

Use the binary or image you are about to release, with a fresh data directory: build the release archives without publishing with `goreleaser release --snapshot --clean --skip=publish` (they land in `dist/`), or the image with `docker build -f deploy/Dockerfile .`. Steps name the screen to use; where the web UI is not built in, the endpoint in brackets does the same thing (see `docs/api.md`).

Record the result at the bottom. Any unticked box blocks the release.

## 1. Start

- [ ] A fresh install starts, `/healthz` and `/readyz` answer 200, and the log has no errors.
- [ ] The log line `listening` shows `dry_run: true`.
- [ ] First-run setup creates the admin account and signs in [`POST /api/auth/setup`].
- [ ] The setup guide follows: Decision model shows what the default decider still lacks beside it; saving its key there clears that warning [`PATCH /api/settings`]. Continue opens First mailbox, the connect wizard; Skip for now there lands on Overview with dry-run still on. Sign out and in again: the guide does not come back.

## 2. Dry-run

- [ ] **Add the account** with its app password. It reaches the status Live, and its folders are listed [`POST /api/accounts`, `GET /api/accounts/{id}/folders`].
- [ ] **Arrival is detected within seconds.** Send a mail from the second address. It appears in the activity feed within 10 seconds, without reloading the page [`GET /api/activity`, `GET /api/events`].
- [ ] **Mail stays unread.** In the provider's own webmail, that message is still unread and still in the inbox.
- [ ] **A condition rule would move mail.** Create a rule from conditions only (for example: sender is the second address, move to a folder named `Smoke`). Send another mail. The feed shows the rule and the move it would make, marked as dry-run.
- [ ] **Nothing changed.** In webmail the message is still in the inbox, unread, and the folder `Smoke` has no new mail.
- [ ] **Trash in dry-run makes no folder.** Settings shows "Send trashed mail to MailRules Trash" ticked [`GET /api/settings`: `"trash_to_folder": true`]. Block the second address in Senders (Always trash) and send a mail from it. The feed says "Would move to MailRules Trash", and webmail has no folder named `MailRules Trash` and nothing new in Trash. Unblock the address again (Senders, "Let my rules decide") [`DELETE /api/senders/address/{value}`], so the steps below are not trashed.
- [ ] **Needs review.** Send a mail no rule clearly covers (or remove the model key and send one that needs a model). It shows up in Needs review with a reason; resolving it there records the choice [`GET /api/review`, `POST /api/review/{message_id}/resolve`].
- [ ] **Cleanup check.** Check a cleanup of the inbox. "N of M checked" moves, the model calls and real cost grow, and the table then shows one row per email with the rule, action and confidence; nothing in the mailbox changes [`POST /api/cleanup/check`, `GET /api/cleanup/check`]. Reload the page mid-check and after it: the same check and table come back, with the same rows unticked.
- [ ] **Cleanup Sort in dry-run.** Untick a few rows, then Sort the rest. The batch finishes, nothing in the mailbox changes, and Senders lists no learned sender rule [`POST /api/cleanup/run`, `GET /api/senders?source=learned`].
- [ ] **No Suggest in the free build.** Add rules shows three tabs, Describe it, Build with conditions and Templates, and no Suggest from my mail [`GET /api/settings` has `"suggest": false` in `features`; `POST /api/rules/suggest` answers 404 `not_available`]. Suggest from my mail is checked on the hosted build, which includes its module.
- [ ] **Composer on another provider.** If you have an OpenAI key or an Ollama server, set Settings, Rule composer model to `openai:<model>` or `ollama:<model>` before adding that key or URL [`PATCH /api/settings {"composer_model": "ollama:llama3.2"}`]. Settings warns that the composer model needs it, and Describe it answers with a sentence naming it. Add the key or URL [`PATCH /api/settings {"ollama_url": "http://localhost:11434"}`]: the warning goes, Describe it returns drafts, and Usage lists that model as "(rule composer)" [`POST /api/rules/compose`, `GET /api/stats/usage`]. Put the composer model back afterwards.
- [ ] **A Claude key for the whole organisation.** If you have a Claude key made for every workspace rather than one, save it in Settings, Anthropic API key [`PATCH /api/settings {"keys": {"anthropic_api_key": "…"}}`]. With one workspace in the organisation, an "Anthropic workspace" row appears under the key with its name, ID and "Found automatically"; with several, a "Choose a workspace" picker, and choosing one saves it [`GET /api/settings/anthropic-workspaces`, `PATCH /api/settings {"anthropic_workspace_id": "wrkspc_…"}`]. Describe it then returns drafts. If the key may not list workspaces, the row says MailRules could not look them up; type the ID from the Claude Console. A key made for one workspace shows no such row, and Describe it works as before. The log never holds the key.

## 3. Live

Switch dry-run off in Settings [`PATCH /api/settings {"dry_run": false}`, or `mailrules dry-run off`]. No restart.

- [ ] **A condition rule moves mail.** Send a mail that matches the rule. Within 10 seconds it is in `Smoke` in webmail, and it is still unread.
- [ ] **Undo restores folder and flags.** Before undoing, note the message's flags in webmail (unread, flagged or not). Undo the action in the feed [`POST /api/actions/{id}/undo`]. The message is back in the inbox with the same flags.
- [ ] **Nothing else was touched.** No other message moved, and the Trash and Junk folders have nothing new.
- [ ] **Mail you move back stays put.** Send another mail that matches the rule. Once it is in `Smoke`, move it back to the inbox in webmail (or Apple Mail). It stays in the inbox, and the feed still shows it once. Do the same with an email waiting in Needs review: move it to another folder and back. Needs review still lists it once, and resolving it there works [`GET /api/review`].
- [ ] **Trash goes to MailRules Trash.** Block the second address again and send a mail from it. Within 10 seconds webmail shows a new ordinary folder `MailRules Trash` holding it, unread, and the real Trash (iCloud: Deleted Messages) has nothing new. The feed says "Moved to MailRules Trash", and the Overview's Trashed tile counts it. Send a second one: it lands in the same folder, next to the first. Undo one [`POST /api/messages/{id}/undo`]: it is back in the inbox.
- [ ] **Turned off, trash goes to the real Trash.** Untick the setting [`PATCH /api/settings {"trash_to_folder": false}`] and send another mail from the blocked address. It lands in the real Trash, and the feed says "Moved to Trash". Undo the remaining email in `MailRules Trash` from the feed: it comes back to the inbox all the same. Tick the setting again, then unblock the address.
- [ ] **Network loss.** Send one mail, then cut the daemon's network for 5 minutes (unplug, or block outbound port 993). The account shows Reconnecting. Send two more mails from the second address during the outage. Restore the network.
  - [ ] The account returns to Live by itself.
  - [ ] All three mails appear in the feed exactly once each: none missing, none twice.
  - [ ] Each was acted on once (one move per message in the action log).
- [ ] **Revoked password.** Revoke the app password at the provider. Within a few minutes the account shows the auth-failed state (`auth_failed`) and stops: the log shows no further login attempts.
  - [ ] Entering a new app password and reconnecting brings it back to Live [`PATCH /api/accounts/{id} {"password": "..."}`, then `POST /api/accounts/{id}/reconnect`], and mail that arrived meanwhile is processed once.
- [ ] **Rule test.** In Rules, test a rule on 20 emails, then on 200. "N of M emails tested" moves from the first second, the mail stays unread and where it was, and with `LOG_LEVEL=debug` the log has `rule test started` and `rule test finished` (tested, matched, model calls, duration), one `model call` line per attempt and the mail server's list and fetch times. Reload the page in the middle of a test: the log says `rule test cancelled by the client after N of M` at INFO, and there is no ERROR line.
- [ ] **Cleanup Sort, live.** Check a folder with a known number of messages, then Sort the selected rows. Progress moves, the batch finishes, and the messages are where the table said they would go. If you move one checked email by hand before sorting, it is reported skipped and left alone. Edit a rule after a check and the table shows an out-of-date notice and refuses Sort until you check again.
- [ ] **Cleanup undo.** Undo the whole batch [`POST /api/batches/{id}/undo`]. Every message is back in its original folder with its original flags, and the message counts only the emails the Sort really moved: the one you moved by hand (skipped) is not among them [`"emails"` in the answer].

## 4. Afterwards

- [ ] The log, run at `LOG_LEVEL=debug`, contains no password, no API key and no email body, subject or sender (search it for the app password and for a phrase and the address of a test mail).
- [ ] Deleting the account removes it and its history [`DELETE /api/accounts/{id}`].
- [ ] Revoke the test app passwords.

## 5. After tagging

Pushing the tag (`git tag v0.1.0 && git push origin v0.1.0`) runs the release workflow.

- [ ] The `release` workflow passed, and the GitHub Release lists four archives (linux and darwin, amd64 and arm64) and `checksums.txt`.
- [ ] A downloaded archive passes `sha256sum --ignore-missing -c checksums.txt`, and its `mailrules version` prints the tag without the `v`.
- [ ] `ghcr.io/turtlebyte-in/mailrules` has the version tag and `latest`, each for amd64 and arm64. After the first release only: the package starts private, so make it public in its package settings, then check `docker pull ghcr.io/turtlebyte-in/mailrules:latest` works signed out.

## Result

| | iCloud | Fastmail |
| --- | --- | --- |
| Date | | |
| Version (`mailrules version`) | | |
| Run as (binary, Docker, systemd) | | |
| All boxes ticked | | |
| Notes | | |
