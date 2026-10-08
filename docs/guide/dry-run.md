---
title: Dry-run and going live
description: How dry-run records what MailRules would do without changing your mailbox, how to check its decisions, and how to switch it off.
order: 40
---

Dry-run is on when you install MailRules. While it is on, every email is decided as usual and the decision is recorded and shown, but no email is moved, flagged, marked or trashed, and no folder is created. Mail is still fetched without marking it read.

Dry-run is one switch for the whole install. There is no separate dry-run per mailbox or per rule.

## Checking the decisions

Leave dry-run on for a day or two and look at what MailRules would have done:

- **Activity** lists every decision as it happens. Rows read **Would move to …** instead of **Moved to …**. Click a row to see why it was decided that way. See [Undo and activity](./undo-and-activity.md).
- **Needs review** holds the emails the model was unsure about. Answering them teaches MailRules, and in dry-run your answer is recorded without changing the mailbox.
- **Overview** shows where today's mail would have gone.
- To try your rules on mail you already have, use **Test on last 200 emails** on a rule (see [Rules](./rules.md#testing-a-rule-on-recent-mail)), or **Cleanup**, which with dry-run on records what it would do and moves nothing.

When a decision is wrong, fix the rule, or use **Wrong?** in Activity to say where the email belongs.

While dry-run is on, the web UI shows a banner: **Dry-run is on.** MailRules logs what it would do but doesn't touch your mailbox.

## Going live

Switch dry-run off in any of these places:

- **Go live** in the dry-run banner;
- the **Dry-run** switch at the bottom of the sidebar;
- the **Dry-run** checkbox in Settings;
- the command line:

```bash
mailrules dry-run off
```

`mailrules dry-run on` switches it back on, and `mailrules dry-run` with no argument shows the current state. The command changes the database in the data directory, so run it with the same `MAILRULES_DATA_DIR` as the daemon. With Docker, run it inside the container:

```bash
docker exec mailrules /mailrules dry-run off
docker compose -f deploy/docker-compose.yml exec mailrules /mailrules dry-run off
```

The switch takes effect from the next email, with no restart. The **Connect** button at the end of the add-mailbox wizard is something else: it saves the mailbox and starts watching it, and does not change dry-run.

To check that MailRules is live, look for the missing banner, or run `mailrules dry-run`, which prints `dry-run is off: rules change mailboxes`. The daemon also logs `dry_run` in its `listening` line at startup.

## What changes when you go live

- New mail is sorted for real: moved, flagged, marked read or trashed as the rules say. Every change is logged and can be undone for 30 days.
- Mail that arrived during dry-run is not sorted again. To sort mail already in your inbox, run **Cleanup**.
- MailRules starts learning. Sender rules are learned only from decisions made live, so a sender is still decided by the model after you go live until MailRules has seen enough of that sender's mail. See [Sender rules](./rules.md#sender-rules).
- Folders that rules move mail to are created the first time an email is moved there.

## The environment variable

`MAILRULES_DRY_RUN` (default `true`) sets dry-run only for an install where it has never been switched. Once you switch it in the browser or with `mailrules dry-run`, that choice is stored in the database and wins over the variable. To go back to dry-run, use the switch, not the variable.

Undo is not affected by dry-run: an undo always reverses changes MailRules really made, even while dry-run is on.
