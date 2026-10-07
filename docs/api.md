# MailRules HTTP API

How to talk to the daemon over HTTP: sign in, change settings, manage rules, read the activity feed, fix what a rule got wrong, and follow live events.

The contract is `api/openapi.yaml` (OpenAPI 3.1). It lists every endpoint with its request and response schemas, its error codes and the event payloads; this page does not repeat it. Every endpoint in it is built. A test fails when the daemon serves a route the contract does not list, or the reverse, and when a response has a field the contract does not define.

## The rules of the road

- **Local by default.** The daemon listens on `127.0.0.1:8080` (`MAILRULES_LISTEN`).
- **Session cookie.** `POST /api/auth/setup` (first run) or `POST /api/auth/login` sets `mailrules_session`. Every route needs it except those two, `logout`, `me`, `/healthz`, `/readyz` and `/metrics`. Without it the answer is 401 with code `setup_required` (no admin account yet) or `unauthenticated`.
- **CSRF header.** Every response hands out a `mailrules_csrf` cookie. Every request that is not a GET must send its value back in the `X-CSRF-Token` header, or it is answered 403 `csrf_failed`. Call `GET /api/auth/me` first to receive the cookie.
- **One port for the API and the UI.** Paths under `/api/` are always the API: one that does not exist answers 404 `not_found` as JSON. Every other path that is not `/healthz`, `/readyz` or `/metrics` is the web UI, which answers `index.html` for a path that is not a file so that its own routes work.
- **JSON in, JSON out.** Unknown fields are rejected (400 `invalid_json`). Timestamps are unix seconds, ids are integers, and an absent time or reference is `null`.
- **Errors** are `{"error": {"code": "...", "message": "...", "path": "..."}}`. `code` is stable; `message` is a sentence for a person, safe to show as it is (capitalised, ending with a full stop, never a field path); `path` alone says where the problem is, for example `conditions.all[0].op` for a rule. A refused import has one line per rule with a problem, each naming its rule: `Rule 3 ("Scams"): Unknown operator "like".`
- **Pagination.** `GET /api/activity`, `GET /api/review`, `GET /api/senders` and `GET /api/batches` take `?cursor=&limit=` (limit 1 to 100, default 50) and return `next_cursor`; `null` means the end.
- **Secrets are write-only.** No response contains an account password or a provider key. `GET /api/settings` says only which keys are set. The only message text ever returned is `snippet`, at most 200 characters.
- **Dry-run is on by default.** Until `PATCH /api/settings {"dry_run": false}`, decisions are recorded and no mailbox is changed.
- **Trash goes to MailRules' own folder by default.** While `trash_to_folder` is true (the default, also for an install that predates it), every trash action, whether from a rule, a sender block, a correction, a Needs review answer or a cleanup run, moves the email to an ordinary folder named "MailRules Trash" instead of the server's Trash, which providers empty on their own (iCloud after 30 days). The folder is made on first use, in live mode only, and gets no special-use role. If the server refuses to make it, the action fails; it never falls back to Trash. The action is still kind `trash`, so stats, `?outcome=trashed` and the Trashed tile count it; its `folder` is "MailRules Trash", and undo moves it back from there. `PATCH /api/settings {"trash_to_folder": false}` sends trash to the server's Trash again. Junk always goes to the server's Junk folder.
- **The mailbox's own mail is left alone by default.** While `leave_own_mail` is true (the default), an email whose From address is the account's own is not sorted at all: no sender rule, rule, model call or action, so it is never trashed, never waits in Needs review and never teaches a sender rule. Its decision has stage `none` and the reason "Sent from this mailbox's own address, so MailRules left it alone.", so the feed shows it kept in the inbox and stats count it as left there. The account's own address is its username, compared ignoring case; on iCloud a username with or without its domain covers the same name at icloud.com, me.com and mac.com. Aliases are not known, so mail from an alias is sorted like any other. The rule tester (`POST /api/rules/test`, and the composer's test of its drafts) and a cleanup check show the same row, with nothing to sort. `PATCH /api/settings {"leave_own_mail": false}` sorts that mail like any other; `null` puts the default back.

## Creating, composing and testing rules

- **Check a connected account.** `POST /api/accounts/{id}/test` logs in with the stored password on a connection of its own and lists the folders, without restarting the account's watcher or changing its status. An account's `last_event_at` is when its status last changed; `last_mail_at` is when its newest email arrived. Each preset in `GET /api/presets` says what the provider calls the secret to paste (`secret_label`).
- **Create.** `POST /api/rules/batch` saves one rule or many in one transaction: a rule built by hand from conditions, a template, or composer drafts the user approved. It needs no model. Every rule is validated first, so one bad rule saves none; the error's `path` is `rules[1].conditions.all[0].op`. Two rules of a batch with the same name, or one named like a saved rule, are refused (`rules[1].name`). `new_folders` are created on the mail server, except in dry-run and on an offline account, where the first live move creates the folder. `position` drops a rule into the priority order (0 is first); without it the rule goes last.
- **Compose.** `POST /api/rules/compose` turns free text into draft rules and saves nothing. Each draft is a card: the rule, `new_folders`, at most one `question`, `conflicts` with existing rules, `errors`, and `match_count` with up to five `samples` from the account's last 200 emails. A draft with `errors` cannot be saved as it is; the request still answers 200. A draft has every field of a rule (`account_id`, `stack` and `model` too). Send the cards the user approves to `/api/rules/batch`.
- **Re-optimize.** `POST /api/rules/{id}/compose` returns one draft to replace a rule, from its original wording plus new text. Save it with `PATCH /api/rules/{id}`.
- **Suggest.** `POST /api/rules/suggest` with the scope a cleanup check takes (`{"account_id", "folder", "since", "limit"}`, at most 2,000 emails) plus `samples` (a number from 1, or `"all"`; default 5) and `body` (`none`, `first500` or `full`; default `none`) scans the mail already in a folder and has the composer model suggest rules for it. It reads with BODY.PEEK and saves nothing; bodies are fetched only when `body` is not `none`. MailRules groups the emails by sender address itself and sends the model a digest per group (address, name, domain, counts of emails, read and List-Unsubscribe, whether the owner has written to it, dates, and the chosen samples), split into several requests when it is too big for one, the answers then merged by one more call that sees the suggestions only. Each of at most 12 suggestions is a draft card (`RuleSuggestion`) with its `kind` (`exact`: conditions only; `meaning`: an intent), `trashes`, the AI's `reason` and the sender `groups` it covers. Its `match_count` and `samples` are, for an exact rule, its conditions run over the scanned emails, and for a rule by meaning the scanned emails of the groups the AI put it on, not a test of the rule. Progress streams as for a rule test (`SuggestProgress`: phase, emails read, model requests, tokens, cost); `notes` says when samples were dropped to fit, suggestions over the cap were left out, or the parts could not be merged. Send the cards the user ticks to `/api/rules/batch`; existing mail is sorted from Cleanup. Calls are booked under the purpose `suggest`.
- **Test.** `POST /api/rules/test` runs rules over an account's newest mail and reports, per email, the rule, stage, confidence, reason and the actions it would take. It only reads: mail stays unread and where it is, and nothing is recorded but the model calls. A request that names rules tests exactly those and nothing else: `rule_ids` the saved rules named (switched on), `rules` the drafts given, both together when both are sent, and never the other saved rules or the sender rules; with neither, the saved rule set runs as it is. `limit` is 1 to 2,000 (default 200). A client that sends `Accept: text/event-stream` (the web UI does) gets an event stream at any size: a `progress` event (`{"done", "total", "model_calls"}`) as soon as the mail is listed (0 of N), then one every `total / 100` emails (every email up to 199 of them) and at N of N, then one `done` event with the result, or one `error` event if the run fails midway. Any other client gets the result as one JSON body. A run that cannot start (no such folder, no model, offline account, invalid input) is plain JSON with its usual status, whatever the client accepts. A client that drops the request ends the run, and the daemon logs it as cancelled, not as an error.
- **Models.** The composer and the rule suggestions need a generative model, and a test needs a decision model only when one of the rules it tests has an intent. With none set the answer is 409 `no_composer_model`, with a message to show; condition-only rules are created and tested without any model. A model that fails is 502 `model_error`. The composer model is the `composer_model` setting: a bare name is a Claude model on Anthropic (`claude-haiku-4-5`, as before), `anthropic:<model>` the same, `openai:<model>` the OpenAI-compatible endpoint (`openai_base_url` and the OpenAI key), `ollama:<model>` the Ollama server (`ollama_url`, no key). The 409's message names what that provider still needs: the Claude (Anthropic) key, the OpenAI key or the Ollama server URL.
- **A Claude key that covers a whole organisation.** Such a key must name a workspace on every request (the `anthropic-workspace-id` header), and MailRules sends the `anthropic_workspace_id` setting with every Claude request (composer, decision model, fallback). When Claude refuses a request for want of one while the setting is empty, MailRules looks the workspace up once (see Settings below): with exactly one workspace it stores it and makes the request again, so it works. Otherwise Describe it, Rewrite with AI, Suggest from my mail and the tester answer 409 `anthropic_workspace_needed` with a sentence to show ("…Choose it in Settings."), and a stream that had begun ends with an `error` event carrying the same code and sentence.
- **A rule's own model.** `model` on a rule is a decider spec: `jev`, `clef`, `anthropic`, or `name:model` such as `clef:clef-flash` or `ollama:llama3.2`. Empty means the default. An email is decided by the model of its highest-priority candidate rule that names one.

## Templates, senders, cleanup and stats

- **Templates.** `GET /api/templates` is the gallery: nine ready rules (newsletters, receipts, login codes, cold sales, recruiters, travel, social notifications, bank statements, calendar invites). Add one by sending its `rule` to `POST /api/rules/batch`.
- **Senders.** `GET /api/senders` lists every address mail was seen from in the last 30 days, by volume (`sort=recent` for the latest first), with its display name, whether its mail carries a List-Unsubscribe header, and its sender rule if it has one. Addresses and domains that have a sender rule are listed even when quiet; such a row has an empty `name`, so show its `value` once. The cursor of this list is a plain offset. `source=learned` shows the rules MailRules learned from three confident, identical model decisions (one, for newsletter-type mail that passed DMARC); `source=user` the ones the user set; `q=` searches address and name. `PUT /api/senders/{type}/{value}` with `{"verdict": "route" | "keep" | "block", "rule_id"}` sets a sender (type `address` or `domain`); `DELETE` forgets a rule, a learned one too. `hits` counts the emails a sender rule has settled, each without a model call. There is no read rate and no "unread" sort, and unsubscribing is not built.
- **Cleanup.** `POST /api/cleanup/check` with `{"account_id", "folder", "since", "limit"}` starts one real check of the selection (the newest `limit` emails of the folder, or those received from `since` on, or both; `limit` is 1 to 2000 and left out means 2000, so a check never covers more than the newest 2,000 emails of its range). `matched` in the answer is how many emails the folder holds in that range before the limit cut it, known once the mail is listed, so a screen can say "the newest 2,000 of 4,310"; the check goes through the cut list, one more check covers the rest. The check: every email goes through the actual flow — sender rules, conditions, then the decision model for rules with an intent, exactly as live processing decides — moving nothing and recording nothing in Activity, but really asking the model and paying for it (the calls are booked on the usage ledger under the `cleanup` purpose). It answers 202 with the check, `running`; `check.progress` events and `GET /api/cleanup/check?account_id=` carry `done`/`total`, `model_calls`, `cost_usd` and, once `ready`, one `CleanupCheckRow` per email (sender, subject, the rule that took it, the actions, the confidence, whether it can be ticked, and `review` when it would wait in Needs review; `confidence` means something only where a rule took the email or it is `review`, and is 0 for an email the model left alone). The check is held in the daemon's memory, one per account (a new check replaces the previous), and lost on a restart; `DELETE /api/cleanup/check?account_id=` discards it. The user's ticks are kept with the check: `PUT /api/cleanup/check/selection` with `{"account_id", "check_id", "exclude"}` (the `index` of every selectable row that is not ticked; empty = all ticked) answers 204 and replaces the saved list, and `GET` returns it as `exclude` with the rows, so a reload or a return to the screen shows the same ticks. It is refused 400 `invalid_input` (path `exclude`) for a row that does not exist or cannot be ticked, 409 `preview_stale` when `check_id` is not the account's current check or the rules changed since, and 409 `check_not_ready` while the check runs or after it failed. The selection is deleted with the check (Sort, Discard, a new check); it only restores the screen, and Sort takes its own `exclude`. `POST /api/cleanup/run` with `{"account_id", "check_id", "exclude"}` applies the kept rows (every selectable row minus the `exclude` indices) as one undoable batch of kind `cleanup`, making no model calls; it is refused 409 `preview_stale` when the check is gone or the rules changed since, `check_not_ready` while it is still running or failed, and `cleanup_running` when a Sort is already going. The batch records what its check covered as `limit` (the most emails it was allowed) and `matched` (how many the range held before that cut), both null for a batch made before they were kept, so a screen can tell a run of the newest 25 from a run of all time. A selected email no longer where the check found it is passed over and counted in the batch's `skipped`; `done` counts every row handled, skipped ones included, so the emails acted on are `done - skipped`. Dry-run is honoured and one batch undo puts everything back.
- **Stats.** `GET /api/stats/summary?range=day|week|month` feeds the Activity tiles: emails processed, sorted and trashed (an email whose actions were all undone counts as left in the inbox, not as sorted), the Needs review queue, the share decided without a model (only emails a sender rule or a condition rule acted on count; one waiting in Needs review or matched by no rule does not, but stays in the base), cost, calls per model, the top five rules and each account's health. `GET /api/stats/usage` is the Usage screen: the last 30 days with calls and cost per day and model, per rule and per model. Its `without_model` counts over `processed` (every email decided), not over `emails` (the sorted ones), so the free share is `without_model / processed`; it counts only emails a sender rule or a condition rule acted on, never one left in Needs review or matched by no rule. A `by_rule` row with `rule_id` null and an empty name is the mail no rule was applied to. Ranges start on a UTC day boundary and include today.
- **Undo lasts 30 days.** An action, and a batch, can be undone for 30 days. After that every undo endpoint answers 409 `too_old`, "undo since" leaves the older actions out, and a feed row's `undoable` turns false. Undoing a batch that only recorded dry-run actions does nothing and answers 200 with zero counts.
- **Why this happened.** In `GET /api/messages/{id}`, a decision step of the trace carries `candidates`: the probability the decision model (Jev or Clef) gave each rule it chose between. It is empty for a generative model, which gives none.
- **What a feed row tells you.** Each row of `GET /api/activity`, each Needs review item and `GET /api/messages/{id}` carries `from_name` (the sender's display name, empty when the email had none), `outcome` (a sentence ready to show: "Moved to Food · read", "Moved to MailRules Trash" (or "Moved to Trash" for a trash that went to the server's Trash), "Kept in Inbox", "Would move to Food" in dry-run, "Undone · back in Inbox") and, once a person has had the last word, `correction` with `kind`: `correction` for a fix made from the feed, `review` for an answer given in Needs review. `?outcome=sorted|trashed|inbox|review` lists the mail behind each of the Overview's four groups, counted exactly as `went` in the summary counts them.
- **Always for this sender, or this domain.** Both fix endpoints take `always_for`: `"address"` (the same as `always_for_sender: true`) or `"domain"`, which stores a sender rule for the sender's whole domain. A domain rule for a free-mail domain such as gmail.com is refused with 422 `domain_too_broad` before anything is done.
- **Undo one email.** `POST /api/messages/{id}/undo` undoes everything still in effect on that email in one call, newest first, as one `undo` batch, and answers with the feed row as it is now plus `undone` and `failed`. An action that cannot be undone does not stop the others; the call is refused (409 `message_gone`, or `account_offline`) only when not one action could be undone. `POST /api/actions/{id}/undo` still undoes a single action.
- **A correction the account cannot carry out.** When the right rule archives, trashes (with `trash_to_folder` off) or junks and the mail account has no folder marked for that, `POST /api/messages/{id}/correct` and `/api/review/{message_id}/resolve` answer 422 `no_special_folder` with a message to show. What had been done to the email was already undone by then, and the daemon sends the events for it: `action.undone` per action, then `message.processed` with the row as it now is.

## Settings and provider keys

`GET /api/settings` returns the settings in force. `openai_base_url` and `ollama_url` are plain settings, readable and writable, so every decider can be set up in the browser. The environment variables in `docs/backend-plan.md` → Configuration are the defaults; whatever is saved through `PATCH /api/settings` lies over them and wins. Provider keys saved this way are stored in the `settings` table encrypted under the master key.

- **One rule for every setting.** A field left out stays as it is. `null` forgets the stored value, so the environment's default is back in force (`retention_days`, `trash_to_folder` and `leave_own_mail` have no environment variable: their built-in defaults, 30, true and true, come back). An empty string is a value and is stored: no fallback (`fallback_model`), the provider's default model (`decider_model`), api.openai.com (`openai_base_url`), no Ollama server (`ollama_url`). Where empty cannot work it is refused: `composer_model`, and `decider_model` for `openai` and `ollama`. A `composer_model` that names a provider without a model (`openai:`) or a provider other than `anthropic`, `openai` and `ollama` is refused too. A provider key cannot be set to nothing, so in `keys` both `""` and `null` remove the stored key.
- **Where a key comes from.** `keys` says, per key, `"stored"` (saved through the API; it wins and can be removed), `"environment"` (set in the daemon's environment only) or `"none"`.
- **Warnings instead of a refusal.** The first-run wizard saves in steps, so a change that leaves the chosen decider, or the composer model, without its key or URL is saved and answers 200. `warnings` in the response, and in `GET /api/settings`, lists what is missing as `{"code", "message", "path"}`, with `code` `decider_not_ready` or `composer_not_ready` and `path` naming the setting to fill in (`ollama_url`, `keys.openrouter_api_key`). A setting both lack is named once, by the decider's warning. `anthropic_workspace_needed` (path `anthropic_workspace_id`) says Claude refused a request because the key covers a whole organisation and no workspace could be found; it goes once a workspace is set.
- **The Claude workspace.** `anthropic_workspace_id` is not a secret: empty, or a `wrkspc_` ID (anything else is refused with 400 on that path); the environment's default is `ANTHROPIC_WORKSPACE_ID`. `GET /api/settings` also returns `anthropic_workspace_name` (when a lookup listed it) and `anthropic_workspace_found` (MailRules stored it itself). `GET /api/settings/anthropic-workspaces` says which workspace the Claude key in force needs, as `{"status", "workspaces": [{"id", "name"}]}`: `none_needed` (a key made for one workspace, or no key), `one` (stored as the setting when none was set), `several` (pick one and `PATCH` it), `failed` (Anthropic could not say: type the ID; the reason is in the daemon's log). It asks Anthropic (List Workspaces with the Default Workspace included; when that is refused, a free token count without a workspace tells a key made for one workspace from one that may not list) only while the answer is unknown: `none_needed`, `one` and `several` are kept per key, by a fingerprint, never the key; `failed` is asked again. Saving or replacing the Claude key runs the same lookup before answering, when no workspace is set, and forgets a workspace MailRules found for the old key; one the user chose or typed stays. The Settings screen calls the endpoint while a Claude key is in force and no workspace is set.

Nothing needs a restart: the dry-run and `trash_to_folder` switches are read before every action, and `leave_own_mail`, the decider, its models, the thresholds and the keys are read for every email.

## Live events

`GET /api/events` is a Server-Sent Events stream. Each event has an `id`, an `event` name and one `data` line of JSON, in the same shapes the REST endpoints return:

| Event | Data | Sent when |
| --- | --- | --- |
| `message.processed` | activity row | an email was decided, acted on or corrected |
| `message.review` | activity row | an email went to Needs review |
| `action.undone` | action | one action was undone |
| `account.status` | account | an account's status changed, or it was edited |
| `batch.progress` | batch | a cleanup run moved forward (about a hundred times per run) or ended |
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
trap '' PIPE # see call: writing to a reader that has seen enough must not end the script

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
  # A reader such as `grep -q` may close the pipe at its first match; that is not a failure.
  printf '%s\n' "$body" 2>/dev/null || true
}
json() { call "$@" -H 'Content-Type: application/json'; }

# 1. The daemon is up and ready. These two need no session.
call GET /healthz 200 >/dev/null
call GET /readyz 200 >/dev/null

# 2. First run: /me says setup is needed and hands out the CSRF cookie; setup signs in.
call GET /api/auth/me 401 | grep -q setup_required
json POST /api/auth/setup 201 -d "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}" >/dev/null
call GET /api/auth/me 200 | grep -q "$EMAIL"

# 3. Settings: dry-run is on until you switch it off, trash goes to MailRules Trash, and
#    the mailbox's own mail is left alone.
#    Raise the act threshold.
call GET /api/settings 200 | grep -q '"dry_run":true'
call GET /api/settings 200 | grep -q '"trash_to_folder":true'
call GET /api/settings 200 | grep -q '"leave_own_mail":true'
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

# 9. Templates, senders, stats and past batches; metrics are open.
call GET /api/templates 200 | grep -q '"id": "newsletters"'
json PUT /api/senders/domain/news.example 200 -d '{"verdict":"keep"}' | grep -q '"source":"user"'
call GET '/api/senders?source=user' 200 | grep -q news.example
call DELETE /api/senders/domain/news.example 204 >/dev/null
call GET '/api/stats/summary?range=week' 200 | grep -q '"decided_without_model"'
call GET /api/stats/usage 200 | grep -q '"by_rule"'
call GET '/api/batches?kind=undo' 200 | grep -q "\"id\":$batch"
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
