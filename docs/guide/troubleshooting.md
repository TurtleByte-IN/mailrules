---
title: Troubleshooting
description: Fix mailbox sign-in failures, find out why mail is not being sorted, work through Needs review and read the logs.
order: 100
---

## A mailbox says Sign-in failed

The mail server refused the user name or password. MailRules stops trying for that mailbox until you fix it.

- Use an app password, not the password you sign in to your provider with. iCloud, Gmail, Fastmail and Yahoo require one. See [App passwords](./first-run.md#app-passwords).
- App passwords stop working when you revoke them or change your main password. Make a new one.
- On a hosted MailRules, deleting your sign-in account deletes your mailboxes' passwords and your saved model keys at once. If you sign in again with the same account within 30 days, your rules and history are back, but each mailbox says "Your MailRules sign-in was deleted, so this mailbox's password was deleted with it" and waits for a new app password, and your model keys need entering again.
- Click **New app password** on the mailbox (or **Edit** → **New app password**), paste it, and **Save**. MailRules reconnects at once.

When adding a mailbox, the same problem reads: "The mail server refused the sign-in. Check the username and use an app password, not your account password."

### iCloud

- The server is `imap.mail.me.com`, port 993, TLS. Pick **iCloud Mail** and the wizard fills it in.
- Use an app-specific password from [account.apple.com](https://account.apple.com) → Sign-In and Security → App-Specific Passwords. Your Apple Account needs two-factor authentication to create one.
- Enter your iCloud address (`name@icloud.com`, `@me.com` or `@mac.com`). MailRules tries the full address, then the part before the `@`.
- iCloud's special folders are Archive, Junk, Deleted Messages and Sent Messages. iCloud empties Deleted Messages after 30 days, which is why trashed mail goes to `MailRules Trash` by default.

### Proton Mail

- MailRules reaches Proton Mail only through Proton Mail Bridge, which must be running and signed in on the same machine. While Bridge is closed, the mailbox says **Reconnecting** and catches up once Bridge is back.
- The password is the one in Bridge's **Mailbox details**, not your Proton password. If Bridge shows a different one than you entered, click **New app password** on the mailbox and paste it.
- "Could not reach Proton Mail Bridge at 127.0.0.1:1143": Bridge is not running, or uses another port. Check the IMAP port in Bridge's Mailbox details.
- If you changed Bridge's IMAP port, or switched it from STARTTLS to SSL, click **Edit** on the mailbox, set the new **Port** or **Encryption** and **Save**; there is no need to remove and add it again. MailRules tests the new settings first and saves them only if that works. If Bridge's certificate is new as well, accept it there.
- MailRules in Docker cannot reach Bridge; see [Install](./install.md#docker).

## A mailbox says Certificate changed

The server presented a certificate other than the one accepted for the mailbox, or, for a mailbox that never needed one accepted, a certificate the system does not trust. MailRules stopped connecting before sending the password, and does not retry.

1. On **Mailboxes**, click **Check certificate**. MailRules connects once more, on its own, and shows the certificate the server presents now: its SHA-256 fingerprint, who it is issued to and by, and its dates.
2. If you know the server's certificate was replaced, for example after reinstalling Proton Mail Bridge or renewing your own server's certificate, click **Accept certificate**. MailRules trusts that one from then on and reconnects.
3. If you did not expect a change, do not accept it: something else may be answering in your server's place. Find out why first.

If the server presents the accepted certificate again by the time you check, MailRules simply reconnects. From the command line, `mailrules accounts test <id>` prints the new certificate; accepting it is done in the web UI.

When adding a mailbox, the connection test shows the same check for a server whose certificate the system does not trust; see [Servers with their own certificate](./first-run.md#servers-with-their-own-certificate).

## A mailbox says Error or Reconnecting

- **Error** means the secure connection could not be set up, or the watched folder no longer exists. Check the host, port and encryption (for **Other IMAP server** or **Proton Mail**), or choose another **Watched folder** under **Edit**, then click **Reconnect**.
- **Reconnecting** means the connection dropped. MailRules retries by itself, waiting longer each time, up to 5 minutes between tries, and catches up on mail that arrived in the meantime. **Reconnect now** on Overview tries at once.
- **Reconnecting** with the error `decrypt failed` means the master key is not the one the passwords were stored with. See [If the master key is lost](./backup.md#if-the-master-key-is-lost). A start with no `master.key` at all, after a restore or a move, no longer gets this far: MailRules stops and says the master key is missing, as described in [Restoring](./backup.md#restoring).

## Nothing is being sorted

Work through these in order:

1. **Is dry-run on?** The banner at the top says so. In dry-run, Activity shows **Would move to …** and nothing changes in the mailbox. See [Dry-run and going live](./dry-run.md).
2. **Is the mailbox live?** Overview and Mailboxes show its status. A paused mailbox is not sorted until you click **Resume**.
3. **Is the mail new?** MailRules sorts only mail that arrives after the mailbox was connected. Use **Cleanup** for mail that was already there.
4. **Is the right folder watched?** MailRules watches one folder per mailbox, `INBOX` unless you changed **Watched folder**. Mail that your provider's own filters move elsewhere first is not seen.
5. **Did a rule match?** In Activity, rows reading "No rule matched" mean no rule's conditions fit, or the model picked none of the candidate rules. Click the row to see each step, and adjust the rule. **Test on last 200 emails** on a rule shows how many recent emails it matches.
6. **Is a model set up?** Rules written in plain English need a decision model. Without one, Settings shows "The … decision model needs …", and the email waits in Needs review. Condition-only rules work without one.
7. **Is it your own mail?** With **Leave my own emails alone** on, mail sent from the mailbox's own address is never sorted.
8. **Did the action fail?** A row reading **Failed: …** says why. "no Archive folder" (or Junk, or Trash) means your server marks no folder with that role: use a rule that moves to a named folder instead.
9. **Can the server move mail?** If Mailboxes shows "cannot move mail", the server supports neither IMAP MOVE nor UIDPLUS, and rules there can only flag and mark mail.

New mail is sorted within seconds on servers with push (IDLE). Without it, MailRules checks once a minute.

## Too much lands in Needs review

Needs review holds emails the model was not sure about. To send fewer there:

- Answer them. Each answer teaches the fallback model. Once dry-run is off, three confident decisions in a row for the same sender, none of them corrected, make a sender rule that needs no model at all.
- Use **Always do this for …** when you answer, so that sender is handled at once from then on.
- Make rules more specific: add conditions, or describe the emails more precisely in **When the email is about**.
- Make sure the fallback model works: it needs an Anthropic API key. Without one, MailRules never asks for a second opinion; Settings says **Not active** under **Fallback model** and the log has a warning. See [Thresholds and the fallback model](./models.md#thresholds-and-the-fallback-model).
- Lower a rule's **Act when sure above**, or **Act at … or above** in Settings. Lower thresholds mean more mistakes; you can undo them in Activity.

If the reason on a card is "Gave up after … retries", the model could not be reached for about half an hour. Check the key and the provider's status, and the log.

## Reading the logs

MailRules writes its log to standard error, one JSON object per line.

| How you run it | Where to read it |
| --- | --- |
| Docker | `docker logs mailrules` |
| Docker Compose | `docker compose -f deploy/docker-compose.yml logs mailrules` |
| systemd | `journalctl -u mailrules` |
| In a terminal | The terminal you started it in |

Set `LOG_LEVEL=debug` for more detail, including one line per model call with its duration and outcome. Logs never contain passwords, keys or email text at any level.

Lines worth searching for:

| Message | Meaning |
| --- | --- |
| `listening` | Started. Shows the address, version, whether dry-run is on and how many mailboxes it has. |
| `account status` | A mailbox changed status, with the error if any. |
| `account connection test failed` | A connection test in the add-mailbox wizard failed; the server's own error is here. |
| `no decision model yet; new mail that needs one waits in Needs review until it is set` | The decision model is missing a key or URL. |
| `could not process a message` | An email failed and will be retried. |
| `the watched folder was rebuilt on the server; starting again from its newest message` | The server renumbered the watched folder, so MailRules skipped ahead to avoid sorting old mail twice. |
| `summary email cannot be sent until these are set` | Settings for the [summary email](./summary-email.md) are missing. |
| `summary email not sent` | Sending the summary failed; the reason is in the line. |
| `web UI is reachable from other machines; put it behind a reverse proxy with TLS` | MailRules listens on a non-loopback address. Expected inside Docker, where the port is published to `127.0.0.1` only. |

## Checking a mailbox from the command line

```bash
mailrules accounts list
mailrules accounts test 1
mailrules accounts test 1 --watch
```

`list` shows every mailbox with its number, server, watched folder and status. `test` logs in with the stored password and prints the folders and the special ones it recognised, changing nothing. With `--watch` it also prints the sender and subject of each new email as it arrives, until you press Ctrl-C; it does not sort them.

Run these with the same data directory and master key settings as the daemon. With Docker, run them inside the container: `docker exec mailrules /mailrules accounts test 1`. For the systemd unit, see [Run as a systemd service](./install.md#run-as-a-systemd-service-linux).

## Other problems

- **The page keeps asking me to sign in.** The cookie is HTTPS-only but you opened MailRules over plain HTTP. Set `MAILRULES_COOKIE_SECURE=false` when you use `http://127.0.0.1`, or `true` and open it over HTTPS behind a proxy. See [Signing in](./security.md#signing-in).
- **MailRules does not start and says "the master key is missing", or that the master key opens none of the stored passwords.** The database holds mailbox passwords or model keys sealed under a key that MailRules cannot find or does not have. Restore `master.key` from your backup into the data directory, or see [If the master key is lost](./backup.md#if-the-master-key-is-lost) to go on without it.
- **MailRules does not start and lists settings.** It checks every setting at startup and prints one line per problem, naming the variable. The [settings reference](./settings.md) lists the allowed values.
- **I forgot the admin password.** Run `mailrules users reset-password` on the machine MailRules runs on, with the same `MAILRULES_DATA_DIR`. With Docker: `docker exec -it mailrules /mailrules users reset-password`. See [Changing or resetting the password](./security.md#changing-or-resetting-the-password).
- **Port 8080 is in use.** Set `MAILRULES_LISTEN=127.0.0.1:8090`, or with Compose, `MAILRULES_PORT=8090` in `deploy/.env`.
- **Something else.** Open an issue on [GitHub](https://github.com/TurtleByte-IN/mailrules/issues) with what you did, what you expected and what happened. Never paste passwords, API keys or email contents into an issue.
