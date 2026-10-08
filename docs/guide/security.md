---
title: Security
description: MailRules' safe defaults, how sign-in works, how to put it behind a reverse proxy with TLS, what is never logged, and how to report a vulnerability.
order: 90
---

## Safe by default

- **Local only.** MailRules listens on `127.0.0.1:8080`, so nothing outside the machine can reach it. The Docker setups publish the port to `127.0.0.1` only.
- **Dry-run on.** A new install changes nothing in your mailbox until you switch dry-run off. See [Dry-run and going live](./dry-run.md).
- **Mail stays unread.** Mail is fetched without marking it read.
- **Nothing is deleted.** MailRules never permanently deletes mail. Where a server cannot move mail directly, it copies the email and then removes only that one message from the old folder.
- **Everything can be undone.** Every change is logged before it is made, and can be undone for 30 days.

## Signing in

MailRules has one admin account, created on first run. It protects the web UI and the HTTP API.

- The password must be at least 12 characters. It is stored only as an Argon2id hash.
- After 5 failed sign-ins within a minute from the same address, sign-in is refused for a minute ("Too many failed sign-ins. Wait a minute and try again."). There is no permanent lockout.
- Signing in sets a session cookie, `mailrules_session`, that lasts 30 days and is renewed as you use MailRules. It is `HttpOnly` and `SameSite=Strict`. **Sign out**, at the bottom of the sidebar, ends the session on the server too.
- Every request that changes something must carry a CSRF token (the `X-CSRF-Token` header), which the web UI sends for you.
- There is no way to change or reset the admin password in this release.

Whether the cookie is marked `Secure` (sent over HTTPS only) is set by `MAILRULES_COOKIE_SECURE`:

| Value | Cookie is HTTPS-only |
| --- | --- |
| `auto` (default) | Yes, unless MailRules listens on a loopback address such as `127.0.0.1` |
| `true` | Always. Use this behind a TLS reverse proxy. |
| `false` | Never. Use this only when you open MailRules over plain HTTP on the same machine, as the Docker setups do. |

Never set it to `false` on a machine others can reach.

## Putting MailRules behind a reverse proxy

To use MailRules from other devices, put a reverse proxy that terminates TLS in front of it, such as Caddy, nginx or Traefik. Do not publish port 8080 to the internet or your network directly: your password and the session cookie would travel unencrypted.

1. Keep MailRules on a private address. With the proxy on the same machine, leave `MAILRULES_LISTEN=127.0.0.1:8080` (the default, and what the Docker setups publish). If you make MailRules listen on another address, it logs a warning at startup: `web UI is reachable from other machines; put it behind a reverse proxy with TLS`.
2. Set `MAILRULES_COOKIE_SECURE=true`. This is needed even with `auto`, because MailRules itself still listens on `127.0.0.1` and cannot see that the browser uses HTTPS.
3. Set `MAILRULES_PUBLIC_URL` to the proxy's address, such as `https://mailrules.example.com`, so links in the [summary email](./summary-email.md) work.
4. Restart MailRules.

The proxy must pass every path through to one port: the UI and the API are served together. Live updates use server-sent events on `/api/events`, which stay open; MailRules sends a keep-alive every 25 seconds and asks nginx not to buffer them, so a read timeout above 25 seconds is enough. There are no WebSockets.

MailRules sets `Content-Security-Policy`, `X-Frame-Options`, `X-Content-Type-Options` and `Referrer-Policy` on its responses; leave them as they are. It does not set `Strict-Transport-Security`; add it at the proxy if you want it.

**Caddy** (it gets a certificate by itself and passes events through without buffering):

```text
mailrules.example.com {
	@metrics path /metrics
	respond @metrics 404
	reverse_proxy 127.0.0.1:8080
}
```

**nginx:**

```nginx
server {
    listen 443 ssl;
    server_name mailrules.example.com;
    ssl_certificate     /etc/ssl/mailrules.example.com/fullchain.pem;
    ssl_certificate_key /etc/ssl/mailrules.example.com/privkey.pem;

    location = /metrics { return 404; }

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_read_timeout 1h;
    }
}
```

Behind a proxy, every request reaches MailRules from the proxy's address, so the limit of 5 failed sign-ins a minute is shared by everyone using that proxy.

### Endpoints without sign-in

`/healthz` and `/readyz` (for health checks) and `/metrics` (Prometheus metrics: request counts and timings, the version, and Go runtime figures, with no mail or account data) answer without sign-in. Keep `/metrics` off the public side of your proxy, as in the examples above.

## Secrets and logs

- Mailbox passwords and model keys entered in the browser are encrypted with AES-256-GCM under the master key before they are stored. See [Backup and the master key](./backup.md).
- They are write-only: no API response contains them. Settings only says whether a key is set, and where from.
- Logs never contain passwords, keys, tokens or the text of an email, at any `LOG_LEVEL`.
- `mailrules accounts add` never takes a password as a flag, where other users of the machine could see it in the process list.
- Mail that a model needs to read is sent to the provider of that model. [What is sent to a model](./models.md#what-is-sent-to-a-model) lists exactly what, and Ollama keeps it on your own server.

## Reporting a vulnerability

Do not open a public issue for a security problem. Report it privately through GitHub: on the repository's Security tab, choose [Report a vulnerability](https://github.com/TurtleByte-IN/mailrules/security/advisories/new). Never include passwords, API keys or the contents of your email in a report.
