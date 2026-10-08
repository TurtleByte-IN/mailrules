---
title: Backup and the master key
description: What the data directory holds, how to back it up and restore it, and what happens if the master key is lost.
order: 80
---

Everything MailRules keeps is in its data directory: `/data` in the Docker image (the `mailrules-data` volume), `/var/lib/mailrules` with the systemd unit, and `./data` otherwise. See [Choosing a data directory](./install.md#choosing-a-data-directory).

## What is in the data directory

- `mailrules.db`, with `mailrules.db-wal` and `mailrules.db-shm` next to it while MailRules runs: the SQLite database. It holds your rules, sender rules, settings, decisions, the action log used for undo, the contacts learned from your Sent folder, short email snippets, and your mailbox passwords and model keys in encrypted form.
- `master.key`: the key that encrypts those passwords and keys. It is generated on first run, readable only by the user MailRules runs as, and never stored in the database.

The database never holds full email bodies. Your email itself stays on your mail server.

Back up the data directory as a whole, and keep the backup as private as your passwords: the database and the key together unlock them.

## Backing up

The database is written continuously, so copy it while MailRules is stopped:

**Docker:**

```bash
docker stop mailrules
docker run --rm -v mailrules-data:/data:ro -v "$PWD":/backup alpine \
  tar czf /backup/mailrules-backup.tar.gz -C /data .
docker start mailrules
```

With Compose, use `docker compose -f deploy/docker-compose.yml stop` and `start`. Compose names the volume after the project, usually `deploy_mailrules-data`; `docker volume ls` shows the exact name.

**systemd:**

```bash
sudo systemctl stop mailrules
sudo tar czf mailrules-backup.tar.gz -C /var/lib/mailrules .
sudo systemctl start mailrules
```

**Binary or Homebrew:** stop `mailrules serve`, copy the data directory, start it again.

Back up before every upgrade. A new version updates the database when it first starts, and MailRules has no command to undo that, so the backup is how you go back.

To keep a readable copy of just your rules as well, export them: **Export rules** on the Rules screen, or `mailrules rules export rules.yaml` (see [Rules](./rules.md#import-and-export-as-yaml)).

## Restoring

1. Stop MailRules.
2. Replace the contents of the data directory with the backup, including `master.key` (unless you keep the key elsewhere; see below).
3. Make sure the files belong to the user MailRules runs as: uid 65532 in the Docker image, `mailrules` with the systemd unit.
4. Start MailRules.

With Docker, for a backup made as above:

```bash
docker stop mailrules
docker run --rm -v mailrules-data:/data -v "$PWD":/backup alpine \
  sh -c 'rm -rf /data/* && tar xzf /backup/mailrules-backup.tar.gz -C /data && chown -R 65532:65532 /data'
docker start mailrules
```

**Restore the key before you start MailRules.** If `master.key` is missing from the data directory, neither `MAILRULES_MASTER_KEY` nor `MAILRULES_MASTER_KEY_FILE` is set, and the database holds stored mailbox passwords or model keys, MailRules does not start. It does not make a new key, because the passwords and keys stored under the old one could then never be read. It stops with a message that names the data directory:

```text
mailrules: the master key is missing: there is no master.key in /var/lib/mailrules, and neither
MAILRULES_MASTER_KEY nor MAILRULES_MASTER_KEY_FILE is set. The database there holds 2 stored
mailbox passwords and model keys sealed under it, and MailRules will not make a new key: …
```

Put the `master.key` from your backup into the data directory, or point `MAILRULES_MASTER_KEY_FILE` at it, and start again. If the key is gone for good, see below. A fresh install, whose database holds no passwords or keys yet, generates its key on first start as before.

MailRules makes the same check when the key is there but is the wrong one: it stops if the key opens none of the stored passwords and keys, for example because `master.key` came from another install. A key that opens only some of them is not stopped; the mailboxes it cannot open say `decrypt failed`.

## If the master key is lost

The stored mailbox passwords and model keys cannot be recovered without it. Nothing else is lost: rules, settings, activity and undo history are not encrypted.

MailRules refuses to start without the key while it holds stored passwords or keys (see [Restoring](#restoring)). To go on without the old key, start it once with `MAILRULES_NEW_MASTER_KEY=true`, or `mailrules serve --new-master-key`. It then generates a new `master.key` (or, if a key is given in the environment or a file, goes on with that one), and logs a warning. Everything stored under the old key stays unreadable. With Docker, add `-e MAILRULES_NEW_MASTER_KEY=true` to the `docker run` command; with Compose, add `MAILRULES_NEW_MASTER_KEY: "true"` to the `environment:` list in `deploy/docker-compose.yml` for that start. Set it for one start only: once the new key holds a password or key, MailRules stops asking for it, and leaving it on would let a later lost key go unnoticed.

You will see mailboxes stuck in **Reconnecting** with the error `decrypt failed`, and model keys that do not work. To recover:

1. For each mailbox, open **Mailboxes** → **Edit** and enter the app password under **New app password**. You may want to make a new app password with your provider.
2. In **Settings** → **Model API keys**, enter each key again. Keys set in the environment are not affected.

There is no command to change the master key of an existing install, so treat a lost key and a replaced key the same way.

## Keeping the key outside the data directory

To keep the key apart from the database, for example on a different disk or in a secrets store:

- `MAILRULES_MASTER_KEY_FILE` names a file holding the key. If that file is missing, MailRules refuses to start instead of making a new key, whatever the database holds.
- `MAILRULES_MASTER_KEY` holds the key itself.

Set only one of them. The key is 32 random bytes, base64-encoded, the same format as `master.key`. To move an existing install, copy the contents of `master.key` into the new file, set `MAILRULES_MASTER_KEY_FILE`, restart, and then remove `master.key` from the data directory. For a new install you can make a key with `openssl rand -base64 32`.

Commands such as `mailrules accounts add` and `mailrules accounts test` read stored passwords, so they need the same setting as the daemon.
