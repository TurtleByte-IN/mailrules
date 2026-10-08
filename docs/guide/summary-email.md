---
title: Summary email
description: Get a daily or weekly email of what MailRules sorted, trashed and left for review, sent through your own mail server.
order: 70
---

MailRules can email you a summary every day or once a week:

- what waits in **Needs review**, with a link to review it;
- how many emails each rule sorted, with a link to see them in Activity;
- what was trashed and where it went, each with a **Wrong? Restore it** link to its row in Activity;
- what the models cost.

The summary holds senders and subjects only, never the text of an email. With dry-run on, it says that nothing was changed and lists what MailRules would have done.

The links open MailRules in your browser, so you sign in first if you are not already.

## Set up the outgoing mail server

MailRules sends the summary through an SMTP server you choose, such as your mail provider's. It is configured with environment variables only, and read at startup:

| Variable | What to set |
| --- | --- |
| `MAILRULES_SMTP_HOST` | The server's host name, such as `smtp.example.com` (no port or scheme). |
| `MAILRULES_SMTP_FROM` | The From address, such as `MailRules <me@example.com>`. Most servers accept only one of your own addresses. |
| `MAILRULES_SMTP_USER` | The user name to sign in with. Leave it empty for a server that takes mail without signing in. |
| `MAILRULES_SMTP_PASSWORD` or `MAILRULES_SMTP_PASSWORD_FILE` | The password, or a file holding it. Use an app password where your provider has them. Set only one of the two. |
| `MAILRULES_SMTP_TLS` | `starttls` (the default; the server must offer STARTTLS), `implicit` (TLS from the start, usually port 465), or `none`, which is allowed only for a mail server on the same machine. |
| `MAILRULES_SMTP_PORT` | Only if it is not the usual one: 587 for `starttls`, 465 for `implicit`, 25 for `none`. |
| `MAILRULES_PUBLIC_URL` | The address you open MailRules at, if it is not `http://127.0.0.1:8080`, such as `https://mailrules.example.com`. Links in the summary start with it. |

For example, in `deploy/.env` for Docker Compose, or `/etc/mailrules/env` for the systemd unit:

```bash
MAILRULES_SMTP_HOST=smtp.example.com
MAILRULES_SMTP_FROM=MailRules <me@example.com>
MAILRULES_SMTP_USER=me@example.com
MAILRULES_SMTP_PASSWORD=app-password-here
```

The compose file does not pass `MAILRULES_SMTP_PASSWORD_FILE` on; with Compose, use `MAILRULES_SMTP_PASSWORD`, or add the variable to the compose file's `environment:` section.

Restart MailRules after changing these. Until the required ones are set, the daemon logs `summary email cannot be sent until these are set` with the missing names, and Settings says which to set.

## Switch it on

In **Settings** → **Summary email**:

1. Tick **Send me a summary**.
2. Choose **How often**: **Every day** or **Every week**, and for a weekly one, the day under **On**.
3. Set the time under **At**. It is in your browser's time zone, saved each time you save the card.
4. **Send to** is your admin account's email unless you enter another address.

**Send a test email** sends a summary of the last 24 hours now, whether the summary is on or not. **Show preview** shows the next summary in the page. The card also shows when the next one is due and when the last one was sent.

## When it is sent

- MailRules checks every minute whether a summary is due. If it was not running at that time, the summary is still sent when it starts, up to six hours late; later than that it is skipped, and the next summary covers its time.
- Each summary covers the time since the previous one, at most 31 days.
- A summary that cannot be sent is tried five times in all, with waits that double from one minute, so about a quarter of an hour. The reason is in the log (`summary email not sent`).
- A summary is never sent twice for the same period, even across restarts.
