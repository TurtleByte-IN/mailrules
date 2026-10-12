package api

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/settings"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

const deletePath = "/api/auth/delete-account"

// deletingModule is signInModule with DeleteIdentity: it records each call, and answers
// with *fail.
func deletingModule(calls *[][2]string, fail *error, during func()) ext.Module {
	m := signInModule()
	m.DeleteIdentity = func(_ context.Context, provider, subject string) error {
		*calls = append(*calls, [2]string{provider, subject})
		if during != nil {
			during()
		}
		return *fail
	}
	return m
}

// A person deletes their own account: the sign-in service deletes them first, and only
// then are they forgotten as when the service deletes them. A refused confirmation or a
// failing service deletes nothing; the service's own deletion notice afterwards changes
// nothing; the email signs up again as a new, empty user.
func TestDeleteAccount(t *testing.T) {
	var (
		calls [][2]string
		fail  error
		e     *env
		// What the database held while the service was deleting the person.
		secretsDuring, sessionsDuring int
	)
	e = newEnv(t, deletingModule(&calls, &fail, func() {
		secretsDuring = e.count(`SELECT COUNT(*) FROM accounts WHERE length(secret_enc) > 0`)
		sessionsDuring = e.count(`SELECT COUNT(*) FROM sessions`)
	}))
	e.signInAs("sub-dee", "dee@example.test", "org-dee")
	other := e.browser() // the same person on another device
	other.signInAs("sub-dee", "dee@example.test", "org-dee")
	acct := e.tenantAccount("dee@imap.example.test")
	if err := e.sett.Apply(t.Context(), e.tenantOf("dee@example.test"), settings.Patch{Keys: map[string]string{"openai_api_key": "sk-test-dee"}}); err != nil {
		t.Fatal(err)
	}
	if on := e.call(http.MethodGet, "/api/settings", "", http.StatusOK)["features"].(map[string]any)["delete_account"]; on != true {
		t.Errorf("features.delete_account = %v with a sign-in module that deletes", on)
	}
	unchanged := func(when string) {
		t.Helper()
		if r := e.do(http.MethodGet, "/api/auth/me", ""); r.status != http.StatusOK {
			t.Errorf("%s: the session answers %d", when, r.status)
		}
		if _, err := e.mgr.Mailbox(acct); err != nil {
			t.Errorf("%s: the mailbox stopped: %v", when, err)
		}
		if n := e.count(`SELECT COUNT(*) FROM accounts WHERE length(secret_enc) > 0`) + e.count(`SELECT COUNT(*) FROM settings WHERE key LIKE 'key.%'`); n != 2 {
			t.Errorf("%s: %d of the mailbox password and the model key left, want both", when, n)
		}
		if n := e.count(`SELECT COUNT(*) FROM forgotten`); n != 0 {
			t.Errorf("%s: %d removals scheduled", when, n)
		}
	}

	// The confirmation must be the person's own email; nothing reaches the service otherwise.
	for _, body := range []string{`{"email":"someone@example.test"}`, `{"email":""}`, `{}`} {
		e.refuse(http.MethodPost, deletePath, body, http.StatusBadRequest, "invalid_input", "email")
	}
	e.refuse(http.MethodPost, deletePath, `{"email":"dee@example.test","now":true}`, http.StatusBadRequest, "invalid_json", "")
	if len(calls) != 0 {
		t.Errorf("the service was asked %d times for a refused confirmation", len(calls))
	}
	unchanged("after a refused confirmation")

	// The service failing deletes nothing, and says so.
	fail = errors.New("sign-in service: 503")
	r := e.do(http.MethodPost, deletePath, `{"email":"dee@example.test"}`)
	if r.status != http.StatusBadGateway || r.body.Error.Code != "sign_in_error" || r.body.Error.Message == "" {
		t.Fatalf("a failing service = %d %s", r.status, r.raw)
	}
	unchanged("after the service failed")

	// Deleted: the service first, while everything was still there, then here at once.
	fail = nil
	calls = nil
	r = e.do(http.MethodPost, deletePath, `{"email":"  DEE@example.test "}`)
	if r.status != http.StatusNoContent {
		t.Fatalf("delete = %d %s", r.status, r.raw)
	}
	if len(calls) != 1 || calls[0] != [2]string{testProvider, "sub-dee"} {
		t.Errorf("the service was asked %v, want once for the person", calls)
	}
	if secretsDuring != 1 || sessionsDuring != 2 {
		t.Errorf("while the service deleted, %d secrets and %d sessions were left, want all of them", secretsDuring, sessionsDuring)
	}
	if ck := r.setCookies[sessionCookie]; ck == nil || ck.MaxAge >= 0 {
		t.Errorf("the session cookie was not cleared: %+v", ck)
	}
	for _, b := range []*env{e, other} {
		if r := b.do(http.MethodGet, "/api/accounts", ""); r.status != http.StatusUnauthorized || r.body.Error.SignIn != signInPath {
			t.Errorf("after the deletion a session of the person answers %d %s", r.status, r.raw)
		}
	}
	if _, err := e.mgr.Mailbox(acct); err == nil {
		t.Error("the mailbox is still running")
	}
	if _, err := e.st.AccountSecret(t.Context(), acct); !errors.Is(err, store.ErrNoSecret) {
		t.Errorf("the mailbox password: %v, want ErrNoSecret", err)
	}
	if n := e.count(`SELECT COUNT(*) FROM settings WHERE key LIKE 'key.%'`); n != 0 {
		t.Errorf("%d model keys left", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM users WHERE email = 'dee@example.test'`); n != 0 {
		t.Error("the email is still taken")
	}
	removeAt := func() int64 {
		t.Helper()
		var at, wait int64
		if err := e.db.QueryRowContext(t.Context(), `SELECT remove_at, remove_at - forgotten_at FROM forgotten`).Scan(&at, &wait); err != nil {
			t.Fatalf("the removal: %v", err)
		}
		if wait != store.ForgetDays*24*60*60 {
			t.Errorf("the removal waits %d s, want %d days", wait, store.ForgetDays)
		}
		return at
	}
	at := removeAt()

	// The service's deletion notice for the same person, a moment later, changes nothing.
	e.ck.t = e.ck.t.Add(time.Hour)
	server := &client{t: t, h: e.h, cookies: map[string]string{}, noCSRF: true}
	if r := server.do(http.MethodPost, forgetPath+"?sub=sub-dee", ""); r.status != http.StatusNoContent {
		t.Fatalf("the service's deletion notice = %d %s", r.status, r.raw)
	}
	if got := removeAt(); got != at || e.count(`SELECT COUNT(*) FROM forgotten`) != 1 {
		t.Errorf("the deletion notice moved the removal from %d to %d", at, got)
	}

	// The same email signs up again, under the service's new id for it, as a new, empty user.
	fresh := e.browser()
	if to := fresh.signInAs("sub-dee-2", "dee@example.test", "org-dee-2"); to != "/" {
		t.Fatalf("signing up again was sent to %q", to)
	}
	if got := fresh.call(http.MethodGet, "/api/accounts", "", http.StatusOK)["items"].([]any); len(got) != 0 {
		t.Errorf("the new user sees %v", got)
	}
}

// A sign-in module that cannot delete people at its sign-in service still offers Delete my
// account: MailRules deletes its own data as with one that can, and the person's login at
// the service stays. Signing in again with that same login, before the removal or after,
// makes a new, empty account, never the old one.
func TestDeleteAccountWithoutSignInHook(t *testing.T) {
	e := newEnv(t, signInModule())
	e.signInAs("sub-pat", "pat@example.test", "org-pat")
	acct := e.tenantAccount("pat@imap.example.test")
	old := e.tenantOf("pat@example.test")
	if err := e.sett.Apply(t.Context(), old, settings.Patch{Keys: map[string]string{"openai_api_key": "sk-test-pat"}}); err != nil {
		t.Fatal(err)
	}
	e.call(http.MethodPost, "/api/rules/batch", cleanupRules, http.StatusCreated)
	if on := e.call(http.MethodGet, "/api/settings", "", http.StatusOK)["features"].(map[string]any)["delete_account"]; on != true {
		t.Errorf("features.delete_account = %v with a sign-in module", on)
	}
	e.refuse(http.MethodPost, deletePath, `{"email":"someone@example.test"}`, http.StatusBadRequest, "invalid_input", "email")

	r := e.do(http.MethodPost, deletePath, `{"email":"pat@example.test"}`)
	if r.status != http.StatusNoContent {
		t.Fatalf("delete = %d %s", r.status, r.raw)
	}
	if ck := r.setCookies[sessionCookie]; ck == nil || ck.MaxAge >= 0 {
		t.Errorf("the session cookie was not cleared: %+v", ck)
	}
	if r := e.do(http.MethodGet, "/api/accounts", ""); r.status != http.StatusUnauthorized {
		t.Errorf("after the deletion the session answers %d %s", r.status, r.raw)
	}
	if _, err := e.mgr.Mailbox(acct); err == nil {
		t.Error("the mailbox is still running")
	}
	if _, err := e.st.AccountSecret(t.Context(), acct); !errors.Is(err, store.ErrNoSecret) {
		t.Errorf("the mailbox password: %v, want ErrNoSecret", err)
	}
	if n := e.count(`SELECT COUNT(*) FROM settings WHERE key LIKE 'key.%'`); n != 0 {
		t.Errorf("%d model keys left", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM users WHERE email = 'pat@example.test'`); n != 0 {
		t.Error("the email is still taken")
	}
	if n := e.count(`SELECT COUNT(*) FROM forgotten`); n != 1 {
		t.Errorf("%d removals scheduled, want 1", n)
	}

	// The same login signs in again within the wait: a new, empty account.
	back := e.browser()
	if to := back.signInAs("sub-pat", "pat@example.test", "org-pat"); to != "/" {
		t.Fatalf("signing in again was sent to %q", to)
	}
	if tenant := e.tenantOf("pat@example.test"); tenant == old {
		t.Errorf("signing in again landed in the old tenant %d", old)
	}
	if got := back.call(http.MethodGet, "/api/accounts", "", http.StatusOK)["items"].([]any); len(got) != 0 {
		t.Errorf("the new account sees mailboxes %v", got)
	}
	if got := back.call(http.MethodGet, "/api/rules", "", http.StatusOK)["items"].([]any); len(got) != 0 {
		t.Errorf("the new account sees rules %v", got)
	}
	if n := e.count(`SELECT COUNT(*) FROM forgotten`); n != 1 {
		t.Errorf("signing in again called the removal off: %d left, want 1", n)
	}
}

// Self-host offers no account deletion: not_available, signed in or not, and so say its
// features. A sign-in build's user not made by the service cannot use it either.
func TestDeleteAccountNotAvailable(t *testing.T) {
	self := newEnv(t)
	self.do(http.MethodGet, "/api/auth/me", "")
	self.refuse(http.MethodPost, deletePath, `{"email":"me@icloud.com"}`, http.StatusNotFound, "not_available", "")
	self.signIn()
	self.refuse(http.MethodPost, deletePath, `{"email":"me@icloud.com"}`, http.StatusNotFound, "not_available", "")
	if on := self.call(http.MethodGet, "/api/settings", "", http.StatusOK)["features"].(map[string]any)["delete_account"]; on != false {
		t.Errorf("self-host features.delete_account = %v", on)
	}

	var calls [][2]string
	var fail error
	e := newEnv(t, deletingModule(&calls, &fail, nil))
	e.do(http.MethodGet, "/api/auth/me", "")
	e.refuse(http.MethodPost, deletePath, `{"email":"x@example.test"}`, http.StatusUnauthorized, "unauthenticated", "")
	// A user with no identity (an admin from before the build signed people in).
	admin, err := e.st.CreateUser(t.Context(), store.SelfHostTenant, "admin@example.test", "hash", e.ck.t.Unix())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.CreateSession(t.Context(), hashToken("admin-session"), admin.ID, e.ck.t.Unix(), e.ck.t.Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	e.cookies[sessionCookie] = "admin-session"
	e.refuse(http.MethodPost, deletePath, `{"email":"admin@example.test"}`, http.StatusNotFound, "not_available", "")
	if len(calls) != 0 {
		t.Errorf("the service was asked %v", calls)
	}
}
