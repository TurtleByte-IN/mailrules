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

1. **Create the admin account.** The first visit asks for an email address and a password of at least 12 characters. This is the only account; it protects the UI and the API.
2. **Set a model key.** The default decision model is Jev through OpenRouter (`OPENROUTER_API_KEY`), with Claude Haiku as the fallback and rule composer (`ANTHROPIC_API_KEY`). Until a key is set, mail that needs a model waits in Needs review; rules made only of conditions work without any model.
3. **Add a mail account.** Pick your provider and enter an app-specific password, not your main password. iCloud and Fastmail both require one.
4. **Write a rule,** watch the activity feed, and correct what it gets wrong.

Without the web UI, the same steps are `POST /api/auth/setup` and the rest of the walk-through in [`docs/api.md`](docs/api.md), or the command line: `mailrules accounts add`, `mailrules rules import`, `mailrules dry-run`.

## Dry-run, and going live

Dry-run is on by default. While it is on, every decision is recorded and shown, and no email is moved, flagged or deleted.

When the decisions look right, switch it off in Settings, or:

```bash
./bin/mailrules dry-run off      # `on` to go back; no argument shows the current state
```

With Docker: `docker compose -f deploy/docker-compose.yml exec mailrules /mailrules dry-run off`. The switch takes effect at the next email, with no restart. Once live, every action is logged and can be undone, one at a time or as a batch.

## Your data and the master key

Everything lives in the data directory (`./data`, the `mailrules-data` volume, or `/var/lib/mailrules`):

- `mailrules.db` (with its `-wal` and `-shm` files): rules, decisions, the action log, and your mailbox passwords and provider keys in encrypted form.
- `master.key`: the key that encrypts those secrets. It is generated on first run, readable only by the daemon's user, and never stored in the database.

**If you lose `master.key`, the stored mailbox passwords and provider keys cannot be recovered.** You would have to enter each of them again. Back up the data directory as a whole, and keep that backup as private as the passwords themselves: the database and the key together unlock them. To keep the key somewhere else, set `MAILRULES_MASTER_KEY_FILE`, or pass the key itself in `MAILRULES_MASTER_KEY`.

## Configuration

Every setting is an environment variable that also works as a `--flag`. The full table, with defaults, is in [`docs/backend-plan.md`](docs/backend-plan.md) under Configuration. The model, its thresholds, the provider keys and dry-run can also be changed in the browser, and what is saved there wins over the environment.

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

The product requirements are in [`docs/prd.md`](docs/prd.md), the build plan in [`docs/backend-plan.md`](docs/backend-plan.md), the HTTP API in [`docs/api.md`](docs/api.md) and the pre-release checklist in [`docs/smoke.md`](docs/smoke.md).
