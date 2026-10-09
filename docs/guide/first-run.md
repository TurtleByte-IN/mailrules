---
title: First run
description: Create the admin account, choose a decision model, connect your first mailbox and learn what MailRules does next.
order: 20
---

Start MailRules (see [Install](./install.md)) and open <http://127.0.0.1:8080> in a browser on the same machine.

## Create the admin account

The first visit shows **Set up MailRules**. Enter an email address and a password of at least 12 characters, then click **Create account**.

This is the only account. It protects the web UI and the HTTP API, and it stays on this machine. Later visits show **Sign in** instead. There is no screen for changing the password yet: keep it in a password manager. If you lose it, `mailrules users reset-password` sets a new one from the machine MailRules runs on, and you can change it while signed in through the HTTP API; see [Security](./security.md#changing-or-resetting-the-password).

Right after the account is created, a short setup guide, **Welcome to MailRules**, walks you through the next two steps. You can leave it at any point with **Skip setup**, or skip a single step with **Skip for now**; everything it does can be done later from Settings and Mailboxes. The guide shows only once, in the browser that created the account, and only while no mailbox is connected.

## Choose how MailRules decides

Rules written in plain words need a decision model, an AI that reads the email and picks the rule it matches. Rules made only of conditions (sender, subject, attachment and so on) work without one.

In **Decision model**, pick one of:

| Choice | What it needs |
| --- | --- |
| Jev (recommended) | An OpenRouter API key |
| Clef | A Cloudflare account ID and API token |
| Claude Haiku 4.5 only | An Anthropic API key |
| OpenAI-compatible endpoint | An API key, a model name, and the endpoint URL unless it is OpenAI itself |
| Ollama on my server | The URL of your Ollama server and a model name |

Enter what it needs in the key fields below the choice. Whatever is still missing is shown next to the choice, and **Continue** stays disabled until it is set. Keys are encrypted with your master key and never logged.

You can skip this step: mail that only a plain-English rule could sort then waits in **Needs review** until a model is set. Keys can also come from environment variables instead of the browser. [Decision models and keys](./models.md) explains each choice, the fallback model, and what each costs.

## Connect your first mailbox

MailRules works with any IMAP mailbox that signs in with a password or app password. Connecting one takes four steps: **Provider**, **Sign in**, **Rules** and **Preview**. The same wizard opens later from **Mailboxes** → **Add mailbox**.

1. **Where is your email?** Pick your provider: iCloud Mail, Gmail, Fastmail, Yahoo Mail, Zoho Mail, or Other IMAP server.
2. **Sign in.** Enter your email address and an app password. Click **Test connection** to check it, or **Test and continue**. MailRules logs in and lists your folders; nothing is saved if the test fails. The result says how many folders it found, which special folders (Archive, Junk, Trash, Sent) it recognised, and whether the server supports push (IDLE).
3. **Start with a few rules.** Tick any starter rules you want. Newsletters, Receipts and Login codes are ticked by default; Cold sales and Travel are not. You can edit them or remove them later.
4. **Preview** lists the starter rules you picked. Click **Connect** to save the mailbox and the rules.

**Connect** does not switch dry-run off. It starts watching the mailbox; with dry-run on, which is the default, MailRules only records what it would do. See [Dry-run and going live](./dry-run.md).

### App passwords

Use an app password (a password made for one program), not the password you sign in to your provider with. Most providers refuse IMAP sign-in with the main password.

| Provider | Server | Notes |
| --- | --- | --- |
| iCloud Mail | `imap.mail.me.com`, port 993, TLS | Needs an [app-specific password](https://support.apple.com/en-us/102654). Create it at [account.apple.com](https://account.apple.com): Sign-In and Security, then App-Specific Passwords. MailRules tries your full address as the user name, then the part before the `@`. |
| Gmail, Google Workspace | `imap.gmail.com`, port 993, TLS | Pick **Gmail** for both. Needs an [app password](https://support.google.com/accounts/answer/185833), which needs 2-Step Verification. A Workspace admin can switch app passwords off. The user name is your full address. Gmail has no Archive folder, so **Archive** moves mail to All Mail, which takes it out of the inbox as Gmail's own Archive button does. Not yet tested with MailRules. |
| Fastmail | `imap.fastmail.com`, port 993, TLS | Needs an [app password](https://www.fastmail.help/hc/en-us/articles/360058752854). |
| Yahoo Mail | `imap.mail.yahoo.com`, port 993, TLS | Needs an [app password](https://help.yahoo.com/kb/SLN15241.html). The user name is your full address. |
| Zoho Mail | `imap.zoho.com` (or your region's host), port 993, TLS | Switch on [IMAP access](https://www.zoho.com/mail/help/imap-access.html) in Zoho's webmail first. The wizard asks which Zoho region your account is in, and for an app password. |
| Other IMAP server | You enter it | Choose **TLS (port 993)** or **STARTTLS (port 143)**, and the port if it differs. |
| Outlook.com, Hotmail, Microsoft 365 | — | Can't connect. Microsoft no longer accepts passwords or app passwords over IMAP, only its own sign-in, which this build does not include. |

The Zoho Mail choice in the wizard asks where your account is: United States (`imap.zoho.com`), Europe (`imap.zoho.eu`), India (`imap.zoho.in`), Australia (`imap.zoho.com.au`), Japan (`imap.zoho.jp`) or China (`imap.zoho.com.cn`). Use the one you see in the address bar when you sign in to Zoho Mail. Paid Zoho organisations use `imappro.zoho.com`: pick **Other IMAP server** and enter it, or add the mailbox from the command line with `--preset zoho --host imappro.zoho.com`.

### What the server needs to support

- **Moving mail.** MailRules moves mail with the IMAP MOVE command, or, where the server lacks it, by copying the email and removing only that one message (UIDPLUS). A server with neither cannot have mail moved safely: the connection test says so, and rules there can only flag and mark mail.
- **Push.** With IDLE, new mail is sorted as it arrives. Without it, MailRules checks for new mail once a minute.

## What happens next

Once a mailbox is connected:

- MailRules reads your Sent folder to learn who you have written to. Rules can use this ("Sender is a contact", "I've replied to sender").
- It watches the folder it was set to, `INBOX` by default, and sorts only mail that arrives from now on. Mail already in the inbox stays where it is; to sort it, use **Cleanup** (see [Undo and activity](./undo-and-activity.md#cleanup-sorting-mail-you-already-have)).
- If MailRules is stopped for a while, it catches up on mail that arrived in the meantime when it starts again.
- Mail is fetched without marking it read.

The last step of the guide, **You're set**, suggests the next move: watch **Activity** for a day while dry-run is on, then switch dry-run off. Next, write your own [rules](./rules.md).

## Managing mailboxes

**Mailboxes** lists every connected mailbox with its status: Connecting, Live, Reconnecting, Paused, Sign-in failed or Error. For each one:

- **Test** logs in once more and reports what it found, without touching the running connection.
- **Pause** stops sorting that mailbox until you click **Resume**.
- **Edit** changes its **Name**, the **Watched folder**, or the app password (**New app password**; leave it empty to keep the current one).
- **Reconnect** appears when a mailbox is not live, and starts it again.
- **Remove** deletes the mailbox from MailRules: its password, folder list, contacts, activity and undo history. Rules that apply only to it are kept but switched off, marked so you can give them another mailbox, and rules whose conditions name it are adjusted (see [When a mailbox is removed](./rules.md#when-a-mailbox-is-removed)). Nothing in the mailbox itself changes.

## Without the web UI

The admin account can only be created in the browser, or with `POST /api/auth/setup` (see the [HTTP API](https://github.com/TurtleByte-IN/mailrules/blob/main/docs/api.md)). After that, mailboxes can also be added from the command line:

```bash
mailrules accounts add --preset icloud --username you@example.com
```

`--preset` is `icloud`, `gmail`, `fastmail`, `yahoo`, `zoho` or `generic`; `generic` also needs `--host`, and optionally `--port` and `--tls implicit|starttls`. Other flags: `--label`, `--watch-folder` (default `INBOX`) and `--data-dir`. The app password is never a flag: it is read from `--password-file`, else from the environment variable `MAILRULES_ACCOUNT_PASSWORD`, else from one line typed on standard input (which is not hidden).

`mailrules accounts list` shows every mailbox, and `mailrules accounts test <id>` logs in to one (see [Troubleshooting](./troubleshooting.md#checking-a-mailbox-from-the-command-line)).

A running daemon notices a mailbox added this way within a few seconds and starts watching it; no restart is needed.
