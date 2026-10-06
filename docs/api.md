# MailRules HTTP API

How to talk to the daemon over HTTP: sign in, change settings, manage rules, read the activity feed, fix what a rule got wrong, and follow live events.

The contract is `api/openapi.yaml` (OpenAPI 3.1). It lists every endpoint with its request and response schemas, its error codes and the event payloads; this page does not repeat it. A test fails when the daemon serves a route the contract does not list, or the reverse, and when a response has a field the contract does not define.

## The rules of the road

- **Local by default.** The daemon listens on `127.0.0.1:8080` (`MAILRULES_LISTEN`).
- **Session cookie.** `POST /api/auth/setup` (first run) or `POST /api/auth/login` sets `mailrules_session`. Every route needs it except those two, `logout`, `me`, `/healthz`, `/readyz` and `/metrics`. Without it the answer is 401 with code `setup_required` (no admin account yet) or `unauthenticated`.
- **CSRF header.** Every response hands out a `mailrules_csrf` cookie. Every request that is not a GET must send its value back in the `X-CSRF-Token` header, or it is answered 403 `csrf_failed`. Call `GET /api/auth/me` first to receive the cookie.
- **JSON in, JSON out.** Unknown fields are rejected (400 `invalid_json`). Timestamps are unix seconds, ids are integers, and an absent time or reference is `null`.
- **Errors** are `{"error": {"code": "...", "message": "...", "path": "..."}}`. `code` is stable; `message` is safe to show; `path` names the field when there is one, for example `conditions.all[0].op` for a rule.
- **Pagination.** `GET /api/activity` and `GET /api/review` take `?cursor=&limit=` (limit 1 to 100, default 50) and return `next_cursor`; `null` means the end.
- **Secrets are write-only.** No response contains an account password or a provider key. `GET /api/settings` says only which keys are set. The only message text ever returned is `snippet`, at most 200 characters.
- **Dry-run is on by default.** Until `PATCH /api/settings {"dry_run": false}`, decisions are recorded and no mailbox is changed.

## What is not built yet

Operations marked `x-status: planned` in the contract belong to a later backend milestone: senders, cleanup, templates and stats (M9). They are served so the contract and the route table stay equal, and answer 501 with code `not_implemented` after the session and CSRF checks.

## Creating, composing and testing rules

- **Create.** `POST /api/rules/batch` saves one rule or many in one transaction: a rule built by hand from conditions, a template, or composer drafts the user approved. It needs no model. Every rule is validated first, so one bad rule saves none; the error's `path` is `rules[1].conditions.all[0].op`. `new_folders` are created on the mail server, except in dry-run and on an offline account, where the first live move creates the folder. `position` drops a rule into the priority order (0 is first); without it the rule goes last.
- **Compose.** `POST /api/rules/compose` turns free text into draft rules and saves nothing. Each draft is a card: the rule, `new_folders`, at most one `question`, `conflicts` with existing rules, `errors`, and `match_count` with up to five `samples` from the account's last 200 emails. A draft with `errors` cannot be saved as it is; the request still answers 200. Send the cards the user approves to `/api/rules/batch`.
- **Re-optimize.** `POST /api/rules/{id}/compose` returns one draft to replace a rule, from its original wording plus new text. Save it with `PATCH /api/rules/{id}`.
- **Test.** `POST /api/rules/test` runs rules over an account's newest mail and reports, per email, the rule, stage, confidence, reason and the actions it would take. It only reads: mail stays unread and where it is, and nothing is recorded but the model calls. `rule_ids` tests those saved rules on their own (switched on, without the sender rules); `rules` tests drafts as if saved after the others; with neither, the saved rule set runs as it is. Up to `limit` 200 the answer is one JSON body. Above that (to 2,000) it is an event stream: `progress` events (`{"done", "total"}`, one per 25 emails), then one `done` event with the result, or one `error` event if the run fails midway.
- **Models.** The composer needs a generative model, and testing a rule that has an intent needs a decision model. With none set the answer is 409 `no_composer_model`, with a message to show; condition-only rules are created and tested without any model. A model that fails is 502 `model_error`.
- **A rule's own model.** `model` on a rule is a decider spec: `jev`, `clef`, `anthropic`, or `name:model` such as `clef:clef-flash` or `ollama:llama3.2`. Empty means the default. An email is decided by the model of its highest-priority candidate rule that names one.

## Settings and provider keys

`GET /api/settings` returns the settings in force. `openai_base_url` and `ollama_url` are plain settings, readable and writable, so every decider can be set up in the browser; an empty string removes the stored value. The environment variables in `docs/backend-plan.md` → Configuration are the defaults; whatever is saved through `PATCH /api/settings` lies over them and wins. Provider keys saved this way are stored in the `settings` table encrypted under the master key. Sending a key as an empty string removes the stored one, which puts the environment's back in force.

Nothing needs a restart: the dry-run switch is read before every action, and the decider, its models, the thresholds and the keys are read for every email.

## Live events

`GET /api/events` is a Server-Sent Events stream. Each event has an `id`, an `event` name and one `data` line of JSON, in the same shapes the REST endpoints return:

| Event | Data | Sent when |
| --- | --- | --- |
| `message.processed` | activity row | an email was decided, acted on or corrected |
| `message.review` | activity row | an email went to Needs review |
| `action.undone` | action | one action was undone |
| `account.status` | account | an account's status changed, or it was edited |
| `batch.progress` | batch | a cleanup batch moved forward (M9) |
| `rules.changed` | `{}` | a rule was edited, deleted, reordered or imported |
| `usage.updated` | `{}` | a model call was recorded |

The daemon keeps the last 200 events. A browser's `EventSource` reconnects by itself and sends `Last-Event-ID`; the daemon then first sends what was missed. A new client is sent only what happens from then on. Event ids restart with the daemon; an id from before a restart replays everything kept.

## A tour with curl

This script walks the main flow against a fresh daemon (one with no admin account yet) and stops at the first answer that is not the expected one. It needs `bash` and `curl`. A test runs it against a real daemon on every `make check`, so it cannot go stale.

```bash
#!/usr/bin/env bash
# Usage: start a fresh daemon (`mailrules serve` with an empty data dir), then:
#   BASE=http://127.0.0.1:8080 bash tour.sh
set -euo pipefail
BASE="${BASE:-http://127.0.0.1:8080}"
EMAIL="${EMAIL:-me@example.com}"
PASSWORD="${PASSWORD:-correct horse battery}" # at least 12 characters
JAR="$(mktemp)"
trap 'rm -f "$JAR" "$JAR.rules" "$JAR.events"' EXIT

# call METHOD PATH WANT_STATUS [curl arguments]: makes the request with the session and
# CSRF cookies, checks the status and prints the body.
call() {
  local method="$1" path="$2" want="$3" csrf out status body
  shift 3
  csrf="$(awk '$6 == "mailrules_csrf" { print $7 }' "$JAR")"
  out="$(curl -sS -X "$method" -b "$JAR" -c "$JAR" -H "X-CSRF-Token: $csrf" -w '\n%{http_code}' "$@" "$BASE$path")"
  status="${out##*$'\n'}"
  body="${out%$'\n'*}"
  if [ "$status" != "$want" ]; then
    echo "FAIL $method $path: got $status, want $want: $body" >&2
    exit 1
  fi
  echo "ok   $method $path -> $status" >&2
  printf '%s\n' "$body"
}
json() { call "$@" -H 'Content-Type: application/json'; }

# 1. The daemon is up and ready. These two need no session.
call GET /healthz 200 >/dev/null
call GET /readyz 200 >/dev/null

# 2. First run: /me says setup is needed and hands out the CSRF cookie; setup signs in.
call GET /api/auth/me 401 | grep -q setup_required
json POST /api/auth/setup 201 -d "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}" >/dev/null
call GET /api/auth/me 200 | grep -q "$EMAIL"

# 3. Settings: dry-run is on until you switch it off. Raise the act threshold.
call GET /api/settings 200 | grep -q '"dry_run":true'
json PATCH /api/settings 200 -d '{"min_confidence":0.8,"retention_days":60}' | grep -q '"min_confidence":0.8'
json PATCH /api/settings 400 -d '{"decider":"nope"}' | grep -q '"path":"decider"'

# 4. Accounts. Connecting one needs a real mailbox, so that part runs only when
#    IMAP_USER and IMAP_PASSWORD are set (IMAP_PRESET defaults to icloud).
call GET /api/presets 200 | grep -q icloud
call GET /api/accounts 200 >/dev/null
if [ -n "${IMAP_USER:-}" ] && [ -n "${IMAP_PASSWORD:-}" ]; then
  account="{\"preset\":\"${IMAP_PRESET:-icloud}\",\"username\":\"$IMAP_USER\",\"password\":\"$IMAP_PASSWORD\"}"
  json POST /api/accounts/test 200 -d "$account" >/dev/null
  json POST /api/accounts 201 -d "$account" >/dev/null
fi

# 5. Rules: import a YAML file, list, edit, reorder, export.
cat >"$JAR.rules" <<'YAML'
rules:
  - id: Food orders
    said: Put all Swiggy and Zomato stuff in Food
    match: {from_domain: [swiggy.in, zomato.com]}
    actions: ["move:Food", read]
  - id: Newsletters
    when: Newsletters and promotional emails
    actions: ["move:Reading"]
YAML
call POST /api/rules/import 200 -H 'Content-Type: application/yaml' --data-binary "@$JAR.rules" | grep -q '"created":2'
ids="$(call GET /api/rules 200 | grep -o '"id":[0-9]*' | cut -d: -f2)"
first="$(echo "$ids" | head -n 1)"
json PATCH "/api/rules/$first" 200 -d '{"enabled":false}' | grep -q '"enabled":false'
json PATCH "/api/rules/$first" 400 -d '{"actions":[]}' | grep -q '"path":"actions"'
reversed="$(echo "$ids" | sort -rn | paste -sd, -)"
json POST /api/rules/reorder 200 -d "{\"ids\":[$reversed]}" >/dev/null
call GET /api/rules/export 200 | grep -q 'id: Newsletters'

# 5b. Create a rule by hand: no model is needed. One invalid rule in a batch saves none.
json POST /api/rules/batch 201 -d '{"rules":[{"name":"Login codes","conditions":{"field":"subject","op":"contains_any","value":["code","OTP"]},"actions":[{"type":"keep"},{"type":"flag"}],"position":0}]}' | grep -q '"priority":1'
json POST /api/rules/batch 400 -d '{"rules":[{"name":"Fine","intent":"Receipts","actions":[{"type":"keep"}]},{"name":"Bad","intent":"x","actions":[]}]}' | grep -q '"path":"rules\[1\].actions"'
#     The composer needs a generative model; without a key it says so.
json POST /api/rules/compose 409 -d '{"text":"Put Swiggy in Food"}' | grep -q no_composer_model

# 6. Live events: open the stream, change a rule, see the event arrive.
curl -sS -N --max-time 3 -b "$JAR" "$BASE/api/events" >"$JAR.events" 2>/dev/null &
stream=$!
sleep 1
json PATCH "/api/rules/$first" 200 -d '{"enabled":true}' >/dev/null
wait "$stream" || true # curl stops at --max-time
grep -q 'event: rules.changed' "$JAR.events"

# 7. The feed and the review queue (empty until an account is connected and mail arrives).
call GET '/api/activity?limit=10' 200 | grep -q '"items"'
call GET /api/review 200 | grep -q '"total"'

# 8. "Undo the last hour": one undo batch, with its counts.
batch="$(call POST "/api/actions/undo?since=$(($(date +%s) - 3600))" 200 | grep -o '"id":[0-9]*' | head -n 1 | cut -d: -f2)"
call GET "/api/batches/$batch" 200 | grep -q '"kind":"undo"'

# 9. An endpoint of a later milestone answers 501; metrics are open.
call GET /api/stats/summary 501 | grep -q not_implemented
call GET /metrics 200 | grep -q mailrules_http_requests_total

# 10. Clean up and sign out.
call DELETE "/api/rules/$first" 204 >/dev/null
call POST /api/auth/logout 204 >/dev/null
call GET /api/auth/me 401 | grep -q unauthenticated
echo "tour complete" >&2
```

## Command line

Two commands do the same as the rule endpoints, straight on the database, for headless setups. A running daemon reads the rules for every email, so it follows an import at once.

```text
mailrules rules import <file>    # same merge as POST /api/rules/import
mailrules rules export [file]    # same file as GET /api/rules/export; standard output without a file
```
