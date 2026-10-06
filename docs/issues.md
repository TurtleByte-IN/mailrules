# Open issues

Stand-in for the tracker until MailRules has a team in Linear. One entry per topic. Each entry says who has to act, where it stands, and the question still open with a recommendation. When a question settles, write the answer into the entry and move it to Settled; when the tracker exists, move every open entry there and delete this file.

## Open: product (Tilak)

### 1. Dashboard screen exists in the PRD but not in the prototype

- **Stands:** `docs/prd.md` → Frontend lists a Dashboard (P1). The prototype has none and has a Usage screen the PRD table does not list. The web UI follows the prototype.
- **Question:** Is Dashboard still wanted? Recommendation: no; Activity opens on today's mail and Usage carries cost. Cost of being wrong: one more screen to build later, nothing to undo.

### 2. Rule tester and Templates are separate screens in the PRD, embedded in the prototype

- **Stands:** The prototype puts the tester inside the rule editor and templates inside Add rules. The web UI follows the prototype.
- **Question:** Correct the PRD table to match? Recommendation: yes. Cost of being wrong: none; it is a doc edit.

### 3. Wording written where the prototype has none

- **Stands:** These strings are in the UI and were not in the prototype. Each needs a yes or a rewrite.
  - Mailboxes: "Reconnect", "Pause", "Resume", "Remove", "Reconnecting <label>", "<label> removed", "since <date>, <time>", "No mailbox connected yet.", "Starter rules you picked", "<label> is live. Starter rules could not be added yet.", status labels "Connecting", "Live", "Reconnecting", "Paused", "Sign-in failed", "Error".
  - Remove confirmation: "Remove <label>? MailRules deletes its password, folder list, contacts, activity, undo history and the rules that apply only to this mailbox. Nothing in the mailbox changes."
  - Settings: "Model API keys", "Set", "Not set", "Save", "Replace", "Remove", "Decision model name", "Fallback model", "Rule composer model", "Act at 0.80 or above", the OpenAI-compatible decider option and its note, the Cloudflare and OpenAI key labels.
  - Activity: filter labels "Sorted", "Trashed", "No rule", "Needs review"; "No activity matches these filters."; "Nothing sorted yet. New mail shows up here as it arrives."; "Load more"; " (dry run)" after an outcome that was only recorded; "Failed: <error>"; "; N could not be undone".
  - Rules: "No rules yet.", "Import rules", "Export rules", "Act when sure above your default".
  - Add rules: "Until then, add rules with Import rules on the Rules screen."; the two dictation fallbacks ("This browser has no speech recognition, so dictation is off. Type your rules instead." and "The microphone is blocked for this page. Allow it in the browser, or type your rules instead.").
  - Senders, Cleanup, Usage: "Most recent first", "No senders found.", "Show more", "Left where it is", "Dry run: N emails checked, nothing moved.", "Cleanup failed after N emails…", "since <date>", "decision model N · fallback M".
  - Everywhere: "<thing> is not available yet. This daemon does not have that part built. Nothing was changed."
- **Question:** Approve or rewrite each. Recommendation: approve; they follow the prototype's voice. Cost of being wrong: a string edit.

### 4. Places the real data changed what the prototype shows

- **Stands:** The UI shows what the contract returns and leaves out what it does not.
  - Feed rows and review cards show the sender's address, not a display name.
  - "Always do this for…" names the sender's address; the daemon stores the sender rule by address, and the prototype promised the domain.
  - The "Sorted" filter includes trashed mail; the contract cannot ask for acted-on-but-not-trashed.
  - Review cards no longer prefill a rule idea, and a draft's question has no answer buttons; the contract carries neither.
  - A review answer and a correction both read "you · corrected".
  - Settings: the fallback is a model-name field, not a checkbox; retention is a number of days, not 7/30/90; the threshold is 0 to 1.
  - Mailboxes: no Test button on a connected mailbox, no "Gmail and Outlook, coming soon" tile, no preview counts in the connect wizard.
  - Senders: no "% opened", no "least opened" sort, no reason on a learned rule.
  - Cleanup: no list of past batches, no sample emails in the preview, no tokens or cost while running, no all-folders run.
  - Usage: no budget line.
  - The builder has no "does not contain" operator.
- **Question:** Which of these should come back? Each one is a backend change first (see entry 8). Recommendation: sender display name, past batches and the review rule idea. Cost of being wrong: per-item work in one screen.

## Open: web

### 5. Not built in the UI although the daemon supports it

- **Stands:**
  - Changing a connected mailbox's app password, label or watched folder (`PATCH /api/accounts/{id}`). Consequence: a mailbox in "Sign-in failed" can only be removed and added again.
  - Choosing the TLS mode for "Other IMAP server"; STARTTLS-only servers cannot be connected from the wizard.
  - Re-optimizing an existing rule (`POST /api/rules/{id}/compose`): the API function exists, the prototype has no control for it.
  - Remaining first-run steps (model and key) after the account step; Settings covers both.
  - A refused save in the condition builder is shown as a toast, not on the row it names.
  - With a filter on, a new live email is not added to the feed until reload.
  - `Settings.features` from the daemon is not read; P2 flags are compile-time in `web/src/lib/features.ts`.
- **Question:** Which are needed for the first release? Recommendation: the mailbox password change and the TLS choice. Cost of being wrong: users with a changed app password or a STARTTLS server are stuck.

### 6. Not yet seen with real mail

- **Stands:** No mailbox was connected during the build. The feed row, decision trace, correction, undo of a real action, the `account.status` and `message.processed` events, a successful connection test and a mailbox going live are covered only by tests with payloads shaped from the contract. Rules, settings, import and export, reorder, the dry-run switch and live refresh on `rules.changed` were exercised against a running daemon.
- **Question:** none. Connect a test mailbox in dry-run and walk Activity, Needs review and Mailboxes once.

### 7. No screen-level tests

- **Stands:** Mounting a component under Vitest fails ("mount(...) is not available on the server") because the test config resolves Svelte's server build. Logic is tested through state modules. Adding `svelteTesting()` from `@testing-library/svelte/vite` to `web/vite.config.ts` would allow render tests.
- **Question:** Add render tests? Recommendation: yes, for the 501 and load-error states of each screen. Cost of being wrong: a markup regression in those states goes unseen.

## Open: backend

### 8. Gaps in the contract the UI ran into

- **Stands:** Each of these removed or weakened something on screen.
  - No `GET /api/batches`: a cleanup cannot be undone, or a running one found again, after a page reload. `Batch` carries no scope, tokens or cost.
  - No single call to undo everything done to one email; the row's Undo makes one call per action and a failure midway leaves it half undone. `UndoResult` returns counts only, so the UI refetches the feed.
  - `ActivityItem` has no sender display name and no outcome sentence; nothing tells a review answer from a correction; `always_for_sender` is by address.
  - Activity filters cannot express "sorted but not trashed"; the UI maps "Sorted" to `status=acted` and "No rule" to `stage=none`, which needs confirming.
  - The event stream takes no filters, so a filtered feed cannot stay live.
  - No route tests a stored account's connection. `Account.last_event_at` is when the status last changed, which the name hides. `Preset` has no wording for the secret ("App-specific password").
  - `fallback_model: ""` means off and nothing restores the environment default; `ProviderKeys` cannot tell a stored key from an environment key, so Remove may leave it "Set".
  - `RuleDraft` has a `question` but no options and no way to answer, and no `account_id`, `stack` or `model`. `TestRequest.account_id` is required, so a fresh install cannot test a rule. The test stream has no error event. `Rule.model` accepts any string.
  - `Sender` has no read rate and no reason for a learned rule; it is unclear whether the list returns address rows, domain rows or both. There is no unsubscribe route.
  - `CleanupPreview.groups` has no sample emails or destination; `CleanupRequest.folder` is one folder.
  - `StatsUsage` has no budget; `days[].models` has no purpose; `range=month` does not say calendar month or 30 days.
  - Error messages start with the raw field path ("min_confidence: a rule that…"), which reads oddly beside the field.
- **Question:** Which of these does M8/M9 take? Recommendation: `GET /api/batches`, undo-per-email and the sender display name first; they are the ones a user notices. Cost of being wrong: the matching line in entry 4 stays as it is.

### 9. Generated types mark defaulted request fields as required

- **Stands:** `openapi-typescript` turns a request property with a `default` (`AccountInput.watch_folder`, `RuleInput.stack`, `RuleInput.enabled`, `TestRequest.folder`, `TestRequest.limit`) into a required field. The UI works around it with derived types in `web/src/lib/api/accounts.ts` and `rules.ts`.
- **Question:** Drop the defaults from request schemas, or pass `--default-non-nullable=false` in `gen:api`? Recommendation: the flag; it is one line on the web side. Cost of being wrong: response fields with defaults become optional in the types.

### 10. Backend plan layout still places the OpenAPI spec under `internal/api`

- **Stands:** The spec lives at `api/openapi.yaml`. The layout block in `docs/backend-plan.md` still annotates `internal/api/` with "OpenAPI spec".
- **Question:** none. Remove the two words.

### 11. The binary does not serve the UI yet

- **Stands:** `web/dist` builds, but `internal/web` with `go:embed` is M10's. Until then the UI runs from the Vite dev server (`make dev`).
- **Question:** none.

## Settled

- **Undo the last hour across all rules.** The contract has `POST /api/actions/undo?since=`; the UI uses it.
- **Stats range values.** The contract defines them; Activity's tiles and Usage call the stats routes and wait on M9.
- **Demo data shapes.** Gone: every screen takes its types from the contract.
