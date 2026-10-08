---
title: Rules
description: Write rules in plain English or as conditions, choose what they do, test them on recent mail, and import or export them as YAML.
order: 30
---

A rule says which emails it is for and what to do with them. It can describe the emails in plain English ("receipts and invoices for purchases"), with conditions ("sender domain is one of shop.example.com, billing.example.net"), or both.

- A rule made only of conditions is decided on your machine, instantly and for free. No email is sent to a model.
- A rule with a plain-English description needs the decision model (see [Decision models and keys](./models.md)). If it also has conditions, the model is asked only about emails that match them.

## How an email is decided

Rules are checked in the order shown on the **Rules** screen, top to bottom. For each new email:

1. **Your own mail is left alone.** With **Leave my own emails alone** on in Settings (the default), mail sent from the mailbox's own address is never sorted, trashed or sent to a model.
2. **Sender rules come first.** If you have said what to do with this sender, or MailRules has learned it, that decides the email without a model. See [Sender rules](#sender-rules).
3. **Rules are walked in order.** Rules that are off, apply to another mailbox, have conditions the email does not match, or have an "unless" that it does match, are skipped.
4. **The first condition-only rule that matches ends the walk.** Plain-English rules above it that matched their conditions (or have none) become candidates. Plain-English rules below it are not considered.
5. **With no candidates,** that condition-only rule acts, or, if none matched, nothing happens and the email stays where it is ("No rule matched").
6. **With candidates,** the decision model reads the email and picks the one it matches, or none of them, with a confidence between 0 and 1. If it is unsure, the fallback model is asked too.
   - At or above the rule's threshold, the rule acts.
   - Below it, nothing is done and the email waits in **Needs review**.
   - If the model picks none, the condition-only rule from step 4 acts, if there was one.
7. **Stacking rules add to the result.** A condition-only rule with **Stacks** on also applies after another rule matched, for example to flag anything that fails DMARC wherever it goes.

The default threshold is 75%, set in Settings as **Act at … or above**. A rule can set its own.

An email that no rule matches is not put in Needs review; it simply stays in the inbox. Needs review is for emails a model was unsure about, and for emails that only a plain-English rule could take while no model is set up.

## Adding rules

**Add rules** in the sidebar offers three ways.

### Describe it

Write what should happen in your own words, for example "Bank statements go to Finance and mark them read. Archive LinkedIn profile-view emails." and click **Turn into rules**. The rule composer, an AI model, turns the text into one or more draft rules. Nothing is saved yet: each draft shows what it will do, whether it overlaps an existing rule, and, when a mailbox is connected, how many of your recent emails it would have matched. Rename drafts or skip the ones you do not want, then save them. Saved rules go to the bottom of the list and apply from the next email on.

The composer sees what you typed, your existing rules and your folder names, not your email. It needs a model, Claude Haiku 4.5 by default; see [The rule composer](./models.md#the-rule-composer).

When it checks drafts against your recent mail, it reads the newest 200 emails of the mailbox without marking them read. For drafts with a plain-English description it asks the decision model about each email that passes the conditions, and those calls are counted in **Usage**.

### Build with conditions

Build a rule field by field:

- **Rule name**.
- **Conditions (optional)**, matched when the email meets **all** or **any** of them. Click **Add condition** for another row.
- **And the email is about**, a plain-English description checked by the decision model. Leave it empty for a condition-only rule.
- **Except when I've replied to the sender before**.
- **Then**: what to do (see [Actions](#actions)).
- **More options**: which mailbox the rule applies to, whether it stacks, and its threshold.

The panel on the right shows the rule **In plain words** and whether it needs a model. New rules go to the bottom of the list.

### Templates

Ready-made rules: Newsletters, Receipts, Login codes, Cold sales, Recruiters, Travel, Social notifications, Bank statements and Calendar invites. Each card says whether it is condition-only or needs the model, and what it does. **Add rule** saves it at the bottom of the list, switched on. Folders it moves mail to are created in your connected mailboxes; with dry-run on, a folder is created only when the first email is really moved there.

The mailbox wizard offers five of these as starter rules.

Cold sales trashes pitches at 90% confidence or above, but not from a sender you have replied to before, as with Recruiters.

## Conditions

The builder offers these fields:

| Field in the builder | YAML field | Operators in the builder |
| --- | --- | --- |
| Sender address | `from` | is any of, contains any of, matches pattern |
| Sender domain | `from_domain` | is any of, contains any of, matches pattern |
| Sent to (alias) | `to` | is any of, contains any of, matches pattern |
| Subject | `subject` | is any of, contains any of, matches pattern |
| Body text | `body` | is any of, contains any of, matches pattern |
| Mailing list (List-Id) | `list_id` | is any of, contains any of, matches pattern |
| Has an attachment | `has_attachment` | is yes, is no |
| Attachment type | `attachment_ext` | is any of, contains any of, matches pattern |
| Size (KB) | `size_kb` | is more than, is less than |
| Sender is a contact | `is_contact` | is yes, is no |
| I've replied to sender | `replied_before` | is yes, is no |
| Bulk or newsletter | `is_bulk` | is yes, is no |
| DMARC result | `dmarc` | is any of |

How they match:

- Text comparisons ignore case. Several values are separated by commas in the builder, or written as a list in YAML; the condition matches if any value does.
- **Sender domain** also matches subdomains: `example.com` matches mail from `news.example.com`.
- **Body text** searches the first 2000 characters of the email's plain text (the `MAILRULES_BODY_CHARS` setting).
- **Matches pattern** is a regular expression (RE2 syntax, case-insensitive, at most 200 characters).
- **Attachment type** is the file extension without the dot, such as `pdf` or `ics`.
- **Sender is a contact** and **I've replied to sender** are both true when you have sent mail to the sender's address. MailRules learns this from your Sent folder.
- **Bulk or newsletter** is true when the email has a `List-Unsubscribe` header or a `Precedence` of `bulk` or `list`.
- **DMARC result** is `pass`, `fail` or `none`, as your mail server recorded it.

YAML and the HTTP API can do more than the builder:

- More fields: `cc`, `delivered_to`, `is_noreply` (the sender's address is a no-reply one), `age_days` (days since the email arrived), `account` (a mailbox's number), and `header:<Name>` for any header, such as `header:X-Mailer`.
- More operators. Text fields take `eq`, `ne`, `in`, `contains`, `contains_any`, `not_contains`, `matches` and `exists`. Yes/no fields take `eq` and `ne`. Numbers take `eq`, `ne`, `gt` and `lt`. `dmarc` and `account` take `eq`, `ne` and `in`.
- Nested `all` and `any` groups. There is no `not`; use `ne`, `not_contains`, `exists: false`, or the rule's `unless`.

Editing such a rule with **Edit conditions** in the builder can lose what the builder cannot show, so keep those rules in YAML.

## Actions

| In the builder | YAML | What it does |
| --- | --- | --- |
| Move to folder | `move:<folder>` | Moves the email to that folder, creating it if needed. |
| Archive | `archive` | Moves it to the folder your server marks as Archive. |
| Move to Trash | `trash` | Moves it to `MailRules Trash`, or to your real Trash (see below). |
| Keep in Inbox | `keep` | Leaves it where it is, and records that a rule took it. |
| Keep in Inbox and flag | `keep`, `flag` | Leaves it and flags it. |
| Also mark as read | `read` | Marks it read. |
| (YAML only) | `junk` | Moves it to the folder your server marks as Junk. |
| (YAML only) | `flag`, `unflag`, `unread` | Sets or clears the flag, or marks it unread. |

Actions run in the order listed. If one fails, the rest are not tried, and the failure shows in Activity.

- **Archive** and **junk** use only the folder your server marks with that role. MailRules never guesses or creates one; if your server has none, the action fails. Use a move to a named folder instead.
- **Trash** goes to a folder named `MailRules Trash` by default, created the first time it is needed, not to your mailbox's Trash. Providers empty Trash on their own (iCloud after 30 days), and a wrongly trashed email could be gone before you notice. MailRules never empties `MailRules Trash`; empty it yourself when you like. To use the real Trash, untick **Send trashed mail to MailRules Trash** in Settings.
- A rule that trashes based on its plain-English description needs a threshold of at least 85%. When you add one in the UI without a threshold, it gets 90%.
- Nothing is ever deleted permanently. Moves use IMAP MOVE where the server has it; otherwise MailRules copies the email and removes only that one message from the old folder.

## Managing rules

The **Rules** screen lists every rule in the order it is checked, with what it does, how often it matched this week, and when it last matched.

- Drag a rule, or use its up and down buttons, to change the order.
- Untick **On** to pause a rule without deleting it.
- Click a rule to edit it:
  - **Name**, and the plain-English description under **When the email is about**.
  - **Applies to**: **All mailboxes**, or one mailbox.
  - **Stacks: also applies after another rule matched** (condition-only rules).
  - **Model**: **Default**, or a specific decision model for this rule: Jev, Clef, Claude Haiku 4.5, **OpenAI-compatible…** (`openai:<model>`), **Ollama…** (`ollama:<model>`) or **Other (name:model)…**. The last three ask for the model name and save it when you click **Set model**; MailRules shows its own message if the value is not accepted. A model set another way, such as in an imported YAML file, is shown as it is and is not changed unless you set another. The key or URL for that model must be set too. When several candidate rules name a model, the highest one in the list decides which model is asked.
  - **Act when sure above**: this rule's threshold, from 50% to 99%. It applies only to rules with a description.
  - **Edit conditions** opens the rule in the builder.
  - **Rewrite with AI**: say what should change ("also skip anything from my bank"), compare **Current rule** and **New draft**, then **Update rule** or **Discard**.
  - **Undo what it did today** puts back every email this rule moved today (see [Undo and activity](./undo-and-activity.md)).
  - **Delete rule**. Past decisions stay in Activity under the old name.

## Testing a rule on recent mail

Under a rule, or in the builder, **Test on last 200 emails** runs the rule over the newest emails in your inbox and reports how many it would match, and how many would go to the decision model or to Needs review. Change the number to test more or fewer, up to 2000.

A test changes nothing: it reads mail without marking it read, and dry-run does not matter. Rules with a description do call the decision model for each email that passes their conditions, and those calls are counted in **Usage**. A test needs a connected mailbox, and for rules with a description, a decision model.

To see what your rules (all of them, or only some) would do to mail that is already in a folder, on one mailbox or several, and then do it, use **Cleanup** (see [Undo and activity](./undo-and-activity.md#cleanup-sorting-mail-you-already-have)).

## Sender rules

A sender rule says what to do with all mail from one address, or one domain, before any other rule is looked at. It never asks a model.

**Senders** lists who emailed you in the last 30 days, with how many emails. For each sender choose:

- **Let my rules decide** (the default);
- **Always keep in Inbox**;
- **Always: \<rule\>**, to send all their mail where that rule sends it;
- **Always trash**.

**Learned from your corrections and consistent decisions** lists the sender rules MailRules made by itself. **Forget** removes one.

MailRules learns a sender rule for an address when the model has sent that sender's last three emails to the same rule, each with at least 90% confidence, and you corrected none of them. For bulk mail that passed DMARC, one such decision is enough. It learns only from decisions made with dry-run off. Correcting an email from a sender removes what was learned about that sender.

You can also make a sender rule when you correct an email or answer Needs review: tick **Always do this for …** and choose **Everyone at \<domain\>** or **Only \<address\>**. A rule for a whole domain is refused for public email providers, where anyone can have an address.

## Import and export as YAML

Rules can be kept in a YAML file: to back them up, edit them in a text editor, or copy them to another install. On the **Rules** screen, **Export rules** downloads `mailrules-rules.yaml` and **Import rules** reads one.

```yaml
defaults:
  decision_model: jev
  fallback_model: claude-haiku-4-5
  min_confidence: 0.75
rules:
  - id: Food
    said: "Put everything from the food delivery apps in Food"
    match:
      from_domain: [orders.example.com, delivery.example.net]
    actions: ["move:Food"]
  - id: Recruiters
    when: "Recruiter outreach about job openings"
    match:
      is_bulk: false
    unless:
      replied_before: true
    actions: ["move:Jobs"]
  - id: Newsletters
    match:
      any:
        - {field: list_id, op: exists}
        - {field: subject, op: matches, value: '^issue \d+'}
    actions: ["move:Reading", read]
  - id: Flag failed DMARC
    match:
      dmarc: fail
    actions: [flag]
    stack: true
  - id: Scams
    when: "Phishing, scams or fake bank alerts"
    actions: [trash]
    min_confidence: 0.9
```

Each rule has:

| Key | Meaning |
| --- | --- |
| `id` | The rule's name. Required and unique. |
| `when` | The plain-English description the decision model checks. |
| `match` | Conditions. |
| `unless` | Conditions that exclude an email. |
| `actions` | Required. A list such as `["move:Food", read]`; a folder name with a colon must be quoted. `{type: move, folder: "Money: 2026"}` works too. |
| `min_confidence` | The rule's threshold, from 0 to 1. |
| `model` | The decision model for this rule, such as `jev`, `clef`, `anthropic`, or `ollama:<model>`. |
| `applies_to` | The address (login) of the mailbox the rule is limited to, as shown under **Applies to** in the rule's options. Left out, the rule applies to every mailbox. See below. |
| `stack` | `true` for a stacking rule. Stacking rules must be condition-only. |
| `enabled` | `false` to import the rule switched off. |
| `said` | Your original wording, shown under "You said". |
| `template` | The template the rule came from. |

`match` and `unless` take either a short form, where each key is a field and every entry must hold (a single value means equals, a list means any of), or a full tree of `all` and `any` groups whose entries are `{field, op, value}`.

The order of the rules in the file is the order they are checked.

The `defaults` block holds three optional keys:

- `min_confidence` applies to every rule in the file that sets none. An export does not write it: it writes each rule's own threshold, and leaves a rule without one to follow Settings.
- `decision_model` and `fallback_model` say which models the rules were written for. An import never changes them, because the models are settings of the install, and a file passed around must not change which paid model someone uses. An export writes the models in force. An import accepts a file whose models are the ones in force here (`decision_model: jev` also matches `jev` with any model of its own), and refuses one that names other models, saying which, so nothing is saved until you change the model in Settings or delete that line from the file.

`applies_to` names a mailbox by its address, not its number, so a file works on another install. The address is the account's username, matched ignoring case. An import refuses a file when a rule's mailbox is not connected here, so a rule never quietly starts acting on every mailbox: add the mailbox first. A file that says nothing leaves a rule's mailbox as it is, so files from before this key existed are still importable. To widen a rule from a file, write `applies_to: all`.

How an import works:

- It is all or nothing. If any rule is invalid, nothing is saved, and every problem is listed.
- A rule whose name already exists is replaced. It keeps its place in the list; whether it is on comes from the file. It keeps its mailbox unless the file has `applies_to`.
- Rules with new names are added at the bottom, for the mailbox in `applies_to` or, without it, all mailboxes.
- Rules that are not in the file are left alone. An import never deletes a rule.
- A file can be at most 1 MB.

An export writes `applies_to` for every rule that is limited to a mailbox, and the models in force under `defaults`.

### From the command line

```bash
mailrules rules export rules.yaml      # without a file name, it prints to standard output
mailrules rules import rules.yaml
mailrules rules validate rules.yaml
mailrules rules test rules.yaml --eml ./samples
```

- `export` and `import` work on the database in the data directory (`MAILRULES_DATA_DIR`), and need the admin account to exist. A running daemon uses imported rules from the next email on.
- `validate` checks a file without touching the database: it prints `rules.yaml: 5 rules ok`, or each problem with the rule's number and name.
- `test` runs the rules in a file over every `.eml` file directly inside a directory, with conditions only. It never asks a model and uses no database, so it prints which rule each email would go to, `needs a model` with the rules the model would choose between, or `no rule matches`. Attachments are not known from a `.eml` file, so `has_attachment` and `attachment_ext` never match, and `is_contact` and `replied_before` are always false.

With Docker, run these inside the container, for example `docker exec mailrules /mailrules rules export`; see [Install](./install.md) for the other setups.
