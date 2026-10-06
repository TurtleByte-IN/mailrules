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

## Settled

Nothing yet.
