package api

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/actions"
	"github.com/TurtleByte-IN/mailrules/internal/config"
	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/mailtest"
	"github.com/TurtleByte-IN/mailrules/internal/models"
	"github.com/TurtleByte-IN/mailrules/internal/pipeline"
	"github.com/TurtleByte-IN/mailrules/internal/settings"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/worker"
)

// env is the daemon as `serve` wires it, on a temp SQLite file, with a fake mailbox in
// place of the IMAP server and a scripted decider in place of the model.
type env struct {
	*client
	t          *testing.T
	ck         *clock
	db         *sql.DB
	st         *store.Store
	hub        *events.Hub
	mb         *mailtest.Mailbox
	mgr        *worker.Manager
	sett       *settings.Settings
	decider    *models.Fake
	noDecider  bool                    // no decision model is set
	own        map[string]*models.Fake // deciders that rules may name as their own model
	gen        *models.Fake            // the composer's model; nil = none is set
	connectErr error                   // makes the next logins fail
	doc        map[string]any
}

// Live, RouterFor and Composer make env the daemon's source of models (ModelSource), with
// scripted fakes in place of the providers.
func (e *env) Live(context.Context) (*models.Router, float64) {
	if e.noDecider {
		return nil, 0.75
	}
	return &models.Router{Primary: e.decider, Usage: e.st, Now: e.ck.now}, 0.75
}

func (e *env) RouterFor(_ context.Context, spec string) *models.Router {
	if d := e.own[spec]; d != nil {
		return &models.Router{Primary: d, Usage: e.st, Now: e.ck.now}
	}
	return nil
}

func (e *env) Composer(context.Context) (models.Generator, error) {
	if e.gen == nil {
		return nil, settings.ErrNoComposer
	}
	return e.gen, nil
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := t.Context()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(nil, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, db: db, st: store.New(db), ck: &clock{t: time.Unix(1_800_000_000, 0)}, hub: events.NewHub(),
		mb: mailtest.New(1), mgr: &worker.Manager{}, decider: &models.Fake{NameValue: "fake"}, doc: spec(t)}
	for name, role := range map[string]string{"Trash": mail.RoleTrash, "Archive": mail.RoleArchive, "Sent": mail.RoleSent} {
		e.mb.AddFolder(name, role)
	}
	master := make([]byte, 32)
	e.sett = &settings.Settings{Store: e.st, Master: master, Env: cfg, Deps: models.Deps{Caller: models.NewCaller(1), Prices: models.DefaultPrices()}}
	exec := &actions.Exec{Store: e.st, Accounts: e.mgr, Hub: e.hub, DryRunDefault: cfg.DryRun, Now: e.ck.now}

	runCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	t.Cleanup(func() { stop(); e.mgr.Wait() }) // runs before the database closes
	start := func(acct store.Account) {
		e.mgr.Start(runCtx, &worker.Supervisor{
			Account: acct, Store: e.st, Hub: e.hub,
			Open:       func(context.Context) (mail.Mailbox, error) { return e.mb, nil },
			Pipeline:   pipeline.Pipeline{Store: e.st, Exec: exec, Hub: e.hub, BodyChars: 2000, Now: e.ck.now, Live: e.Live, Override: e.RouterFor},
			BackoffMin: time.Millisecond, DrainTimeout: 50 * time.Millisecond,
		})
	}
	e.client = &client{t: t, cookies: map[string]string{}, h: NewHandler(Options{
		Store: e.st, Now: e.ck.now, Hub: e.hub, Exec: exec, Settings: e.sett, Models: e, Master: master, Version: "test",
		StartCheck: e.mgr.StartCheck, Checks: e.mgr.Checks(), Sort: e.mgr.Sort,
		Connect: func(_ context.Context, acct store.Account, _ string) (mail.Mailbox, string, error) {
			if e.connectErr != nil {
				return nil, "", e.connectErr
			}
			return e.mb, acct.Username, nil
		},
		StartAccount: start, StopAccount: e.mgr.Stop,
	})}
	return e
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// call makes a request and fails the test unless it is answered with the wanted status.
func (e *env) call(method, path, body string, want int) map[string]any {
	e.t.Helper()
	r := e.do(method, path, body)
	if r.status != want {
		e.t.Fatalf("%s %s = %d %s, want %d", method, path, r.status, r.raw, want)
	}
	if len(r.raw) == 0 || r.header.Get("Content-Type") != "application/json" {
		return nil
	}
	return r.object(e.t)
}

// refuse makes a request and checks it is refused with this status, code and path.
func (e *env) refuse(method, path, body string, status int, code, errPath string) {
	e.t.Helper()
	r := e.do(method, path, body)
	if r.status != status || r.body.Error.Code != code || r.body.Error.Path != errPath {
		e.t.Fatalf("%s %s = %d %q path %q (%s), want %d %q path %q", method, path, r.status, r.body.Error.Code,
			r.body.Error.Path, r.body.Error.Message, status, code, errPath)
	}
}

func (e *env) signIn() {
	e.t.Helper()
	e.call(http.MethodGet, "/api/auth/me", "", http.StatusUnauthorized)
	e.call(http.MethodPost, "/api/auth/setup", goodBody, http.StatusCreated)
}

const accountBody = `{"preset":"generic","host":"imap.example.test","username":"me@example.test","password":"app-password-1234"}`

// connect signs in, switches dry-run off and connects the fake mailbox as account 1.
func (e *env) connect() {
	e.t.Helper()
	e.signIn()
	e.call(http.MethodPatch, "/api/settings", `{"dry_run":false}`, http.StatusOK)
	e.call(http.MethodPost, "/api/accounts", accountBody, http.StatusCreated)
	e.live()
}

func (e *env) live() {
	e.t.Helper()
	eventually(e.t, "the account to be live", func() bool {
		a, err := e.st.Account(e.t.Context(), 1)
		_, mbErr := e.mgr.Mailbox(1)
		return err == nil && a.Status == worker.StatusLive && mbErr == nil
	})
}

const rulesYAML = `rules:
  - id: Food
    said: Put Swiggy in Food
    match: {from_domain: swiggy.in}
    actions: ["move:Food", read]
  - id: Reading
    when: Newsletters and promotions
    actions: ["move:Reading"]
`

func (e *env) deliver(from, subject string) mail.MsgRef {
	return e.mb.Deliver("INBOX", "From: "+from+"\r\nTo: me@example.test\r\nSubject: "+subject+
		"\r\nMessage-ID: <"+strings.ReplaceAll(subject, " ", "-")+"@example.test>\r\n\r\n"+strings.Repeat("The body of "+subject+". ", 30)+"\r\n")
}

// item waits until the email with this subject has reached a state and returns its feed row.
func (e *env) item(subject, state string) map[string]any {
	e.t.Helper()
	var found map[string]any
	eventually(e.t, subject+" to be "+state, func() bool {
		for _, it := range e.call(http.MethodGet, "/api/activity", "", http.StatusOK)["items"].([]any) {
			if m := it.(map[string]any); m["subject"] == subject && m["state"] == state {
				found = m
				return true
			}
		}
		return false
	})
	return found
}

func id(v any) int64 { return int64(v.(float64)) }

// folderOf says which folder of the fake mailbox holds the email with this subject.
func (e *env) folderOf(subject string) string {
	e.t.Helper()
	folders, _ := e.mb.Folders(e.t.Context())
	for _, f := range folders {
		ref, err := e.mb.FindByMessageID(e.t.Context(), f.Name, strings.ReplaceAll(subject, " ", "-")+"@example.test")
		if err == nil && ref.UID != 0 {
			return f.Name
		}
	}
	return ""
}

// Every route but the auth ones needs a session, and every non-GET needs the CSRF header.
func TestSessionAndCSRFOnEveryRoute(t *testing.T) {
	e := newEnv(t)
	e.do(http.MethodGet, "/api/auth/me", "") // hands out the CSRF cookie
	fill := strings.NewReplacer("{id}", "1", "{message_id}", "1", "{type}", "address", "{value}", "a@b.example")
	for _, r := range (&server{}).routes() {
		path := fill.Replace(r.path)
		if r.method != http.MethodGet {
			e.noCSRF = true
			if got := e.do(r.method, path, "{}"); got.status != http.StatusForbidden || got.body.Error.Code != "csrf_failed" {
				t.Errorf("%s %s without the CSRF header = %d %q, want 403 csrf_failed", r.method, r.path, got.status, got.body.Error.Code)
			}
			e.noCSRF = false
		}
		if r.public {
			continue
		}
		if got := e.do(r.method, path, "{}"); got.status != http.StatusUnauthorized || got.body.Error.Code != "setup_required" {
			t.Errorf("%s %s without a session = %d %q, want 401 setup_required", r.method, r.path, got.status, got.body.Error.Code)
		}
	}
	// The operational routes are open: they carry no user data.
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		if got := e.do(http.MethodGet, path, ""); got.status != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, got.status)
		}
	}
}

func TestOpsAndSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	r := e.do(http.MethodGet, "/api/auth/me", "")
	for header, want := range map[string]string{"Content-Security-Policy": "default-src 'self'", "X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY"} {
		if got := r.header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	e.do(http.MethodGet, "/api/accounts/123", "")
	metrics := string(e.do(http.MethodGet, "/metrics", "").raw)
	for _, want := range []string{`mailrules_http_requests_total{code="401",method="GET",route="GET /api/accounts/{id}"} 1`, `mailrules_build_info{version="test"} 1`, "go_goroutines"} {
		if !strings.Contains(metrics, want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}
	if strings.Contains(metrics, "/api/accounts/123") {
		t.Error("/metrics labels a request by its raw path")
	}
	// Not ready once the database is gone.
	_ = e.db.Close()
	if got := e.do(http.MethodGet, "/readyz", ""); got.status != http.StatusServiceUnavailable {
		t.Errorf("readyz with the database closed = %d, want 503", got.status)
	}
	if got := e.do(http.MethodGet, "/healthz", ""); got.status != http.StatusOK {
		t.Errorf("healthz with the database closed = %d, want 200", got.status)
	}
}

func TestAccounts(t *testing.T) {
	e := newEnv(t)
	e.signIn()
	ctx := t.Context()

	presets := e.call(http.MethodGet, "/api/presets", "", http.StatusOK)["items"].([]any)
	if len(presets) != 5 || presets[0].(map[string]any)["name"] != "icloud" {
		t.Fatalf("presets = %v", presets)
	}
	conform(t, e.doc, "Preset", presets[0])

	for body, path := range map[string]string{
		`{"preset":"hotmail","username":"me","password":"pw"}`:                      "preset",
		`{"preset":"icloud","password":"pw"}`:                                       "username",
		`{"preset":"icloud","username":"me"}`:                                       "password",
		`{"preset":"generic","username":"me","password":"pw"}`:                      "host",
		`{"preset":"icloud","username":"me","password":"pw","port":70000}`:          "port",
		`{"preset":"icloud","username":"me","password":"pw","tls_mode":"plain"}`:    "tls_mode",
		`{"preset":"icloud","username":"me","password":"pw","watch_folder":"Nope"}`: "watch_folder",
	} {
		status, code := http.StatusBadRequest, "invalid_input"
		if path == "watch_folder" { // the server is asked, and has no such folder
			status, code = http.StatusUnprocessableEntity, "no_folder"
		}
		e.refuse(http.MethodPost, "/api/accounts/test", body, status, code, path)
	}
	e.refuse(http.MethodPost, "/api/accounts", `{"preset":"icloud","username":"me","password":"pw","secret":"x"}`, http.StatusBadRequest, "invalid_json", "")

	e.connectErr = fmt.Errorf("login: %w", mail.ErrAuth)
	e.refuse(http.MethodPost, "/api/accounts/test", accountBody, http.StatusUnprocessableEntity, "auth_failed", "password")
	e.refuse(http.MethodPost, "/api/accounts", accountBody, http.StatusUnprocessableEntity, "auth_failed", "password")
	e.connectErr = fmt.Errorf("handshake: %w", mail.ErrTLS)
	e.refuse(http.MethodPost, "/api/accounts/test", accountBody, http.StatusUnprocessableEntity, "tls_failed", "host")
	e.connectErr = fmt.Errorf("dial: %w", mail.ErrConnection)
	e.refuse(http.MethodPost, "/api/accounts/test", accountBody, http.StatusUnprocessableEntity, "connection_failed", "host")
	e.connectErr = nil
	if list := e.call(http.MethodGet, "/api/accounts", "", http.StatusOK)["items"].([]any); len(list) != 0 {
		t.Fatalf("a failed login stored an account: %v", list)
	}

	tested := e.call(http.MethodPost, "/api/accounts/test", accountBody, http.StatusOK)
	conform(t, e.doc, "AccountTestResult", tested)
	if len(tested["folders"].([]any)) != 4 || tested["can_move"] != true {
		t.Errorf("connection test = %v", tested)
	}

	// Creating the account starts its supervisor: no restart, and no secret in the answer.
	r := e.do(http.MethodPost, "/api/accounts", accountBody)
	if r.status != http.StatusCreated || strings.Contains(string(r.raw), "app-password") || strings.Contains(string(r.raw), "secret") {
		t.Fatalf("create = %d %s", r.status, r.raw)
	}
	e.live()
	e.refuse(http.MethodPost, "/api/accounts", accountBody, http.StatusConflict, "account_exists", "username")
	acct := e.call(http.MethodGet, "/api/accounts/1", "", http.StatusOK)["account"].(map[string]any)
	conform(t, e.doc, "Account", acct)
	if acct["status"] != "live" || acct["label"] != "me@example.test" || acct["folder_count"] != float64(4) || acct["watch_folder"] != "INBOX" {
		t.Errorf("account = %v", acct)
	}
	if pw, err := e.st.AccountSecret(ctx, make([]byte, 32), 1); err != nil || pw != "app-password-1234" {
		t.Errorf("stored password = %q, %v", pw, err)
	}
	folders := e.call(http.MethodGet, "/api/accounts/1/folders", "", http.StatusOK)["items"].([]any)
	conform(t, e.doc, "Folder", folders[0])
	if len(folders) != 4 || folders[3].(map[string]any)["special_use"] != `\Trash` {
		t.Errorf("folders = %v", folders)
	}

	// Edits: the label alone leaves the connection; pausing stops it; resuming starts it.
	e.refuse(http.MethodPatch, "/api/accounts/1", `{"label":" "}`, http.StatusBadRequest, "invalid_input", "label")
	e.refuse(http.MethodPatch, "/api/accounts/1", `{"password":""}`, http.StatusBadRequest, "invalid_input", "password")
	e.refuse(http.MethodPatch, "/api/accounts/1", `{"label":7}`, http.StatusBadRequest, "invalid_input", "label")
	e.refuse(http.MethodPatch, "/api/accounts/1", `{"host":"x"}`, http.StatusBadRequest, "invalid_json", "host")
	if a := e.call(http.MethodPatch, "/api/accounts/1", `{"label":"Home"}`, http.StatusOK)["account"].(map[string]any); a["label"] != "Home" || a["status"] != "live" {
		t.Errorf("rename = %v", a)
	}
	if a := e.call(http.MethodPatch, "/api/accounts/1", `{"paused":true}`, http.StatusOK)["account"].(map[string]any); a["status"] != "paused" {
		t.Errorf("pause = %v", a)
	}
	if _, err := e.mgr.Mailbox(1); err == nil {
		t.Error("a paused account is still connected")
	}
	e.refuse(http.MethodPost, "/api/accounts/1/reconnect", "", http.StatusConflict, "account_paused", "")
	r = e.do(http.MethodPatch, "/api/accounts/1", `{"paused":false,"password":"new-app-password"}`)
	if r.status != http.StatusOK || strings.Contains(string(r.raw), "new-app-password") {
		t.Fatalf("resume = %d %s", r.status, r.raw)
	}
	e.live()
	if pw, _ := e.st.AccountSecret(ctx, make([]byte, 32), 1); pw != "new-app-password" {
		t.Errorf("password after the change = %q", pw)
	}
	e.call(http.MethodPost, "/api/accounts/1/reconnect", "", http.StatusOK)
	e.live()

	for _, path := range []string{"/api/accounts/9", "/api/accounts/x", "/api/accounts/9/folders"} {
		e.refuse(http.MethodGet, path, "", http.StatusNotFound, "not_found", "")
	}
	e.refuse(http.MethodPost, "/api/accounts/9/reconnect", "", http.StatusNotFound, "not_found", "")
	e.refuse(http.MethodDelete, "/api/accounts/9", "", http.StatusNotFound, "not_found", "")

	// Deleting stops the supervisor and wipes the row.
	e.call(http.MethodDelete, "/api/accounts/1", "", http.StatusNoContent)
	if _, err := e.mgr.Mailbox(1); err == nil {
		t.Error("a deleted account is still connected")
	}
	e.refuse(http.MethodGet, "/api/accounts/1", "", http.StatusNotFound, "not_found", "")
}

func TestRules(t *testing.T) {
	e := newEnv(t)
	e.signIn()

	e.refuse(http.MethodPost, "/api/rules/import", "rules:\n  - {id: a, match: {sender: x}, actions: [keep]}\n", http.StatusBadRequest, "rule_invalid", "conditions.all[0].field")
	e.refuse(http.MethodPost, "/api/rules/import", "rules: [", http.StatusBadRequest, "rule_invalid", "")
	if got := e.call(http.MethodGet, "/api/rules", "", http.StatusOK)["items"].([]any); len(got) != 0 {
		t.Fatalf("an invalid file stored rules: %v", got)
	}
	imported := e.call(http.MethodPost, "/api/rules/import", rulesYAML, http.StatusOK)
	conform(t, e.doc, "ImportResult", imported)
	if imported["created"] != float64(2) || imported["updated"] != float64(0) {
		t.Fatalf("import = %v", imported)
	}
	// Importing again updates by name: same ids, same order, a new version.
	again := e.call(http.MethodPost, "/api/rules/import", strings.Replace(rulesYAML, "Put Swiggy in Food", "Swiggy goes to Food", 1), http.StatusOK)
	food := again["items"].([]any)[0].(map[string]any)
	if again["created"] != float64(0) || again["updated"] != float64(2) || id(food["id"]) != 1 || food["version"] != float64(2) || food["said"] != "Swiggy goes to Food" {
		t.Fatalf("second import = %v", again)
	}

	r := e.do(http.MethodGet, "/api/rules/export", "")
	if r.status != http.StatusOK || r.header.Get("Content-Type") != "application/yaml" || !strings.Contains(string(r.raw), "id: Reading") || !strings.Contains(string(r.raw), "move:Food") {
		t.Fatalf("export = %d %s", r.status, r.raw)
	}

	rule := e.call(http.MethodGet, "/api/rules/2", "", http.StatusOK)["rule"].(map[string]any)
	conform(t, e.doc, "Rule", rule)
	if rule["name"] != "Reading" || rule["intent"] != "Newsletters and promotions" || rule["account_id"] != nil || rule["hits_week"] != float64(0) || rule["last_match_at"] != nil {
		t.Errorf("rule = %v", rule)
	}

	for body, want := range map[string][2]string{
		`{"actions":[]}`:                {"rule_invalid", "actions"},
		`{"actions":[{"type":"move"}]}`: {"rule_invalid", "actions[0].folder"},
		`{"conditions":{"all":[{"field":"subject","op":"sounds_like"}]}}`: {"rule_invalid", "conditions.all[0].op"},
		`{"actions":[{"type":"trash"}]}`:                                  {"rule_invalid", "min_confidence"},
		`{"name":" "}`:                                                    {"rule_invalid", "name"},
		`{"conditions":{"feild":"subject"}}`:                              {"invalid_input", "conditions"},
		`{"enabled":"yes"}`:                                               {"invalid_input", "enabled"},
		`{"account_id":99}`:                                               {"invalid_input", "account_id"},
		`{"priority":1}`:                                                  {"invalid_json", "priority"},
		`[]`:                                                              {"invalid_json", ""},
	} {
		e.refuse(http.MethodPatch, "/api/rules/2", body, http.StatusBadRequest, want[0], want[1])
	}
	patched := e.call(http.MethodPatch, "/api/rules/2", `{"enabled":false,"min_confidence":0.9,"actions":[{"type":"trash"}],"exceptions":{"field":"is_contact","op":"eq","value":true}}`, http.StatusOK)["rule"].(map[string]any)
	if patched["enabled"] != false || patched["min_confidence"] != 0.9 || patched["version"] != float64(3) || patched["name"] != "Reading" {
		t.Errorf("patched = %v", patched)
	}
	if cleared := e.call(http.MethodPatch, "/api/rules/2", `{"min_confidence":null,"actions":[{"type":"archive"}]}`, http.StatusOK)["rule"].(map[string]any); cleared["min_confidence"] != nil {
		t.Errorf("min_confidence was not cleared: %v", cleared)
	}

	for _, body := range []string{`{"ids":[2]}`, `{"ids":[2,2]}`, `{"ids":[1,2,3]}`, `{"ids":[]}`} {
		e.refuse(http.MethodPost, "/api/rules/reorder", body, http.StatusBadRequest, "invalid_input", "ids")
	}
	order := e.call(http.MethodPost, "/api/rules/reorder", `{"ids":[2,1]}`, http.StatusOK)["items"].([]any)
	if id(order[0].(map[string]any)["id"]) != 2 || order[0].(map[string]any)["priority"] != float64(1) || order[0].(map[string]any)["version"] != float64(4) {
		t.Errorf("after reorder = %v", order)
	}

	e.refuse(http.MethodPost, "/api/rules/1/undo", "", http.StatusBadRequest, "invalid_input", "since")
	e.refuse(http.MethodPost, "/api/rules/1/undo?since=soon", "", http.StatusBadRequest, "invalid_input", "since")
	e.refuse(http.MethodPost, "/api/rules/9/undo?since=1", "", http.StatusNotFound, "not_found", "")
	for _, method := range []string{http.MethodGet, http.MethodPatch, http.MethodDelete} {
		e.refuse(method, "/api/rules/9", "{}", http.StatusNotFound, "not_found", "")
	}
	e.call(http.MethodDelete, "/api/rules/1", "", http.StatusNoContent)
	if left := e.call(http.MethodGet, "/api/rules", "", http.StatusOK)["items"].([]any); len(left) != 1 {
		t.Errorf("after delete = %v", left)
	}
}

// The main path, end to end: mail arrives, a rule moves it, the feed shows why, and every
// fix the UI offers puts it back or files it elsewhere.
func TestActivityAndFixes(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.call(http.MethodPost, "/api/rules/import", rulesYAML, http.StatusOK)

	e.deliver("Swiggy <noreply@swiggy.in>", "order one")
	it := e.item("order one", "acted")
	conform(t, e.doc, "ActivityItem", it)
	msg := id(it["id"])
	acts := it["actions"].([]any)
	dec := it["decision"].(map[string]any)
	if len(acts) != 2 || it["undoable"] != true || dec["stage"] != "condition" || dec["rule_name"] != "Food" || id(dec["rule_id"]) != 1 || len(it["snippet"].(string)) != 200 {
		t.Fatalf("feed row = %v", it)
	}
	move := acts[0].(map[string]any)
	if move["kind"] != "move" || move["status"] != "done" || move["from_folder"] != "INBOX" || move["to_folder"] != "Food" || e.folderOf("order one") != "Food" {
		t.Fatalf("move = %v; the email is in %q", move, e.folderOf("order one"))
	}

	// The detail view: the trace says which step decided and what was done.
	detail := e.call(http.MethodGet, fmt.Sprintf("/api/messages/%d", msg), "", http.StatusOK)["message"].(map[string]any)
	conform(t, e.doc, "MessageDetail", detail)
	trace := detail["trace"].([]any)
	if len(trace) != 3 || detail["current_folder"] != "Food" || detail["folder"] != "INBOX" {
		t.Fatalf("detail = %v", detail)
	}
	if s := trace[0].(map[string]any); s["kind"] != "condition" || s["active"] != true || s["detail"] != `Matched "Food" by its conditions` || s["rule_name"] != "Food" {
		t.Errorf("trace[0] = %v", s)
	}
	if s := trace[1].(map[string]any); s["kind"] != "action" || s["detail"] != "Moved to Food" || s["status"] != "done" || s["active"] != false {
		t.Errorf("trace[1] = %v", s)
	}
	e.refuse(http.MethodGet, "/api/messages/99", "", http.StatusNotFound, "not_found", "")

	// The rule list counts the hit.
	if r := e.call(http.MethodGet, "/api/rules/1", "", http.StatusOK)["rule"].(map[string]any); r["hits_week"] != float64(1) || r["last_match_at"] != float64(e.ck.t.Unix()) {
		t.Errorf("rule after a hit = %v", r)
	}

	// Undo one action: the email is back, and is not sorted again as new mail.
	undone := e.call(http.MethodPost, fmt.Sprintf("/api/actions/%d/undo", id(move["id"])), "", http.StatusOK)["action"].(map[string]any)
	conform(t, e.doc, "MessageAction", undone)
	if undone["status"] != "undone" || undone["undone_at"] != float64(e.ck.t.Unix()) || e.folderOf("order one") != "INBOX" {
		t.Fatalf("undo = %v; the email is in %q", undone, e.folderOf("order one"))
	}
	e.call(http.MethodPost, fmt.Sprintf("/api/actions/%d/undo", id(move["id"])), "", http.StatusOK) // again: a no-op
	e.refuse(http.MethodPost, "/api/actions/99/undo", "", http.StatusNotFound, "not_found", "")

	// Correct it to another rule: filed there, as a correction batch.
	e.refuse(http.MethodPost, fmt.Sprintf("/api/messages/%d/correct", msg), `{"rule_id":99}`, http.StatusBadRequest, "invalid_input", "rule_id")
	e.refuse(http.MethodPost, "/api/messages/99/correct", `{"rule_id":null}`, http.StatusNotFound, "not_found", "")
	fixed := e.call(http.MethodPost, fmt.Sprintf("/api/messages/%d/correct", msg), `{"rule_id":2,"always_for_sender":true}`, http.StatusOK)
	conform(t, e.doc, "FixResult", fixed)
	if c := fixed["item"].(map[string]any)["correction"].(map[string]any); id(c["rule_id"]) != 2 || c["rule_name"] != "Reading" || e.folderOf("order one") != "Reading" {
		t.Fatalf("after the correction: %v; the email is in %q", fixed, e.folderOf("order one"))
	}
	batch := e.call(http.MethodGet, fmt.Sprintf("/api/batches/%d", id(fixed["batch_id"])), "", http.StatusOK)["batch"].(map[string]any)
	conform(t, e.doc, "Batch", batch)
	if batch["kind"] != "correction" || batch["status"] != "done" || batch["actions"].(map[string]any)["done"] != float64(1) {
		t.Errorf("correction batch = %v", batch)
	}
	detail = e.call(http.MethodGet, fmt.Sprintf("/api/messages/%d", msg), "", http.StatusOK)["message"].(map[string]any)
	trace = detail["trace"].([]any)
	if last := trace[len(trace)-1].(map[string]any); last["kind"] != "correction" || last["active"] != true || last["detail"] != "You chose Reading" {
		t.Errorf("trace after the correction = %v", trace)
	}
	// "Always for this sender" made a sender rule: the next Swiggy mail goes to Reading.
	e.deliver("Swiggy <noreply@swiggy.in>", "order two")
	if it := e.item("order two", "acted"); it["decision"].(map[string]any)["stage"] != "sender" || e.folderOf("order two") != "Reading" {
		t.Fatalf("after always_for_sender: %v in %q", it, e.folderOf("order two"))
	}

	// Filters and pagination.
	for query, want := range map[string]int{"": 2, "?rule=2": 1, "?rule=1": 1, "?stage=sender": 1, "?status=acted": 2, "?status=review": 0,
		"?action=read": 1, "?action=trash": 0, "?account=1": 2, "?account=2": 0, "?limit=1": 1} {
		if got := e.call(http.MethodGet, "/api/activity"+query, "", http.StatusOK)["items"].([]any); len(got) != want {
			t.Errorf("activity%s has %d rows, want %d", query, len(got), want)
		}
	}
	page := e.call(http.MethodGet, "/api/activity?limit=1", "", http.StatusOK)
	conform(t, e.doc, "ActivityPage", page)
	cursor, _ := page["next_cursor"].(string)
	page2 := e.call(http.MethodGet, "/api/activity?limit=1&cursor="+cursor, "", http.StatusOK)
	if cursor == "" || page2["next_cursor"] != nil || page2["items"].([]any)[0].(map[string]any)["subject"] != "order one" {
		t.Errorf("page 1 cursor %q, page 2 = %v", cursor, page2)
	}
	for _, query := range []string{"stage=maybe", "status=done", "action=burn", "limit=0", "limit=101", "cursor=x", "account=-1", "rule=x"} {
		e.refuse(http.MethodGet, "/api/activity?"+query, "", http.StatusBadRequest, "invalid_input", strings.SplitN(query, "=", 2)[0])
	}

	// Undo everything this rule did since a minute ago, then everything at all.
	since := e.ck.t.Unix() - 60
	e.refuse(http.MethodPost, "/api/actions/undo", "", http.StatusBadRequest, "invalid_input", "since")
	byRule := e.call(http.MethodPost, fmt.Sprintf("/api/rules/2/undo?since=%d", since), "", http.StatusOK)
	conform(t, e.doc, "UndoResult", byRule)
	if b := byRule["batch"].(map[string]any); byRule["undone"] != float64(1) || byRule["failed"] != float64(0) || b["kind"] != "undo" || b["status"] != "done" || b["total"] != float64(1) || e.folderOf("order two") != "INBOX" {
		t.Fatalf("undo by rule = %v; order two is in %q", byRule, e.folderOf("order two"))
	}
	all := e.call(http.MethodPost, fmt.Sprintf("/api/actions/undo?since=%d", since), "", http.StatusOK)
	if all["undone"] != float64(1) || e.folderOf("order one") != "INBOX" {
		t.Fatalf("undo since = %v; order one is in %q", all, e.folderOf("order one"))
	}
	if again := e.call(http.MethodPost, fmt.Sprintf("/api/actions/undo?since=%d", since), "", http.StatusOK); again["undone"] != float64(0) {
		t.Errorf("a second undo-since undid %v", again["undone"])
	}

	// A deleted rule keeps its place in the feed, by name.
	e.call(http.MethodDelete, "/api/rules/2", "", http.StatusNoContent)
	if d := e.item("order two", "acted")["decision"].(map[string]any); d["rule_id"] != nil || d["rule_name"] != "Reading" {
		t.Errorf("decision after its rule was deleted = %v", d)
	}
}

// Undoing an email that was moved outside MailRules is refused, and says so.
func TestUndoOfAMessageThatIsGone(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.call(http.MethodPost, "/api/rules/import", rulesYAML, http.StatusOK)
	e.mb.AddFolder("Elsewhere", "")

	e.deliver("noreply@swiggy.in", "order one")
	e.deliver("noreply@swiggy.in", "order two")
	one, two := e.item("order one", "acted"), e.item("order two", "acted")
	ref, err := e.mb.FindByMessageID(t.Context(), "Food", "order-one@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.mb.Move(t.Context(), ref, "Elsewhere"); err != nil { // the user filed it by hand
		t.Fatal(err)
	}
	move := one["actions"].([]any)[0].(map[string]any)
	e.refuse(http.MethodPost, fmt.Sprintf("/api/actions/%d/undo", id(move["id"])), "", http.StatusConflict, "message_gone", "")
	e.refuse(http.MethodPost, fmt.Sprintf("/api/messages/%d/correct", id(one["id"])), `{"rule_id":null}`, http.StatusConflict, "message_gone", "")

	// The day's batch: what can be undone is, the rest is counted, and the batch stays open.
	batchID := id(two["actions"].([]any)[0].(map[string]any)["batch_id"])
	res := e.call(http.MethodPost, fmt.Sprintf("/api/batches/%d/undo", batchID), "", http.StatusOK)
	conform(t, e.doc, "UndoResult", res)
	if b := res["batch"].(map[string]any); res["undone"] != float64(2) || res["failed"] != float64(2) || b["kind"] != "live" || b["status"] != "done" || e.folderOf("order two") != "INBOX" {
		t.Fatalf("batch undo = %v; order two is in %q", res, e.folderOf("order two"))
	}
	e.refuse(http.MethodPost, "/api/batches/99/undo", "", http.StatusNotFound, "not_found", "")
	e.refuse(http.MethodGet, "/api/batches/99", "", http.StatusNotFound, "not_found", "")

	// With the account offline nothing can be put back either.
	e.call(http.MethodPatch, "/api/accounts/1", `{"paused":true}`, http.StatusOK)
	read := one["actions"].([]any)[1].(map[string]any)
	e.refuse(http.MethodPost, fmt.Sprintf("/api/actions/%d/undo", id(read["id"])), "", http.StatusConflict, "account_offline", "")
}

func TestReview(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.call(http.MethodPost, "/api/rules/import", rulesYAML, http.StatusOK)
	// The model is unsure: below the threshold, the email waits in Needs review.
	e.decider.DecideFunc = func(models.DecideRequest) (models.Decision, models.Usage, error) {
		return models.Decision{RuleID: 2, Confidence: 0.4, Reason: "Might be a newsletter"}, models.Usage{Provider: "fake", Model: "fake-1", TokensIn: 300, CostUSD: 0.00002}, nil
	}
	for _, subject := range []string{"weekly digest", "monthly digest", "a third"} {
		e.deliver("hello@news.example", subject)
		e.item(subject, "review")
	}
	queue := e.call(http.MethodGet, "/api/review?limit=2", "", http.StatusOK)
	conform(t, e.doc, "ReviewPage", queue)
	items := queue["items"].([]any)
	first := items[0].(map[string]any)
	if d := first["decision"].(map[string]any); queue["total"] != float64(3) || len(items) != 2 || queue["next_cursor"] == nil || first["subject"] != "a third" ||
		id(d["rule_id"]) != 2 || d["reason"] != "Might be a newsletter" || d["model"] != "fake-1" || d["stage"] != "decider" || first["undoable"] != false {
		t.Fatalf("review queue = %v", queue)
	}

	// Approve the suggestion for one, keep another in the inbox.
	approved := e.call(http.MethodPost, fmt.Sprintf("/api/review/%d/resolve", id(first["id"])), `{"rule_id":2}`, http.StatusOK)
	if approved["item"].(map[string]any)["state"] != "acted" || e.folderOf("a third") != "Reading" {
		t.Fatalf("approve = %v; the email is in %q", approved, e.folderOf("a third"))
	}
	second := items[1].(map[string]any)
	kept := e.call(http.MethodPost, fmt.Sprintf("/api/review/%d/resolve", id(second["id"])), `{"rule_id":null}`, http.StatusOK)
	if acts := kept["item"].(map[string]any)["actions"].([]any); acts[len(acts)-1].(map[string]any)["kind"] != "keep" || e.folderOf("monthly digest") != "INBOX" {
		t.Fatalf("keep = %v", kept)
	}
	if left := e.call(http.MethodGet, "/api/review", "", http.StatusOK); left["total"] != float64(1) {
		t.Errorf("after two were resolved the queue holds %v", left["total"])
	}
	e.refuse(http.MethodPost, fmt.Sprintf("/api/review/%d/resolve", id(first["id"])), `{"rule_id":2}`, http.StatusConflict, "not_in_review", "")
	e.refuse(http.MethodPost, "/api/review/99/resolve", `{"rule_id":2}`, http.StatusNotFound, "not_found", "")
	e.refuse(http.MethodPost, fmt.Sprintf("/api/review/%d/resolve", id(second["id"])+1), `{"rule":2}`, http.StatusConflict, "not_in_review", "")
}

// In dry-run, which is the default, decisions are recorded and the mailbox is not touched.
func TestDryRunIsTheDefault(t *testing.T) {
	e := newEnv(t)
	e.signIn()
	e.call(http.MethodPost, "/api/accounts", accountBody, http.StatusCreated)
	e.live()
	e.call(http.MethodPost, "/api/rules/import", rulesYAML, http.StatusOK)
	e.deliver("noreply@swiggy.in", "order one")
	it := e.item("order one", "acted")
	if a := it["actions"].([]any)[0].(map[string]any); a["status"] != "dry_run" || a["to_folder"] != "" || it["undoable"] != false || e.folderOf("order one") != "INBOX" {
		t.Fatalf("dry-run row = %v; the email is in %q", it, e.folderOf("order one"))
	}
}

func TestSettings(t *testing.T) {
	e := newEnv(t)
	e.signIn()
	ctx := t.Context()

	got := e.call(http.MethodGet, "/api/settings", "", http.StatusOK)
	conform(t, e.doc, "Settings", got)
	for key, want := range map[string]any{"dry_run": true, "decider": "jev", "decider_model": "", "fallback_model": "claude-haiku-4-5",
		"composer_model": "claude-haiku-4-5", "escalate_below": 0.75, "min_confidence": 0.75, "retention_days": float64(30)} {
		if got[key] != want {
			t.Errorf("default %s = %v, want %v", key, got[key], want)
		}
	}
	if srv := got["server"].(map[string]any); srv["version"] != "test" || srv["listen"] != "127.0.0.1:8080" || srv["mode"] != "selfhost" {
		t.Errorf("server = %v", srv)
	}
	for name, on := range got["features"].(map[string]any) {
		if on != false {
			t.Errorf("feature %s is on before its milestone", name)
		}
	}
	for name, set := range got["keys"].(map[string]any) {
		if set != "none" {
			t.Errorf("key %s reads as %v on a fresh install", name, set)
		}
	}
	if router, _ := e.sett.Live(ctx); router != nil {
		t.Fatal("a decision model is in use before any key is set")
	}

	for body, want := range map[string][2]string{
		`{"decider":"gpt"}`:                 {"invalid_input", "decider"},
		`{"decider":"ollama"}`:              {"invalid_input", "decider_model"},
		`{"composer_model":""}`:             {"invalid_input", "composer_model"},
		`{"escalate_below":1.5}`:            {"invalid_input", "escalate_below"},
		`{"min_confidence":-0.1}`:           {"invalid_input", "min_confidence"},
		`{"retention_days":0}`:              {"invalid_input", "retention_days"},
		`{"retention_days":"30"}`:           {"invalid_input", "retention_days"},
		`{"keys":{"stripe_secret":"sk-1"}}`: {"invalid_input", "keys.stripe_secret"},
		`{"listen":"0.0.0.0:80"}`:           {"invalid_json", "listen"},
		`{"server":{"version":"2"}}`:        {"invalid_json", "server"},
	} {
		e.refuse(http.MethodPatch, "/api/settings", body, http.StatusBadRequest, want[0], want[1])
	}
	if after := e.call(http.MethodGet, "/api/settings", "", http.StatusOK); after["decider"] != "jev" || after["retention_days"] != float64(30) {
		t.Errorf("a refused change was stored: %v", after)
	}

	// A key is write-only: stored encrypted, reported as set, never returned.
	const secret = "sk-or-v1-very-secret-key"
	r := e.do(http.MethodPatch, "/api/settings", `{"dry_run":false,"escalate_below":0.6,"min_confidence":0.8,"retention_days":90,"fallback_model":"","keys":{"openrouter_api_key":"`+secret+`"}}`)
	if r.status != http.StatusOK || strings.Contains(string(r.raw), secret) {
		t.Fatalf("patch = %d %s", r.status, r.raw)
	}
	got = r.object(t)
	conform(t, e.doc, "Settings", got)
	if got["dry_run"] != false || got["escalate_below"] != 0.6 || got["min_confidence"] != 0.8 || got["retention_days"] != float64(90) ||
		got["fallback_model"] != "" || got["keys"].(map[string]any)["openrouter_api_key"] != "stored" || got["keys"].(map[string]any)["anthropic_api_key"] != "none" {
		t.Errorf("after patch = %v", got)
	}
	if strings.Contains(string(e.do(http.MethodGet, "/api/settings", "").raw), secret) {
		t.Error("GET /api/settings returns the key")
	}
	rows, err := e.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	stored := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		stored[k] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if v := stored["key.openrouter_api_key"]; v == "" || strings.Contains(fmt.Sprint(stored), secret) {
		t.Fatalf("the key is missing from the settings table or stored in plain text: %v", stored)
	}
	if cfg, err := e.sett.Effective(ctx); err != nil || cfg.OpenRouterAPIKey != secret {
		t.Fatalf("the stored key does not decrypt: %v", err)
	}
	// The stored key is bound to its name: moved to another row, it does not open.
	if _, err := e.db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES ('key.anthropic_api_key', ?)`, stored["key.openrouter_api_key"]); err != nil {
		t.Fatal(err)
	}
	if _, err := e.sett.Effective(ctx); err == nil {
		t.Error("a key moved to another row still decrypts")
	}
	if _, err := e.db.ExecContext(ctx, `DELETE FROM settings WHERE key = 'key.anthropic_api_key'`); err != nil {
		t.Fatal(err)
	}

	// The change is in force at once, with no restart: the next email gets a router and the new threshold.
	router, minConfidence := e.sett.Live(ctx)
	if router == nil || router.Name() != "jev" || router.Fallback != nil || router.EscalateBelow != 0.6 || minConfidence != 0.8 {
		t.Fatalf("after the key was set: router %v, threshold %v", router, minConfidence)
	}
	if same, _ := e.sett.Live(ctx); same != router {
		t.Error("the router was rebuilt although nothing changed")
	}
	if on, _ := e.st.DryRun(ctx, true); on {
		t.Error("the executor still sees dry-run on")
	}
	// Removing the stored key puts the environment's (none here) back in force.
	e.call(http.MethodPatch, "/api/settings", `{"keys":{"openrouter_api_key":""}}`, http.StatusOK)
	if router, _ := e.sett.Live(ctx); router != nil {
		t.Error("the decision model is still in use after its key was removed")
	}
}

// stream opens the event stream over a real connection and returns its lines.
func (e *env) stream(t *testing.T, url, lastEventID string) (lines <-chan string, status int) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range e.cookies {
		req.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	ch := make(chan string, 100)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			if line := sc.Text(); line != "" && !strings.HasPrefix(line, ":") {
				ch <- line
			}
		}
	}()
	if resp.StatusCode == http.StatusOK && resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("Content-Type = %q", resp.Header.Get("Content-Type"))
	}
	return ch, resp.StatusCode
}

// next reads one event: its id, name and data lines.
func next(t *testing.T, lines <-chan string) (eventID, name string, data map[string]any) {
	t.Helper()
	var got [3]string
	for i, prefix := range []string{"id: ", "event: ", "data: "} {
		select {
		case line, ok := <-lines:
			if !ok || !strings.HasPrefix(line, prefix) {
				t.Fatalf("stream line %q, want one starting %q", line, prefix)
			}
			got[i] = strings.TrimPrefix(line, prefix)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for the %q line of an event", prefix)
		}
	}
	if err := json.Unmarshal([]byte(got[2]), &data); err != nil {
		t.Fatalf("event data is not a JSON object: %s", got[2])
	}
	return got[0], got[1], data
}

func TestEventStream(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(e.h)
	t.Cleanup(srv.Close)

	if _, status := e.stream(t, srv.URL, ""); status != http.StatusUnauthorized {
		t.Fatalf("stream without a session = %d", status)
	}
	e.hub.Publish(events.UsageUpdated, nil) // event 1, before anyone listens
	e.connect()                             // account.status events as the account goes live
	e.call(http.MethodPost, "/api/rules/import", rulesYAML, http.StatusOK)
	published := 0
	missed, _, cancel := e.hub.Subscribe(-1)
	cancel()
	published = len(missed)

	// A new client is sent only what happens from now on.
	lines, status := e.stream(t, srv.URL, "")
	if status != http.StatusOK {
		t.Fatalf("stream = %d", status)
	}
	time.Sleep(20 * time.Millisecond) // let the handler subscribe before the mail arrives
	e.deliver("noreply@swiggy.in", "order one")
	eventID, name, data := next(t, lines)
	conform(t, e.doc, "EventMessageProcessed", data)
	if eventID != fmt.Sprint(published+1) || name != events.MessageProcessed || data["subject"] != "order one" || len(data["actions"].([]any)) != 2 {
		t.Fatalf("live event = id %s %s %v (after %d earlier events)", eventID, name, data, published)
	}
	move := data["actions"].([]any)[0].(map[string]any)
	e.call(http.MethodPost, fmt.Sprintf("/api/actions/%d/undo", id(move["id"])), "", http.StatusOK)
	if _, name, data := next(t, lines); name != events.ActionUndone || data["status"] != "undone" || id(data["id"]) != id(move["id"]) {
		t.Fatalf("undo event = %s %v", name, data)
	} else {
		conform(t, e.doc, "EventActionUndone", data)
	}
	e.call(http.MethodPatch, "/api/rules/1", `{"enabled":false}`, http.StatusOK)
	if _, name, data := next(t, lines); name != events.RulesChanged || len(data) != 0 {
		t.Fatalf("rules event = %s %v", name, data)
	}

	// A client that reconnects with Last-Event-ID is first sent what it missed, in order.
	replay, _ := e.stream(t, srv.URL, "1")
	seen := map[string]bool{}
	for want := 2; want <= published+3; want++ {
		eventID, name, data := next(t, replay)
		if eventID != fmt.Sprint(want) {
			t.Fatalf("replayed id %s, want %d", eventID, want)
		}
		if name == events.AccountStatus {
			conform(t, e.doc, "EventAccountStatus", data)
		}
		seen[name] = true
	}
	for _, name := range []string{events.AccountStatus, events.RulesChanged, events.MessageProcessed, events.ActionUndone} {
		if !seen[name] {
			t.Errorf("the replay did not include %s", name)
		}
	}
	// And then goes on live.
	e.hub.Publish(events.UsageUpdated, nil)
	if eventID, name, _ := next(t, replay); eventID != fmt.Sprint(published+4) || name != events.UsageUpdated {
		t.Errorf("after the replay: id %s %s", eventID, name)
	}
	// An id from before a restart (ahead of the hub) replays everything kept.
	if eventID, _, _ := next(t, func() <-chan string { ch, _ := e.stream(t, srv.URL, "999999"); return ch }()); eventID != "1" {
		t.Errorf("a stale Last-Event-ID replayed from id %s, want 1", eventID)
	}
}
