---
title: Settings reference
description: Every MailRules setting, as an environment variable and as a command-line flag, with its default.
order: 110
---

Every setting is an environment variable, and also a flag of `mailrules serve`: the flag wins when both are set. The flag is the variable's name in lower case, without `MAILRULES_`, with `-` for `_`. Give keys and passwords as environment variables, not flags: other users of the machine can see a program's flags in the process list.

The decision models, their thresholds and the provider keys can also be set in the browser, under Settings. What is saved there wins over the environment; see [Decision models and keys](./models.md). Dry-run works the same way: see [Dry-run and going live](./dry-run.md).

Where to put the variables depends on how you run MailRules; see [Install](./install.md). This page is generated from the daemon's source, so it matches the release it comes with.

## Daemon

| Variable | Flag | Default | What it does |
| --- | --- | --- | --- |
| `MAILRULES_DATA_DIR` | `--data-dir` | `./data` | Directory for the database (`mailrules.db`) and the generated `master.key`. A relative path is relative to the directory MailRules starts in. |
| `MAILRULES_LISTEN` | `--listen` | `127.0.0.1:8080` | Address (`host:port`) the web UI and API listen on. Any address other than a loopback one makes MailRules log a warning: put a reverse proxy with TLS in front. |
| `MAILRULES_MODE` | `--mode` | `selfhost` | `selfhost` or `cloud`. `cloud` is for the hosted service and only hides the Self-hosting card in Settings; leave it as `selfhost`. |
| `MAILRULES_MASTER_KEY` | `--master-key` | none | The master key itself: 32 bytes, base64-encoded. It encrypts the stored mailbox passwords and provider keys. Set this or `MAILRULES_MASTER_KEY_FILE`, not both; with neither, `master.key` in the data directory is used, and generated on first run. |
| `MAILRULES_MASTER_KEY_FILE` | `--master-key-file` | none | A file holding the master key, to keep it outside the data directory. MailRules does not start if the file is missing. |
| `MAILRULES_DRY_RUN` | `--dry-run` | `true` | Dry-run until it is first switched: decisions are recorded and no mailbox is changed. Once dry-run has been switched in the browser or with `mailrules dry-run on\|off`, that choice wins. |
| `LOG_LEVEL` | `--log-level` | `info` | `debug`, `info`, `warn` or `error`. Logs are JSON lines on standard error and never hold passwords, keys or the text of an email. |

## Decision models

| Variable | Flag | Default | What it does |
| --- | --- | --- | --- |
| `MAILRULES_DECIDER` | `--decider` | `jev` | The decision model, which picks the rule an email matches: `jev`, `clef`, `anthropic`, `openai` or `ollama`. |
| `MAILRULES_DECIDER_MODEL` | `--decider-model` | none | The decision model's model name. Empty uses the provider's default; `openai` and `ollama` have none, so they need one. |
| `MAILRULES_FALLBACK_MODEL` | `--fallback-model` | `claude-haiku-4-5` | The Claude model asked for a second opinion when the decision model is unsure. It needs `ANTHROPIC_API_KEY`. Empty turns the fallback off. |
| `MAILRULES_COMPOSER_MODEL` | `--composer-model` | `claude-haiku-4-5` | The model that writes rules from your words: a Claude model, or `openai:<model>` or `ollama:<model>`. |
| `MAILRULES_ESCALATE_BELOW` | `--escalate-below` | `0.75` | When the decision model's confidence is below this (0 to 1), the fallback model is asked too. |
| `MAILRULES_MIN_CONFIDENCE` | `--min-confidence` | `0.75` | The confidence (0 to 1) a decision needs before a rule acts, unless the rule sets its own. Below it, the email waits in Needs review. |
| `MAILRULES_BODY_CHARS` | `--body-chars` | `2000` | How many characters of an email's plain text are sent to a model. |
| `MAILRULES_MODEL_CONCURRENCY` | `--model-concurrency` | `8` | How many model calls may be in flight at once. |
| `MAILRULES_PRICES_FILE` | `--prices-file` | none | A JSON file of per-model prices, in USD per million tokens, laid over the built-in table the cost estimate uses. |

## Provider keys

| Variable | Flag | Default | What it does |
| --- | --- | --- | --- |
| `OPENROUTER_API_KEY` | `--openrouter-api-key` | none | OpenRouter API key, for Jev. |
| `CLOUDFLARE_ACCOUNT_ID` | `--cloudflare-account-id` | none | Cloudflare account ID, for Clef. |
| `CLOUDFLARE_API_TOKEN` | `--cloudflare-api-token` | none | Cloudflare API token, for Clef. |
| `ANTHROPIC_API_KEY` | `--anthropic-api-key` | none | Anthropic API key, for the fallback model, for the rule composer when its model is Claude, and for the `anthropic` decision model. |
| `ANTHROPIC_WORKSPACE_ID` | `--anthropic-workspace-id` | none | The Claude workspace (`wrkspc_…`) requests run in. Only a key that covers a whole organisation needs it. |
| `OPENAI_BASE_URL` | `--openai-base-url` | none | Base URL of an OpenAI-compatible endpoint. Empty means OpenAI itself. |
| `OPENAI_API_KEY` | `--openai-api-key` | none | API key for the OpenAI-compatible endpoint. |
| `OLLAMA_URL` | `--ollama-url` | none | URL of an Ollama server, such as `http://localhost:11434`. |

## Web UI

| Variable | Flag | Default | What it does |
| --- | --- | --- | --- |
| `MAILRULES_COOKIE_SECURE` | `--cookie-secure` | `auto` | Send the login cookie over HTTPS only: `auto` (yes unless MailRules listens on loopback only), `true` or `false`. Set `true` behind a TLS proxy; `false` only when the UI is opened over plain HTTP on this machine, as with Docker. |
| `MAILRULES_PUBLIC_URL` | `--public-url` | `http://127.0.0.1:8080` | Where you open MailRules; links in the summary email start with it. Set it to your proxy's address, such as `https://mailrules.example.com`. |

## Summary email

| Variable | Flag | Default | What it does |
| --- | --- | --- | --- |
| `MAILRULES_SMTP_HOST` | `--smtp-host` | none | Outgoing mail server the summary email is sent through, such as `smtp.example.com`, without a scheme or port. |
| `MAILRULES_SMTP_PORT` | `--smtp-port` | `0` | Its port. `0` uses the usual one: 587 for `starttls`, 465 for `implicit`, 25 for `none`. |
| `MAILRULES_SMTP_USER` | `--smtp-user` | none | User name to sign in to the mail server with. Empty sends without signing in. |
| `MAILRULES_SMTP_PASSWORD` | `--smtp-password` | none | Its password; an app password where the provider has them. |
| `MAILRULES_SMTP_PASSWORD_FILE` | `--smtp-password-file` | none | A file holding that password, instead of `MAILRULES_SMTP_PASSWORD`. |
| `MAILRULES_SMTP_FROM` | `--smtp-from` | none | The summary's From address, such as `MailRules <me@example.com>`. Most servers want one of your own addresses. |
| `MAILRULES_SMTP_TLS` | `--smtp-tls` | `starttls` | `starttls` (the server must offer it), `implicit` (TLS from the start), or `none`, which is allowed only for a mail server on this machine. |
