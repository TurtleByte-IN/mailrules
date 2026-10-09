package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/mailtest"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/worker"
)

const (
	testProvider = "testsso"
	signInPath   = "/api/testsso/start"
	signOutPath  = "/api/testsso/end"
	webhookPath  = "/api/testsso/webhook"
	pingPath     = "/api/testsso/ping" // a public POST that is not a webhook
)

// signInModule stands in for the hosted build's sign-in module. Its sign-in route trusts
// the identity in the query (sub, email, org), where the real one asks the sign-in service;
// its webhook ends the sessions of ?sub=.
func signInModule() ext.Module {
	return ext.Module{
		Name: "testsso", SignIn: signInPath, SignOut: signOutPath,
		Routes: func(h ext.Host) []ext.Route {
			return []ext.Route{
				{Method: http.MethodGet, Path: signInPath, Public: true, Handler: func(w http.ResponseWriter, r *http.Request) {
					q := r.URL.Query()
					_, err := h.SignIn(w, r, ext.Identity{Provider: testProvider, Subject: q.Get("sub"), Email: q.Get("email"), Tenant: q.Get("org")})
					switch {
					case errors.Is(err, ext.ErrTenantMismatch):
						h.SignInFailed(w, r, ext.SignInTenantMismatch)
					case errors.Is(err, ext.ErrEmailInUse):
						h.SignInFailed(w, r, ext.SignInEmailInUse)
					case err != nil:
						h.SignInFailed(w, r, ext.SignInRefused)
					default:
						http.Redirect(w, r, "/", http.StatusSeeOther)
					}
				}},
				{Method: http.MethodGet, Path: signOutPath, Public: true, Handler: func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, "/", http.StatusSeeOther)
				}},
				{Method: http.MethodPost, Path: webhookPath, Public: true, Webhook: true, Handler: func(w http.ResponseWriter, r *http.Request) {
					if err := h.EndSessions(r.Context(), testProvider, r.URL.Query().Get("sub")); err != nil {
						h.Fail(w, r, err)
						return
					}
					w.WriteHeader(http.StatusNoContent)
				}},
				{Method: http.MethodPost, Path: pingPath, Public: true, Handler: func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusNoContent)
				}},
			}
		},
	}
}

// browser is another browser on the same daemon: e's helpers, with its own cookies.
func (e *env) browser() *env {
	cp := *e
	cp.client = &client{t: e.t, h: e.h, cookies: map[string]string{}}
	return &cp
}

// signInAs signs this browser in through the test module and returns where it was sent.
func (e *env) signInAs(sub, email, org string) string {
	e.t.Helper()
	q := url.Values{"sub": {sub}, "email": {email}, "org": {org}}
	r := e.do(http.MethodGet, signInPath+"?"+q.Encode(), "")
	if r.status != http.StatusSeeOther {
		e.t.Fatalf("sign in as %s = %d %s", sub, r.status, r.raw)
	}
	return r.header.Get("Location")
}

// me is this browser's GET /api/auth/me 200 body.
func (e *env) me() map[string]any {
	e.t.Helper()
	return e.call(http.MethodGet, "/api/auth/me", "", http.StatusOK)
}

func TestCheckModules(t *testing.T) {
	ok := func(http.ResponseWriter, *http.Request) {}
	routes := func(rs ...ext.Route) func(ext.Host) []ext.Route {
		return func(ext.Host) []ext.Route { return rs }
	}
	start := ext.Route{Method: http.MethodGet, Path: "/api/s/start", Public: true, Handler: ok}
	end := ext.Route{Method: http.MethodGet, Path: "/api/s/end", Public: true, Handler: ok}
	cases := []struct {
		name    string
		mode    string
		modules []ext.Module
		want    string // a part of the error; empty = accepted
	}{
		{"self-host without modules", "selfhost", nil, ""},
		{"the test module", "cloud", []ext.Module{signInModule()}, ""},
		{"cloud without a sign-in module", "cloud", []ext.Module{{Name: "suggest", Routes: routes()}}, "MAILRULES_MODE=cloud"},
		{"a webhook that is not public", "selfhost", []ext.Module{{Name: "m", Routes: routes(ext.Route{Method: http.MethodPost, Path: "/api/s/hook", Webhook: true, Handler: ok})}}, "Webhook but not Public"},
		{"SignIn names no route", "selfhost", []ext.Module{{Name: "m", SignIn: "/api/s/nope", Routes: routes(start)}}, `SignIn "/api/s/nope"`},
		{"SignIn names a route that is not public", "selfhost", []ext.Module{{Name: "m", SignIn: "/api/s/start", Routes: routes(ext.Route{Method: http.MethodGet, Path: "/api/s/start", Handler: ok})}}, "SignIn"},
		{"SignIn names a POST", "selfhost", []ext.Module{{Name: "m", SignIn: "/api/s/start", Routes: routes(ext.Route{Method: http.MethodPost, Path: "/api/s/start", Public: true, Handler: ok})}}, "SignIn"},
		{"SignOut names no route", "selfhost", []ext.Module{{Name: "m", SignIn: "/api/s/start", SignOut: "/api/s/gone", Routes: routes(start)}}, `SignOut "/api/s/gone"`},
		{"SignOut without SignIn", "selfhost", []ext.Module{{Name: "m", SignOut: "/api/s/end", Routes: routes(end)}}, "SignOut is set without SignIn"},
		{"two sign-in modules", "cloud", []ext.Module{
			{Name: "one", SignIn: "/api/s/start", Routes: routes(start)},
			{Name: "two", SignIn: "/api/s/end", Routes: routes(end)}}, "more than one module"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckModules(c.mode, c.modules)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("err = %v, want one saying %q", err, c.want)
			}
		})
	}
}

func TestModuleSignIn(t *testing.T) {
	e := newEnv(t, signInModule())
	ctx := t.Context()

	// Signed out: the screen is the module's sign-in, never setup, and there is no password
	// sign-in, setup or password change.
	e.do(http.MethodGet, "/api/auth/me", "")
	for _, path := range []string{"/api/auth/me", "/api/accounts"} {
		r := e.do(http.MethodGet, path, "")
		if r.status != http.StatusUnauthorized || r.body.Error.Code != "unauthenticated" || r.body.Error.SignIn != signInPath {
			t.Fatalf("GET %s signed out = %d %s", path, r.status, r.raw)
		}
	}
	for path, body := range map[string]string{"/api/auth/setup": goodBody, "/api/auth/login": goodBody, "/api/auth/password": passwordBody("x", "y")} {
		e.refuse(http.MethodPost, path, body, http.StatusNotFound, "not_available", "")
	}

	// A first sign-in makes the tenant and a user who has no password.
	if to := e.signInAs("sub-1", "ana@alpha.test", "org-alpha"); to != "/" {
		t.Fatalf("signed in, sent to %q", to)
	}
	me := e.me()
	conform(t, e.doc, "Session", me)
	user := me["user"].(map[string]any)
	if user["email"] != "ana@alpha.test" || me["members"] != float64(1) || me["sign_out"] != signOutPath {
		t.Fatalf("me = %v", me)
	}
	ana := id(user["id"])
	u, err := e.st.User(ctx, ana)
	if err != nil || u.PasswordHash != "" || u.TenantID == store.SelfHostTenant {
		t.Fatalf("user = %+v, %v", u, err)
	}
	// Signed in, the password routes are still not there.
	e.refuse(http.MethodPost, "/api/auth/password", passwordBody("x", "correct horse battery staple"), http.StatusNotFound, "not_available", "")

	// Steps on a second browser; each checks what the sign-in left behind.
	tenants := func() int { return e.count(`SELECT COUNT(*) FROM tenants`) }
	steps := []struct {
		name, sub, email, org string
		to                    string // where the browser is sent
		check                 func(t *testing.T, b *env)
	}{
		{"the same person again, by subject, with a new email", "sub-1", "ana@new.test", "org-alpha", "/", func(t *testing.T, b *env) {
			if got := b.me()["user"].(map[string]any); id(got["id"]) != ana || got["email"] != "ana@new.test" {
				t.Errorf("returning user = %v, want user %d with the new email", got, ana)
			}
		}},
		{"a teammate", "sub-2", "ben@alpha.test", "org-alpha", "/", func(t *testing.T, b *env) {
			if m := b.me(); m["members"] != float64(2) {
				t.Errorf("teammate's members = %v", m["members"])
			}
		}},
		{"a returning person under another organisation", "sub-1", "ana@new.test", "org-other", "/?signin_error=tenant_mismatch", nil},
		{"an email another user has", "sub-3", "ben@alpha.test", "org-third", "/?signin_error=email_in_use", nil},
		{"an identity with no organisation", "sub-4", "cy@alpha.test", "", "/?signin_error=refused", nil},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			b := e.browser()
			b.t = t
			before := tenants()
			if to := b.signInAs(s.sub, s.email, s.org); to != s.to {
				t.Fatalf("sent to %q, want %q", to, s.to)
			}
			if s.check != nil {
				s.check(t, b)
				return
			}
			// A refused sign-in changes nothing and starts no session.
			if after := tenants(); after != before {
				t.Errorf("tenants went from %d to %d", before, after)
			}
			if r := b.do(http.MethodGet, "/api/auth/me", ""); r.status != http.StatusUnauthorized {
				t.Errorf("after a refused sign-in me = %d", r.status)
			}
		})
	}
	if got, _ := e.st.User(ctx, ana); got.Email != "ana@new.test" || got.TenantID != u.TenantID {
		t.Errorf("after the refusals the user is %+v", got)
	}

	// A public POST that is not a webhook still needs the CSRF header; with it, no session.
	anon := e.browser()
	anon.do(http.MethodGet, "/api/auth/me", "")
	anon.noCSRF = true
	expect(t, anon.do(http.MethodPost, pingPath, ""), http.StatusForbidden, "csrf_failed")
	anon.noCSRF = false
	if r := anon.do(http.MethodPost, pingPath, ""); r.status != http.StatusNoContent {
		t.Errorf("public POST with the token = %d", r.status)
	}

	// The webhook, called by another server with no cookie and no token, signs Ana out of
	// every browser at once; her teammate stays in.
	second := e.browser()
	second.signInAs("sub-1", "ana@new.test", "org-alpha")
	ben := e.browser()
	ben.signInAs("sub-2", "ben@alpha.test", "org-alpha")
	server := &client{t: t, h: e.h, cookies: map[string]string{}, noCSRF: true}
	for _, sub := range []string{"sub-1", "nobody"} {
		if r := server.do(http.MethodPost, webhookPath+"?sub="+sub, ""); r.status != http.StatusNoContent {
			t.Fatalf("webhook for %s = %d %s", sub, r.status, r.raw)
		}
	}
	for _, b := range []*env{e, second} {
		if r := b.do(http.MethodGet, "/api/accounts", ""); r.status != http.StatusUnauthorized || r.body.Error.SignIn != signInPath {
			t.Errorf("after the webhook a session of Ana's answers %d %s", r.status, r.raw)
		}
	}
	ben.call(http.MethodGet, "/api/accounts", "", http.StatusOK)

	// Logging out ends the session; the browser then goes to sign_out.
	ben.call(http.MethodPost, "/api/auth/logout", "", http.StatusNoContent)
	if r := ben.do(http.MethodGet, "/api/auth/me", ""); r.status != http.StatusUnauthorized {
		t.Errorf("after logout me = %d", r.status)
	}
	if r := ben.do(http.MethodGet, signOutPath, ""); r.status != http.StatusSeeOther {
		t.Errorf("sign_out route = %d", r.status)
	}
}

// A user with no password (one made from an identity) cannot sign in with one, whatever
// password is tried, in a build with password sign-in too.
func TestNoPasswordUserCannotLogIn(t *testing.T) {
	e := newEnv(t)
	e.signIn()
	ctx := t.Context()
	if _, err := e.st.CreateUser(ctx, store.SelfHostTenant, "sso@example.test", "", e.ck.now().Unix()); err != nil {
		t.Fatal(err)
	}
	other := e.browser()
	other.do(http.MethodGet, "/api/auth/me", "")
	for _, pw := range []string{"", "no such account", "correct horse battery"} {
		body, _ := json.Marshal(credentials{Email: "sso@example.test", Password: pw})
		other.refuse(http.MethodPost, "/api/auth/login", string(body), http.StatusUnauthorized, "invalid_credentials", "")
	}
	// Nor change one: there is no current password to give.
	ids, err := e.st.UserByEmail(ctx, "sso@example.test")
	if err != nil {
		t.Fatal(err)
	}
	hash := hashToken("sso-session")
	if err := e.st.CreateSession(ctx, hash, ids.ID, e.ck.now().Unix(), e.ck.now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	other.cookies[sessionCookie] = "sso-session"
	other.refuse(http.MethodPost, "/api/auth/password", passwordBody("", "a brand new password"), http.StatusBadRequest, "invalid_input", "current_password")
	if got, _ := e.st.User(ctx, ids.ID); got.PasswordHash != "" {
		t.Error("a user with no password was given one")
	}
}

// tenantAccount connects a fresh fake mailbox for the signed-in browser and waits until it
// is live; it returns the account id.
func (e *env) tenantAccount(username string) int64 {
	e.t.Helper()
	// The fake stamps the account id on every email it lists: the next one the store gives.
	mb := mailtest.New(int64(e.count(`SELECT COALESCE(MAX(id), 0) + 1 FROM accounts`)))
	for name, role := range map[string]string{"Trash": mail.RoleTrash, "Archive": mail.RoleArchive, "Sent": mail.RoleSent} {
		mb.AddFolder(name, role)
	}
	e.boxes[username] = mb
	body := fmt.Sprintf(`{"preset":"generic","host":"imap.example.test","username":%q,"password":"app-password-1234"}`, username)
	acct := id(e.call(http.MethodPost, "/api/accounts", body, http.StatusCreated)["account"].(map[string]any)["id"])
	eventually(e.t, username+" to be live", func() bool {
		a, err := e.st.Account(e.t.Context(), acct)
		_, mbErr := e.mgr.Mailbox(acct)
		return err == nil && a.Status == worker.StatusLive && mbErr == nil
	})
	return acct
}

// deliverTo delivers an email to the fake mailbox of username.
func (e *env) deliverTo(username, from, subject string) {
	e.boxes[username].Deliver("INBOX", "From: "+from+"\r\nTo: "+username+"\r\nSubject: "+subject+
		"\r\nMessage-ID: <"+strings.ReplaceAll(subject, " ", "-")+"@example.test>\r\n\r\nThe body of "+subject+".\r\n")
}

// tenantOf is the tenant of the user with this email.
func (e *env) tenantOf(email string) int64 {
	e.t.Helper()
	u, err := e.st.UserByEmail(e.t.Context(), email)
	if err != nil {
		e.t.Fatal(err)
	}
	return u.TenantID
}

// nextEvent reads the next event of a stream, or fails after a short wait.
func nextEvent(t *testing.T, lines <-chan string) (eventID, name, data string) {
	t.Helper()
	var got [3]string
	for i, prefix := range []string{"id: ", "event: ", "data: "} {
		select {
		case line, ok := <-lines:
			if !ok || !strings.HasPrefix(line, prefix) {
				t.Fatalf("stream line %q, want one starting %q", line, prefix)
			}
			got[i] = strings.TrimPrefix(line, prefix)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for the %q line of an event", prefix)
		}
	}
	return got[0], got[1], got[2]
}

// One tenant can neither read nor change anything of another's over HTTP: its mailbox and
// what hangs off it, its rules, senders, settings, usage, cleanup and live events. What
// the other tenant has answers as missing (404, or 400 at account_id where a missing
// account is answered so) or is simply not in a list. Everything of tenant A is named
// "alpha", so no response to B may contain the word.
func TestTenantIsolation(t *testing.T) {
	e := newEnv(t, signInModule())
	e.decider.DecideFunc = cleanupDeciderFunc
	a, b := e.browser(), e.browser()
	a.signInAs("alpha-1", "ana@alpha.test", "org-alpha")
	b.signInAs("bravo-1", "bob@bravo.test", "org-bravo")
	tenantA, tenantB := e.tenantOf("ana@alpha.test"), e.tenantOf("bob@bravo.test")
	if tenantA == tenantB {
		t.Fatal("two organisations share a tenant")
	}

	// Tenant A: settings, a mailbox, rules, a sender rule, sorted mail, a review, a cleanup run.
	a.call(http.MethodPatch, "/api/settings", `{"dry_run":false,"retention_days":90}`, http.StatusOK)
	acctA := a.tenantAccount("ana@alpha.test")
	a.call(http.MethodPost, "/api/rules/batch", strings.ReplaceAll(cleanupRules, `"Food"`, `"alpha Food"`), http.StatusCreated)
	ruleA := id(a.call(http.MethodGet, "/api/rules", "", http.StatusOK)["items"].([]any)[0].(map[string]any)["id"])
	a.call(http.MethodPut, "/api/senders/domain/alphajunk.example", `{"verdict":"block"}`, http.StatusOK)
	e.deliverTo("ana@alpha.test", "noreply@swiggy.in", "alpha order")
	sorted := a.item("alpha order", "acted")
	msgA := id(sorted["id"])
	actA := id(sorted["actions"].([]any)[0].(map[string]any)["id"])
	e.deliverTo("ana@alpha.test", "hello@news.example", "alpha maybe reading")
	reviewA := id(a.item("alpha maybe reading", "review")["id"])
	for i := range 3 {
		e.deliverTo("ana@alpha.test", "hello@news.example", fmt.Sprintf("alpha old reading %d", i))
	}
	chk := a.checkReady(fmt.Sprintf(`{"account_id":%d}`, acctA))
	run, _ := json.Marshal(map[string]any{"account_id": acctA, "check_id": chk["id"]})
	batchA := id(a.call(http.MethodPost, "/api/cleanup/run", string(run), http.StatusAccepted)["batch"].(map[string]any)["id"])
	a.batchDone(batchA)
	if u := a.call(http.MethodGet, "/api/stats/usage", "", http.StatusOK); u["calls"] == float64(0) {
		t.Fatalf("tenant A booked no model calls, so the usage check below proves nothing: %v", u)
	}

	// Tenant B: its own mailbox, rules and one sorted email.
	b.call(http.MethodPatch, "/api/settings", `{"dry_run":false}`, http.StatusOK)
	acctB := b.tenantAccount("bob@bravo.test")
	b.call(http.MethodPost, "/api/rules/import", rulesYAML, http.StatusOK)
	e.deliverTo("bob@bravo.test", "noreply@swiggy.in", "bravo order")
	b.item("bravo order", "acted")

	acct := func(path string) string { return fmt.Sprintf(path, acctA) }
	msg := func(path string) string { return fmt.Sprintf(path, msgA) }
	rule := func(path string) string { return fmt.Sprintf(path, ruleA) }
	refusals := []struct {
		method, path, body string
		status             int
		code, errPath      string
	}{
		{http.MethodGet, acct("/api/accounts/%d"), "", 404, "not_found", ""},
		{http.MethodPatch, acct("/api/accounts/%d"), `{"label":"mine now"}`, 404, "not_found", ""},
		{http.MethodPatch, acct("/api/accounts/%d"), `{"shared":true}`, 404, "not_found", ""},
		{http.MethodDelete, acct("/api/accounts/%d"), "", 404, "not_found", ""},
		{http.MethodGet, acct("/api/accounts/%d/folders"), "", 404, "not_found", ""},
		{http.MethodPost, acct("/api/accounts/%d/test"), "", 404, "not_found", ""},
		{http.MethodPost, acct("/api/accounts/%d/reconnect"), "", 404, "not_found", ""},
		{http.MethodGet, msg("/api/messages/%d"), "", 404, "not_found", ""},
		{http.MethodPost, msg("/api/messages/%d/correct"), `{"rule_id":null}`, 404, "not_found", ""},
		{http.MethodPost, msg("/api/messages/%d/undo"), "", 404, "not_found", ""},
		{http.MethodPost, fmt.Sprintf("/api/review/%d/resolve", reviewA), `{"rule_id":null}`, 404, "not_found", ""},
		{http.MethodPost, fmt.Sprintf("/api/actions/%d/undo", actA), "", 404, "not_found", ""},
		{http.MethodGet, fmt.Sprintf("/api/batches/%d", batchA), "", 404, "not_found", ""},
		{http.MethodPost, fmt.Sprintf("/api/batches/%d/undo", batchA), "", 404, "not_found", ""},
		{http.MethodGet, rule("/api/rules/%d"), "", 404, "not_found", ""},
		{http.MethodPatch, rule("/api/rules/%d"), `{"enabled":false}`, 404, "not_found", ""},
		{http.MethodDelete, rule("/api/rules/%d"), "", 404, "not_found", ""},
		{http.MethodPost, rule("/api/rules/%d/compose"), `{"text":"and mark it read"}`, 404, "not_found", ""},
		{http.MethodPost, rule("/api/rules/%d/undo?since=1"), "", 404, "not_found", ""},
		{http.MethodPost, "/api/rules/test", fmt.Sprintf(`{"account_id":%d}`, acctA), 400, "invalid_input", "account_id"},
		{http.MethodPost, "/api/rules/test", fmt.Sprintf(`{"account_id":%d,"rule_ids":[%d]}`, acctB, ruleA), 400, "invalid_input", "rule_ids"},
		{http.MethodPost, "/api/rules/compose", fmt.Sprintf(`{"text":"Put Swiggy in Food","account_id":%d}`, acctA), 400, "invalid_input", "account_id"},
		{http.MethodGet, acct("/api/cleanup/check?account_id=%d"), "", 400, "invalid_input", "account_id"},
		{http.MethodDelete, acct("/api/cleanup/check?account_id=%d"), "", 400, "invalid_input", "account_id"},
		{http.MethodPost, "/api/cleanup/check", fmt.Sprintf(`{"account_id":%d}`, acctA), 400, "invalid_input", "account_id"},
		{http.MethodPut, "/api/cleanup/check/selection", fmt.Sprintf(`{"account_id":%d,"check_id":%q,"exclude":[]}`, acctA, chk["id"]), 400, "invalid_input", "account_id"},
		{http.MethodPost, "/api/cleanup/run", string(run), 400, "invalid_input", "account_id"},
	}
	for _, r := range refusals {
		b.refuse(r.method, r.path, r.body, r.status, r.code, r.errPath)
	}

	// Lists and totals carry nothing of A's.
	for _, path := range []string{"/api/accounts", "/api/activity", acct("/api/activity?account=%d"), "/api/review", "/api/batches",
		"/api/rules", "/api/rules/export", "/api/senders", "/api/senders?source=user", "/api/settings", "/api/stats/summary",
		"/api/stats/usage", "/api/cleanup/checks", "/api/summary/preview"} {
		r := b.do(http.MethodGet, path, "")
		if r.status != http.StatusOK || strings.Contains(strings.ToLower(string(r.raw)), "alpha") {
			t.Errorf("B's GET %s = %d %s", path, r.status, r.raw)
		}
	}
	if q := b.call(http.MethodGet, "/api/review", "", http.StatusOK); q["total"] != float64(0) {
		t.Errorf("B's review queue = %v", q)
	}
	if u := b.call(http.MethodGet, "/api/stats/usage", "", http.StatusOK); u["calls"] != float64(0) || u["cost_usd"] != float64(0) || u["processed"] != float64(1) {
		t.Errorf("B's usage = calls %v, cost %v, processed %v; want 0, 0, 1", u["calls"], u["cost_usd"], u["processed"])
	}
	if s := b.call(http.MethodGet, "/api/stats/summary", "", http.StatusOK); s["counts"].(map[string]any)["processed"] != float64(1) || len(s["accounts"].([]any)) != 1 {
		t.Errorf("B's summary = %v", s)
	}

	// B's undo-everything and settings change leave A alone.
	if all := b.call(http.MethodPost, "/api/actions/undo?since=1", "", http.StatusOK); all["undone"] != float64(2) {
		t.Errorf("B's undo since undid %v, want only its own 2 (the move and the mark read)", all["undone"])
	}
	if got := a.call(http.MethodGet, msg("/api/messages/%d"), "", http.StatusOK)["message"].(map[string]any); got["current_folder"] != "alpha Food" {
		t.Errorf("after B's undo A's email is in %v", got["current_folder"])
	}
	if got := b.call(http.MethodGet, "/api/settings", "", http.StatusOK); got["retention_days"] != float64(30) {
		t.Errorf("B's settings show A's: dry_run %v, retention_days %v", got["dry_run"], got["retention_days"])
	}
	b.call(http.MethodPatch, "/api/settings", `{"retention_days":60}`, http.StatusOK)
	if got := a.call(http.MethodGet, "/api/settings", "", http.StatusOK); got["dry_run"] != false || got["retention_days"] != float64(90) {
		t.Errorf("B's change reached A: dry_run %v, retention_days %v", got["dry_run"], got["retention_days"])
	}
	// A still has all of it.
	a.call(http.MethodGet, rule("/api/rules/%d"), "", http.StatusOK)
	a.call(http.MethodGet, fmt.Sprintf("/api/batches/%d", batchA), "", http.StatusOK)
	a.call(http.MethodGet, acct("/api/accounts/%d"), "", http.StatusOK)

	// Live events: B's stream gets none of A's, account-scoped or not, before its own.
	srv := httptest.NewServer(e.h)
	t.Cleanup(srv.Close)
	lines, status := b.stream(t, srv.URL, "")
	if status != http.StatusOK {
		t.Fatalf("B's stream = %d", status)
	}
	time.Sleep(20 * time.Millisecond) // let the handler subscribe
	e.hub.Publish(tenantA, acctA, events.AccountStatus, store.Account{ID: acctA, Label: "alpha"})
	e.hub.Publish(tenantA, 0, events.RulesChanged, nil)
	e.hub.Publish(tenantA, 0, events.UsageUpdated, nil)
	e.hub.Publish(0, 0, events.UsageUpdated, nil)            // no tenant: nobody's
	e.hub.Publish(tenantB, acctA, events.AccountStatus, nil) // B's tenant, but not a mailbox B sees
	e.hub.Publish(tenantB, 0, events.RulesChanged, nil)
	kept, _, cancel := e.hub.Subscribe(-1)
	cancel()
	published := map[string]events.Event{}
	for _, ev := range kept {
		published[fmt.Sprint(ev.ID)] = ev
	}
	last := fmt.Sprint(kept[len(kept)-1].ID)
	if eventID, name, _ := nextEvent(t, lines); eventID != last || name != events.RulesChanged {
		t.Fatalf("B's first event = %s %s, want its own rules.changed, id %s", eventID, name, last)
	}
	// A reconnecting stream's replay is filtered the same way: every event it carries is of
	// B's tenant and of no mailbox or B's own.
	replay, _ := b.stream(t, srv.URL, "1")
	for n := 0; ; n++ {
		eventID, name, data := nextEvent(t, replay)
		if ev := published[eventID]; ev.TenantID != tenantB || (ev.AccountID != 0 && ev.AccountID != acctB) || strings.Contains(data, "alpha") {
			t.Fatalf("B's replay carried event %s %s (tenant %d, account %d): %s", eventID, name, ev.TenantID, ev.AccountID, data)
		}
		if eventID == last {
			if n == 0 {
				t.Error("the replay carried none of B's earlier events, so it checked nothing")
			}
			break
		}
	}
}

// A teammate sees a mailbox shared with the team, its mail and its events, and not a
// private one; only the owner manages a mailbox or shares it.
func TestTeammateAndSharedMailbox(t *testing.T) {
	e := newEnv(t, signInModule())
	owner, mate := e.browser(), e.browser()
	owner.signInAs("ana", "ana@alpha.test", "org-alpha")
	mate.signInAs("ben", "ben@alpha.test", "org-alpha")
	tenant := e.tenantOf("ana@alpha.test")
	if e.tenantOf("ben@alpha.test") != tenant {
		t.Fatal("teammates are in different tenants")
	}
	shared := owner.tenantAccount("team@alpha.test")
	private := owner.tenantAccount("ana@alpha.test")
	owner.call(http.MethodPost, "/api/rules/import", rulesYAML, http.StatusOK)
	e.deliverTo("team@alpha.test", "noreply@swiggy.in", "team order")
	e.deliverTo("ana@alpha.test", "noreply@swiggy.in", "private order")
	teamMsg := id(owner.item("team order", "acted")["id"])
	privMsg := id(owner.item("private order", "acted")["id"])

	// Before sharing the teammate sees neither.
	mate.refuse(http.MethodGet, fmt.Sprintf("/api/accounts/%d", shared), "", http.StatusNotFound, "not_found", "")
	// Only the owner shares it.
	mate.refuse(http.MethodPatch, fmt.Sprintf("/api/accounts/%d", shared), `{"shared":true}`, http.StatusNotFound, "not_found", "")
	got := owner.call(http.MethodPatch, fmt.Sprintf("/api/accounts/%d", shared), `{"shared":true}`, http.StatusOK)["account"].(map[string]any)
	if got["shared"] != true || got["mine"] != true {
		t.Fatalf("shared account = %v", got)
	}

	seen := mate.call(http.MethodGet, fmt.Sprintf("/api/accounts/%d", shared), "", http.StatusOK)["account"].(map[string]any)
	if seen["shared"] != true || seen["mine"] != false {
		t.Errorf("teammate's view = %v", seen)
	}
	list := mate.call(http.MethodGet, "/api/accounts", "", http.StatusOK)["items"].([]any)
	if len(list) != 1 || id(list[0].(map[string]any)["id"]) != shared {
		t.Errorf("teammate's mailboxes = %v", list)
	}
	mate.call(http.MethodGet, fmt.Sprintf("/api/messages/%d", teamMsg), "", http.StatusOK)
	activity := string(mate.do(http.MethodGet, "/api/activity", "").raw)
	if !strings.Contains(activity, "team order") || strings.Contains(activity, "private order") {
		t.Errorf("teammate's activity = %s", activity)
	}
	for _, r := range []struct{ method, path, body string }{
		{http.MethodGet, fmt.Sprintf("/api/accounts/%d", private), ""},
		{http.MethodGet, fmt.Sprintf("/api/messages/%d", privMsg), ""},
		{http.MethodPatch, fmt.Sprintf("/api/accounts/%d", shared), `{"label":"ours"}`},
		{http.MethodPatch, fmt.Sprintf("/api/accounts/%d", shared), `{"shared":false}`},
		{http.MethodDelete, fmt.Sprintf("/api/accounts/%d", shared), ""},
		{http.MethodPost, fmt.Sprintf("/api/accounts/%d/reconnect", shared), ""},
	} {
		mate.refuse(r.method, r.path, r.body, http.StatusNotFound, "not_found", "")
	}

	// Events: the teammate gets the shared mailbox's, not the private one's.
	srv := httptest.NewServer(e.h)
	t.Cleanup(srv.Close)
	lines, _ := mate.stream(t, srv.URL, "")
	time.Sleep(20 * time.Millisecond)
	e.hub.Publish(tenant, private, events.AccountStatus, store.Account{ID: private, TenantID: tenant})
	e.hub.Publish(tenant, shared, events.AccountStatus, store.Account{ID: shared, TenantID: tenant, Shared: true})
	if _, name, data := nextEvent(t, lines); name != events.AccountStatus || !strings.Contains(data, fmt.Sprintf(`"id":%d`, shared)) {
		t.Fatalf("teammate's first event = %s %s, want the shared mailbox's", name, data)
	}

	// The owner still manages it, and can make it private again.
	owner.call(http.MethodPatch, fmt.Sprintf("/api/accounts/%d", shared), `{"shared":false}`, http.StatusOK)
	mate.refuse(http.MethodGet, fmt.Sprintf("/api/messages/%d", teamMsg), "", http.StatusNotFound, "not_found", "")
}
