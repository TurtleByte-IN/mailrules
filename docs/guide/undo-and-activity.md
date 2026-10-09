---
title: Undo and activity
description: See every decision MailRules makes, correct the wrong ones, answer Needs review, and undo one email, the last hour, a rule's day or a whole batch.
order: 60
---

MailRules records every decision and every change it makes to your mailbox, before it makes the change. Anything it did can be undone for 30 days.

## Activity

**Activity** lists every decision, newest first, live as mail arrives. The tiles at the top show today's numbers: **Acted on today**, **Needs review**, **Decided without a model** and **Model cost today**.

Filter the list by **Rule**, **Mailbox** and **Outcome** (Sorted, Left in Inbox, Needs review, Trashed). Each row shows the time, sender, subject, why it was decided that way, which rule took it, and how:

- **condition · free**: the rule's conditions decided it;
- **sender · learned**: a sender rule decided it;
- **you · reviewed** or **you · corrected**: you decided it;
- a model name and a confidence: the decision model or the fallback decided it.

The outcome reads, for example, **Moved to Receipts**, **Kept in Inbox**, or, in dry-run, **Would move to Receipts**. A failed action shows its reason, such as **Failed: no Archive folder**.

Click a row to open **Why this happened**: each step of the decision, the model and its confidence for every candidate rule, the tokens and cost of each model call, and a short preview of the email. The preview is kept for 30 days by default; the label above it shows the number of days set under **Keep email snippets for** in Settings (from 1 to 3650). Full email bodies are never stored.

## Correcting a decision

When an email went to the wrong place, click **Wrong?** on its row, or use **Wrong call? Put it where it belongs** in its details. Choose **Keep in Inbox** or the rule it should have gone to, and click **Fix it**. MailRules:

1. undoes what it did to that email;
2. applies the chosen rule's actions, or leaves it in the inbox;
3. keeps the email as an example, which the fallback model sees next time a similar email arrives;
4. forgets any sender rule it had learned for that sender.

Tick **Always do this for …** to make a sender rule at the same time, for **Everyone at \<domain\>** or **Only \<address\>**. Future mail from that sender then goes straight to that rule, without a model.

## Needs review

**Needs review** holds the emails MailRules did not act on because it was not sure. They are still in your inbox, untouched. An email lands here when:

- the model's confidence was below the rule's threshold;
- only a plain-English rule could take it, and no decision model is set up ("No decision model is set");
- the model could not be reached for about half an hour ("Gave up after … retries");
- the email could not be read.

Each card shows the sender, subject, a snippet, and, when a model was asked, its **Best guess** with a confidence. Answer with **Yes, \<rule\>**, **Keep in Inbox**, or pick another rule and **Apply**. **Always do this for …** works as when correcting. Each answer is recorded like a correction and teaches MailRules. **Create a rule for emails like this** opens **Add rules**.

An email you move out of the inbox yourself is dropped from the queue when you answer it.

## Undoing

Undo puts an email back in the folder it came from and restores its flags and read state. Dry-run does not stop an undo: it only reverses changes MailRules really made, even while dry-run is on.

| What to undo | Where |
| --- | --- |
| One email | **Undo** on its row in Activity. No confirmation. |
| Everything in the last hour | **Undo the last hour** at the top of Activity, then **Yes, undo the last hour**. |
| Everything one rule did today | **Undo what it did today** on the rule in Rules, then **Yes, undo today**. |
| A Cleanup run | **Undo batch** under **Batches you can undo** in Cleanup, then **Yes, undo this batch**. For a run over several mailboxes, **Undo the whole run**, then **Yes, undo this run**, undoes every mailbox of it. |

An undo can fail for one email and still work for the rest; the message says how many could not be undone. An email cannot be undone when:

- it was done more than 30 days ago;
- the email was moved or deleted outside MailRules since;
- its mailbox is not connected right now (try again once it is live);
- it was only recorded in dry-run, so there is nothing to put back.

### Trashed mail

Rules that trash mail move it to `MailRules Trash`, a normal folder that MailRules never empties, unless you unticked **Send trashed mail to MailRules Trash** in Settings. To restore a trashed email, undo it in Activity (filter **Outcome** by **Trashed**), or move it back yourself in any mail app.

An email you move back into the inbox yourself, from a folder a rule put it in or from `MailRules Trash`, stays there: MailRules recognises it by its Message-ID and does not sort it again.

## Cleanup: sorting mail you already have

MailRules sorts only mail that arrives after a mailbox is connected. **Cleanup** applies your rules to mail already in the Inbox or Archive folder, on any mix of mailboxes and rules, in two steps:

1. **Check what would move.** Choose the **Mailboxes** (one, some, or **All mailboxes**; shown when you have more than one), the **Rules** (**Every rule**, or **Only some rules** with the ones to check ticked; shown when you have more than one rule), the **Folder**, and **Which emails**: the newest emails, those from the last few days, or all mail, up to the newest 2000 of the range. With several mailboxes, mail is taken from each Inbox. The check reads each email without marking it read and decides it as live mail would be decided, including asking the decision model, which costs money and is counted in **Usage**. Nothing moves. The check runs in the background, so you can close the tab, but it is lost if MailRules restarts. With several mailboxes each one is checked on its own and gets a line showing its progress; **Show** puts that mailbox's chart and list on screen.
2. **Sort.** Review the result per rule, untick emails you want left alone with **Choose emails**, and click **Sort N emails**. Sort does what the check found, without asking the model again, oldest first, as one batch for each mailbox. Emails that moved since the check are skipped. If your rules changed since the check, check again first.

Choosing only some rules limits the check to them. The rules you pick are checked in their usual order, so an email that a rule you left out would have moved is not touched by it, and a picked rule further down gets its turn. Sender rules, your own "always keep", "always move to" and "always trash" answers, apply whatever you pick, and a sender rule that sends a sender's mail to a rule you left out still does. A check made with some rules is only spoiled by a change to one of those rules, or to a sender rule.

Only one Sort can run for a mailbox at a time. If one mailbox of a run is already being sorted, the whole run is refused and nothing starts.

Sort respects dry-run: with dry-run on, it records what it would do and moves nothing. Every run is listed under **Batches you can undo**, kept for 30 days. A run over several mailboxes is shown as one card with a line for each mailbox: **Undo batch** on a line puts that mailbox's emails back where they were, and **Undo the whole run** does it for every mailbox that can still be undone.

## How long things are kept

- Undo works for 30 days.
- Email snippets are cleared after 30 days by default (**Keep email snippets for** in Settings).
- Decisions and the action log are deleted after 180 days, except actions that can still be undone.
