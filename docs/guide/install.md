---
title: Install
description: Run MailRules with Docker, Docker Compose, Homebrew, a release binary or a systemd service, and upgrade it later.
order: 10
---

MailRules is one program, `mailrules`, with the web UI built in. Every release is published three ways:

- a Docker image, `ghcr.io/turtlebyte-in/mailrules`, for amd64 and arm64, tagged with the version (for example `0.1.0`, without the `v`) and `latest`;
- archives for Linux and macOS on amd64 and arm64, with a `checksums.txt`, on the [Releases page](https://github.com/TurtleByte-IN/mailrules/releases);
- a Homebrew cask in the tap [TurtleByte-IN/homebrew-tap](https://github.com/TurtleByte-IN/homebrew-tap), for macOS and Linux.

Pick one of the methods below. Whichever you choose, MailRules listens on `127.0.0.1:8080`, so open <http://127.0.0.1:8080> on the same machine to continue with [First run](./first-run.md).

## Docker

```bash
docker run -d --name mailrules --restart unless-stopped \
  --read-only --tmpfs /tmp -p 127.0.0.1:8080:8080 \
  -e MAILRULES_COOKIE_SECURE=false -v mailrules-data:/data \
  ghcr.io/turtlebyte-in/mailrules:0.1.0
```

- The image is distroless and runs as a non-root user (uid 65532). Its root filesystem can be read-only: everything MailRules writes goes to `/data`, here the volume `mailrules-data`.
- Inside the container the daemon listens on every interface, and `-p 127.0.0.1:8080:8080` publishes the port to this machine only.
- `MAILRULES_COOKIE_SECURE=false` lets you stay signed in over plain HTTP at `http://127.0.0.1:8080`. Change it to `true` once a TLS reverse proxy is in front; see [Security](./security.md#putting-mailrules-behind-a-reverse-proxy).
- Pass other settings with more `-e NAME=value` options, or with `--env-file`. All of them are in the [settings reference](./settings.md). Model keys can also be entered in the browser instead.

The image has no shell. To run a `mailrules` command against the running container, use `docker exec`:

```bash
docker exec mailrules /mailrules dry-run
```

## Docker Compose

The compose file is `deploy/docker-compose.yml` in the repository, so you need a checkout:

```bash
git clone https://github.com/TurtleByte-IN/mailrules.git
cd mailrules
```

To run the published image, put this line in `deploy/.env` and start it:

```bash
MAILRULES_IMAGE=ghcr.io/turtlebyte-in/mailrules:0.1.0
```

```bash
docker compose -f deploy/docker-compose.yml up -d
```

To build the image from the checkout instead, leave `MAILRULES_IMAGE` unset and add `--build`:

```bash
docker compose -f deploy/docker-compose.yml up -d --build
```

What the compose file sets up:

- The port is published to `127.0.0.1` only. If port 8080 is taken, put `MAILRULES_PORT=8090` (or any free port) in `deploy/.env`.
- Data lives in the `mailrules-data` volume.
- The container runs with a read-only root filesystem, no capabilities and `no-new-privileges`.
- `MAILRULES_COOKIE_SECURE` defaults to `false`, for the same reason as with `docker run`. Set it to `true` in `deploy/.env` once a TLS reverse proxy is in front.

Put your settings in `deploy/.env`, never in the compose file itself. Compose passes on only the variables listed in the `environment:` section of `deploy/docker-compose.yml`; to use a setting that is not listed there, such as `MAILRULES_PRICES_FILE`, add it to that section.

Run commands with `exec`:

```bash
docker compose -f deploy/docker-compose.yml exec mailrules /mailrules dry-run
```

## Homebrew (macOS and Linux)

```bash
brew install --cask turtlebyte-in/tap/mailrules
mailrules serve
```

The release binaries are not signed or notarized by Apple. The cask removes the macOS quarantine attribute when it installs, so macOS opens the binary without a prompt.

`mailrules serve` runs in the foreground; stop it with Ctrl-C. It keeps its data in `./data`, relative to the directory you start it from, so start it from the same directory every time, or set a fixed data directory (see [Choosing a data directory](#choosing-a-data-directory)):

```bash
MAILRULES_DATA_DIR="$HOME/.local/share/mailrules" mailrules serve
```

Homebrew does not set MailRules up as a background service. Run it in a terminal, or under a service manager of your choice.

## Single binary

Download the archive for your system and `checksums.txt` from the [Releases page](https://github.com/TurtleByte-IN/mailrules/releases). Archives are named `mailrules_<version>_<os>_<arch>.tar.gz`, where `<os>` is `linux` or `darwin` (macOS) and `<arch>` is `amd64` or `arm64`. For example, on Linux on amd64:

```bash
curl -LO https://github.com/TurtleByte-IN/mailrules/releases/download/v0.1.0/mailrules_0.1.0_linux_amd64.tar.gz
curl -LO https://github.com/TurtleByte-IN/mailrules/releases/download/v0.1.0/checksums.txt
sha256sum --ignore-missing -c checksums.txt
tar xzf mailrules_0.1.0_linux_amd64.tar.gz
./mailrules serve
```

On macOS, check the archive with `shasum -a 256 --ignore-missing -c checksums.txt` instead. The check must print `OK` for the archive you downloaded; if it does not, do not run the binary.

The archive holds the `mailrules` binary with the web UI built in, `LICENSE`, `README.md` and the systemd unit `deploy/mailrules.service`.

On macOS, a binary downloaded with a browser is quarantined, and macOS refuses to open it because it is not signed. Remove the quarantine attribute:

```bash
xattr -d com.apple.quarantine mailrules
```

As with Homebrew, the data directory is `./data` unless you set `MAILRULES_DATA_DIR`. `mailrules version` prints the version.

## Build from source

You need Go 1.27 or newer and Node.js 24 (for the web UI).

```bash
git clone https://github.com/TurtleByte-IN/mailrules.git
cd mailrules
make build
./bin/mailrules serve
```

`make build` builds the web UI and embeds it in `bin/mailrules`.

## Run as a systemd service (Linux)

`deploy/mailrules.service` is a hardened systemd unit. It runs MailRules as its own system user, keeps its data in `/var/lib/mailrules`, and listens on `127.0.0.1:8080`. From an unpacked release archive:

```bash
sudo useradd --system --home-dir /var/lib/mailrules --shell /usr/sbin/nologin mailrules
sudo install -m 0755 mailrules /usr/local/bin/mailrules
sudo install -m 0644 deploy/mailrules.service /etc/systemd/system/mailrules.service
sudo systemctl daemon-reload && sudo systemctl enable --now mailrules
```

From a source checkout, install `bin/mailrules` instead of `mailrules`.

Put settings in `/etc/mailrules/env`, one `NAME=value` per line, owned by root with mode `0600`, then `sudo systemctl restart mailrules`. The logs are in the journal: `journalctl -u mailrules`.

To run a `mailrules` command against the service's data, run it as the service user with the same data directory:

```bash
sudo -u mailrules MAILRULES_DATA_DIR=/var/lib/mailrules /usr/local/bin/mailrules dry-run
```

If `/etc/mailrules/env` sets `MAILRULES_MASTER_KEY_FILE`, pass that too to commands that read stored passwords, such as `mailrules accounts test`.

The service is reachable from the server itself only. To open the UI from your own computer, use an SSH tunnel (`ssh -L 8080:127.0.0.1:8080 your-server`, then open <http://127.0.0.1:8080>), or put a reverse proxy with TLS in front as described in [Security](./security.md#putting-mailrules-behind-a-reverse-proxy).

## Choosing a data directory

The data directory holds the database and the master key that encrypts your mailbox passwords and model keys. See [Backup and the master key](./backup.md) for what is in it.

| How you run it | Data directory |
| --- | --- |
| Docker and Docker Compose | `/data` in the container, the `mailrules-data` volume |
| systemd unit | `/var/lib/mailrules` |
| Homebrew, release binary, source build | `./data`, relative to the directory you start it from |

Set `MAILRULES_DATA_DIR` (or `--data-dir`) to choose another one. Use an absolute path when you run the binary yourself, so it does not depend on the directory you start from. The directory is created with mode `0700` if it does not exist. Keep it on a local disk: the database is SQLite in WAL mode, which does not work reliably on network file systems.

The Settings screen shows the data directory in use, in the Self-hosting card.

## Upgrading

MailRules updates its database by itself the first time a new version starts, so upgrading is: back up, replace the program, restart. Back up the data directory first (see [Backup and the master key](./backup.md)); MailRules has no command to undo a database update, so the backup is how you would go back to the older version.

**Docker.** Pull the new tag, then remove and recreate the container with the same volume:

```bash
docker pull ghcr.io/turtlebyte-in/mailrules:<new-version>
docker stop mailrules && docker rm mailrules
# then the same docker run command as before, with the new tag
```

**Docker Compose with the published image.** Change the tag in `MAILRULES_IMAGE` in `deploy/.env` (or keep `latest`), then:

```bash
docker compose -f deploy/docker-compose.yml pull
docker compose -f deploy/docker-compose.yml up -d
```

**Docker Compose from a checkout.** Update the checkout and rebuild:

```bash
git pull
docker compose -f deploy/docker-compose.yml up -d --build
```

**Homebrew.**

```bash
brew upgrade --cask mailrules
```

Then stop `mailrules serve` and start it again.

**Release binary.** Download and check the new archive as above, replace the old `mailrules` binary, and restart it.

**systemd.** Download and check the new archive, then:

```bash
sudo install -m 0755 mailrules /usr/local/bin/mailrules
sudo systemctl restart mailrules
```

If a new release changes `deploy/mailrules.service`, install the new unit as well and run `sudo systemctl daemon-reload` before the restart.
