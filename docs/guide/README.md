---
title: MailRules user guide
description: How to install, set up and run MailRules, the self-hosted daemon that sorts your email over IMAP with rules you write in plain English.
order: 0
---

MailRules watches one or more IMAP mailboxes and sorts each new email as it arrives, using rules you write in plain English or as structured conditions. A decision model reads the email when a rule needs one; rules made only of conditions need no model at all. Everything runs on your own machine as one program with the web UI built in.

It starts safe. It listens on this machine only, and dry-run is on, so it records what it would do and changes nothing in your mailbox until you switch dry-run off.

## Getting started

1. [Install](./install.md): Docker, Docker Compose, Homebrew, a single binary, or a systemd service.
2. [First run](./first-run.md): create the admin account, choose a decision model and connect a mailbox.
3. [Rules](./rules.md): write rules in plain English or as conditions, test them, and import or export them as YAML.
4. [Dry-run and going live](./dry-run.md): check what MailRules would do, then let it act.

## Running it day to day

- [Decision models and keys](./models.md): Jev, Clef, Claude, OpenAI-compatible endpoints and Ollama; thresholds; keys; what it costs.
- [Undo and activity](./undo-and-activity.md): see every decision, correct mistakes, undo one email or a whole batch.
- [Summary email](./summary-email.md): a daily or weekly email of what was sorted, sent through your own mail server.
- [Backup and the master key](./backup.md): what to back up, how to restore, and what losing the key means.
- [Security](./security.md): the defaults, putting MailRules behind a reverse proxy, and reporting a vulnerability.
- [Troubleshooting](./troubleshooting.md): sign-in failures, mail that is not sorted, and reading the logs.
- [Settings reference](./settings.md): every environment variable and flag, with its default.

## Other documentation

- [HTTP API](https://github.com/TurtleByte-IN/mailrules/blob/main/docs/api.md): everything the web UI does, for scripts.
- [Source code](https://github.com/TurtleByte-IN/mailrules) and [releases](https://github.com/TurtleByte-IN/mailrules/releases).
