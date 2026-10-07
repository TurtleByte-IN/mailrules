# MailRules

MailRules is a small daemon that sorts incoming email over IMAP, in real time, using rules you write in plain English or as structured conditions. It works with any IMAP provider (iCloud, Fastmail, Yahoo, Zoho, your own server), lets you choose the model that decides, and runs as one binary with the web UI built in.

It starts safe: it listens on this machine only, and dry-run is on, so it records what it would do and changes no mailbox until you switch dry-run off.

> **Status.** Nothing is released yet: there are no downloadable binaries, no published Docker image and no Homebrew package. Build from source as shown below. The web UI is developed in `web/`; a build from a checkout without that directory still runs the daemon, the HTTP API and the command line, and its web page says the UI is not built in.

## Quickstart with Docker

You need Docker with Compose, and a checkout of this repository.

```bash
docker compose -f deploy/docker-compose.yml up -d --build
```

Open <http://127.0.0.1:8080> on the same machine. The image is distroless, runs as a non-root user with a read-only root filesystem, and keeps everything it writes in the `mailrules-data` volume.

Settings are environment variables. Put the ones you need in `deploy/.env` (never in the compose file), for example a model key; or leave them out and enter the keys in the browser.

Inside the container the daemon listens on every interface, and Compose publishes the port to `127.0.0.1` only, over plain HTTP. The Compose file therefore sets `MAILRULES_COOKIE_SECURE=false` so you can stay signed in at `http://127.0.0.1:8080`. On a server (a VPS such as Hetzner), put a TLS reverse proxy in front and set `MAILRULES_COOKIE_SECURE=true` in `.env`, so the login cookie only ever travels over HTTPS.

## Quickstart with a single binary

You need Go 1.27 or newer, and Node.js 24 if the checkout has the `web/` directory.

```bash
make build
./bin/mailrules serve
```

Open <http://127.0.0.1:8080>. The data directory is `./data` unless you set `MAILRULES_DATA_DIR`.

To run it as a service on Linux, `deploy/mailrules.service` is a hardened systemd unit; the steps to install it are at the top of that file. It keeps its data in `/var/lib/mailrules`.

## First run

1. **Create the admin account.** The first visit asks for an email address and a password of at least 12 characters. This is the only account; it protects the UI and the API. A short setup guide follows with the next two steps. Either can be skipped, and Skip setup leaves the guide for Overview at any point. It shows once, in the browser that created the account, and only while no mailbox is connected.
2. **Choose the AI.** Pick the decision model and give it what it needs: an OpenRouter key for Jev, a Cloudflare account ID and API token for Clef, an Anthropic key for Claude, an endpoint URL and key for an OpenAI-compatible server, or the URL of your Ollama server. Whatever is still missing is shown beside the choice. The same controls are in Settings, and the keys can also come from the environment (`OPENROUTER_API_KEY` and the rest; see Configuration). Claude Haiku is the fallback and the rule composer by default (`ANTHROPIC_API_KEY`). The rule composer, which writes rules from plain English and suggests them from your mail, can run on an OpenAI-compatible endpoint or a local Ollama server instead: set its model to `openai:<model>` or `ollama:<model>` in Settings. Rules made only of conditions work without any model, wherever they sit in the order. Until a key is set, rules that need a model are passed over, and mail that only such a rule could take waits in Needs review.

   A Claude key made for your whole organisation, rather than for one workspace, must name the workspace each request runs in. MailRules looks it up when you save the key: with one workspace it uses it, with several Settings lists them under the key to pick from. If your key may not list workspaces, copy the ID (`wrkspc_…`) from the ID column of [Settings → Workspaces](https://platform.claude.com/settings/workspaces) in the Claude Console into "Anthropic workspace", or set `ANTHROPIC_WORKSPACE_ID`. A key made for one workspace needs nothing.
3. **Connect a mailbox.** Pick your provider and enter an app-specific password, not your main password. iCloud and Fastmail both require one. The connection is tested before anything is saved; then pick a few starter rules. Dry-run stays on throughout. Mailboxes → Add mailbox does the same later.
4. **Write a rule,** watch the activity feed, and correct what it gets wrong.

Without the web UI, the same steps are `POST /api/auth/setup` and the rest of the walk-through in [`docs/api.md`](docs/api.md), or the command line: `mailrules accounts add`, `mailrules rules import`, `mailrules dry-run`.

## Dry-run, and going live

Dry-run is on by default. While it is on, every decision is recorded and shown, and no email is moved, flagged or deleted. Nothing is learned from those decisions either: MailRules learns a sender rule only from decisions it made live, so a sender is still decided by the model after you go live until it has seen enough of that sender's mail.

When the decisions look right, switch it off in Settings, or:

```bash
./bin/mailrules dry-run off      # `on` to go back; no argument shows the current state
```

With Docker: `docker compose -f deploy/docker-compose.yml exec mailrules /mailrules dry-run off`. The switch takes effect at the next email, with no restart. Once live, every action is logged and can be undone, one at a time or as a batch.

Mail a rule trashes goes to a folder named `MailRules Trash`, made the first time it is needed, not to your mailbox's Trash: providers empty Trash on their own (iCloud after 30 days), and a wrongly trashed email could be gone before you notice. Empty `MailRules Trash` yourself when you like. To use the real Trash instead, untick "Send trashed mail to MailRules Trash" in Settings. Junk still goes to the real Junk folder.

An email you move back into the inbox yourself, from a folder a rule put it in or from `MailRules Trash`, stays there: MailRules knows it by its Message-ID and does not sort it again. An email waiting in Needs review that you move out and back is still listed there once.

## Your data and the master key

Everything lives in the data directory (`./data`, the `mailrules-data` volume, or `/var/lib/mailrules`):

- `mailrules.db` (with its `-wal` and `-shm` files): rules, decisions, the action log, and your mailbox passwords and provider keys in encrypted form.
- `master.key`: the key that encrypts those secrets. It is generated on first run, readable only by the daemon's user, and never stored in the database.

**If you lose `master.key`, the stored mailbox passwords and provider keys cannot be recovered.** You would have to enter each of them again. Back up the data directory as a whole, and keep that backup as private as the passwords themselves: the database and the key together unlock them. To keep the key somewhere else, set `MAILRULES_MASTER_KEY_FILE`, or pass the key itself in `MAILRULES_MASTER_KEY`.

## Configuration

Every setting is an environment variable that also works as a `--flag` (the variable name in lower case, without `MAILRULES_` and with `-` for `_`). The model, its thresholds, the provider keys and dry-run can also be changed in the browser, and what is saved there wins over the environment.

| Variable | Default | What it does |
| --- | --- | --- |
| `MAILRULES_DATA_DIR` | `./data` | Where the database and the generated master key live |
| `MAILRULES_LISTEN` | `127.0.0.1:8080` | HTTP address; anything non-local logs a warning |
| `MAILRULES_MASTER_KEY` / `MAILRULES_MASTER_KEY_FILE` | generated into the data directory on first run | 32-byte base64 key that encrypts stored passwords and keys |
| `MAILRULES_DRY_RUN` | `true` | Dry-run until it is switched in the browser or with `mailrules dry-run on\|off` |
| `MAILRULES_DECIDER` | `jev` | Decision model: `jev`, `clef`, `anthropic`, `openai` or `ollama` |
| `MAILRULES_DECIDER_MODEL` | provider's default | The decider's model; `openai` and `ollama` need one |
| `MAILRULES_FALLBACK_MODEL` | `claude-haiku-4-5` | Asked when the decider is unsure; empty turns it off |
| `MAILRULES_COMPOSER_MODEL` | `claude-haiku-4-5` | Writes rules from your words: a Claude model, or `openai:<model>` or `ollama:<model>` |
| `MAILRULES_ESCALATE_BELOW` | `0.75` | Decider confidence below which the fallback is asked |
| `MAILRULES_MIN_CONFIDENCE` | `0.75` | Default confidence a rule needs to act; each rule can set its own |
| `MAILRULES_BODY_CHARS` | `2000` | Characters of an email's text sent to a model |
| `MAILRULES_MODEL_CONCURRENCY` | `8` | Model calls in flight at once |
| `OPENROUTER_API_KEY` |  | Jev, through OpenRouter |
| `CLOUDFLARE_ACCOUNT_ID`, `CLOUDFLARE_API_TOKEN` |  | Clef |
| `ANTHROPIC_API_KEY` |  | Claude, for the fallback and the composer |
| `ANTHROPIC_WORKSPACE_ID` |  | The Claude workspace (`wrkspc_…`), needed only for a key that covers a whole organisation |
| `OPENAI_BASE_URL`, `OPENAI_API_KEY` |  | Any OpenAI-compatible endpoint |
| `OLLAMA_URL` |  | Local models through Ollama |
| `MAILRULES_PRICES_FILE` | built-in table | JSON file of per-model prices in USD per million tokens, laid over the built-in table, for the cost estimate |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`; logs are JSON lines on stderr and never contain mail text or keys |
| `MAILRULES_COOKIE_SECURE` | `auto` | Mark login cookies HTTPS-only: `auto` (yes unless listening on localhost), `true` or `false` |

## Security

- **Local by default.** The daemon listens on `127.0.0.1:8080`. Nothing outside the machine can reach it.
- **Before you expose it,** put a reverse proxy that terminates TLS in front (Caddy, nginx, Traefik) and leave MailRules on a private address behind it. Do not publish port 8080 to the internet directly: the session cookie and your admin password would travel in the clear. Listening on a non-local address makes the daemon log a warning and, unless `MAILRULES_COOKIE_SECURE` says otherwise, mark its cookies `Secure`, so the browser sends them over HTTPS only. Never set it to `false` on a machine others can reach.
- **Mail is fetched without marking it read,** and nothing is ever permanently deleted except the exact messages MailRules has just moved.
- **Secrets stay put.** Passwords and keys are encrypted at rest, never returned by the API and never logged.
- `/healthz`, `/readyz` and `/metrics` need no sign-in. Keep `/metrics` off the public side of your proxy.

## Release channels (planned)

Tagged releases with binaries for Linux and macOS (amd64 and arm64), a published Docker image and a Homebrew tap are planned. `.goreleaser.yaml` is the release configuration; no release has been cut with it. Until one has, build from source.

## Developing

- `make check`: gofmt, go vet, golangci-lint and `go test -race ./...`
- `make dev`: the daemon, plus the Vite dev server when `web/` exists
- `make build`: builds the web app when `web/` exists, then the single binary

The HTTP API is described in [`docs/api.md`](docs/api.md) and the pre-release checklist is [`docs/smoke.md`](docs/smoke.md).

## License

Copyright (C) 2026 TurtleByte.

MailRules is free software: you can redistribute it and/or modify it under the terms of the GNU Affero General Public License, version 3 only, as published by the Free Software Foundation. It is distributed in the hope that it will be useful, but without any warranty; without even the implied warranty of merchantability or fitness for a particular purpose. See [`LICENSE`](LICENSE) for the full text.

If you run a modified MailRules where other people use it over a network, the licence requires you to offer them your modified source. The web UI links to the source from the sidebar ("Source code"); point that link at your own copy.

The web UI bundles the Schibsted Grotesk and IBM Plex Mono fonts, which are licensed under the SIL Open Font License 1.1. Every Go dependency, and every npm package shipped in the web UI, is under MIT, BSD, ISC or Apache 2.0.
