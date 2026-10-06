# Open issues

Stand-in for the tracker until MailRules has a team in Linear. One entry per topic. Each entry says who has to act, where it stands, and the question still open with a recommendation. When a question settles, write the answer into the entry and move it to Settled; when the tracker exists, move every open entry there and delete this file.

## Open

### 1. Dashboard screen exists in the PRD but not in the prototype

- **For:** product (Tilak)
- **Stands:** `docs/prd.md` → Frontend lists a Dashboard (P1): counts for today, account health, cost today, top rules. The prototype has no Dashboard and has a Usage screen the PRD table does not list. The web UI follows the prototype, so Usage is built and Dashboard is not.
- **Question:** Is Dashboard still wanted? Recommendation: no; Activity already opens on today's mail and Usage carries cost. Cost of being wrong: one more screen to design and build later, nothing to undo.

### 2. Rule tester and Templates are separate screens in the PRD, embedded in the prototype

- **For:** product (Tilak)
- **Stands:** The PRD lists Rule tester and Templates as screens. The prototype puts the tester inside the rule editor and templates inside Add rules. The web UI follows the prototype.
- **Question:** Should the PRD table be corrected to match? Recommendation: yes, edit the PRD. Cost of being wrong: none; it is a doc edit.

### 3. Backend plan layout still places the OpenAPI spec under `internal/api`

- **For:** backend
- **Stands:** The spec lives at `api/openapi.yaml` and `docs/backend-plan.md` → HTTP API says so. The layout block in the same file still annotates `internal/api/` with "OpenAPI spec".
- **Question:** none. Remove the two words from the layout comment.

### 4. Web UI is built on demo data ahead of the API; shapes need the backend's review

- **For:** backend (M7, M8, M9)
- **Stands:** Only the auth paths are in `api/openapi.yaml`. Every other screen calls functions in `web/src/lib/api/<resource>.ts` that return in-memory demo data. Each file starts with a `// DEMO:` comment naming the endpoints it stands in for, and its exported types are the shape the UI needs.
- **Old shape:** none; these endpoints have no contract yet.
- **New shape:** the exported interfaces in those files.
- **What breaks if ignored:** nothing fails at build time. When an endpoint ships with a different shape, the screen for it must be reworked at wiring time, and until then the UI shows sample mail that is not the user's.
- **Question:** When M7 writes the spec, does it start from these shapes or from the data model alone? Recommendation: read the UI's types first and deviate on purpose, with a note here per deviation. Cost of being wrong: per-screen rework in the UI.

### 5. The binary does not serve the UI yet

- **For:** backend (M10)
- **Stands:** `web/dist` builds, but `internal/web` with `go:embed` is M10's. Until then the UI runs only from the Vite dev server (`make dev`).
- **Question:** none.

### 6. "Undo the last hour" needs an undo across all rules

- **For:** backend (M7)
- **Stands:** Activity has one button that undoes every action from the last hour. The backend plan only has `POST /api/rules/{id}/undo?since=`, which is per rule. The demo function takes the rule as optional.
- **Question:** Add an all-rules form, or should the UI loop over rules? Recommendation: one endpoint without the rule id, so the undo is one batch and one audit entry. Cost of being wrong: the UI loops and a failure half way leaves a partial undo.

### 7. Cleanup needs a list of past batches

- **For:** backend (M9)
- **Stands:** The Cleanup screen lists past batches with Undo. The backend plan has `GET /api/batches/{id}` only. The demo has a `list()`. The screen also shows tokens and cost per batch, which the `batches` table does not hold.
- **Question:** Add `GET /api/batches` and carry tokens and cost on a batch? Recommendation: yes to both. Cost of being wrong: the list and the cost line come out of the screen.

### 8. The stats endpoint's range values are cut off in the backend plan

- **For:** backend
- **Stands:** In `docs/backend-plan.md` → HTTP API the Stats row breaks at a `|` inside the cell: it reads `GET /api/stats/summary?range=day` and stops. Usage therefore asks for no range and shows this month. The Usage screen also shows a monthly budget (`$5.00` in the prototype) that the plan does not define.
- **Question:** What are the range values, and where does the budget come from? No recommendation; the UI adapts to either.

### 9. Activity's three "today" tiles are not built

- **For:** backend (M9), then web
- **Stands:** The prototype shows Sorted today, Decided without a model, and Model cost today above the feed, with hard-coded numbers. The stats stand-in only has month totals, so the tiles were left out rather than filled with invented figures. The Needs review tile is built.
- **Question:** none. Build them when the stats endpoint has a day range.

### 10. Wording written where the prototype has none

- **For:** product (Tilak)
- **Stands:** These strings are in the UI and were not in the prototype. Each needs a yes or a rewrite.
  - Mailboxes row actions and toasts: "Test", "Reconnect", "Remove", "<email> reconnected", "<email> removed".
  - Remove confirmation: "Remove <email>? MailRules forgets its password, activity and undo history. Nothing in the mailbox changes."
  - Settings keys section: "Model API keys", "Set", "Not set", "Save", "Replace", "<label> saved".
  - Mailboxes: "last event <date>, <time>", "No mailbox connected yet."
  - Activity filters: "Sorted", "Trashed", "No rule", "Needs review", "No activity matches these filters."
  - Dictation: "This browser has no speech recognition, so dictation is off. Type your rules instead." and "The microphone is blocked for this page. Allow it in the browser, or type your rules instead."
- **Question:** Approve or rewrite each. Recommendation: approve; they follow the prototype's voice. Cost of being wrong: a string edit.

### 11. Places the UI follows the backend plan instead of the prototype

- **For:** product (Tilak), for awareness
- **Stands:** Where the two disagreed on data, the UI took the backend plan's shape and kept the prototype's wording on screen.
  - Rules are a condition tree with an exceptions tree and an actions list; the "unless" sentence is generated, so it reads "unless I've replied to the sender".
  - The builder has no "does not contain" operator; the backend operator list has no negated contains.
  - The threshold is 0 to 1 (shown as 0.75), not 50 to 95.
  - The decider id for Haiku is `anthropic`; the label stays "Claude Haiku 4.5".
  - Usage's cost tile is computed from the per-model costs and shows $0.27; the prototype's $0.47 does not match its own per-model figures.
  - A row waiting for review shows a Review link in the decision panel, not the correction form.
  - Dictation adds to the text already typed; the prototype replaced it.
  - Clef can be chosen but has no field for the Cloudflare account id and token; the prototype has no copy for them.
- **Question:** Any of these to reverse? Recommendation: none. Cost of being wrong: per-item rework in one screen.

### 12. Not built because the prototype shows no control or the data is another screen's

- **For:** web
- **Stands:** Rule YAML import and export, and re-optimizing an existing rule, have endpoints in the backend plan but no control in the prototype. "Undo what it did today" on a rule and the "Learned from your corrections" footer under the rule list are in the prototype but belong to Activity and Senders data. Activity's feed is one page with filters applied in the browser; cursor pagination comes with the real endpoint. The remaining first-run steps (model and key) and the "Open first-run setup" button in Settings are not built.
- **Question:** Which of these are wanted for the first release? Recommendation: pagination with M7; the rest after. Cost of being wrong: a missing convenience, nothing destructive.

## Settled

Nothing yet.
