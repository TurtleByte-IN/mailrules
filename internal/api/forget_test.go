package api

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/settings"
	"github.com/TurtleByte-IN/mailrules/internal/worker"
)

// A person the sign-in service deleted, forgotten through ext.Host.ForgetIdentity: signed
// out at once, their mailbox stopped and its password deleted, their tenant's model keys
// deleted, their email free for the service's new id for them, who gets a new, empty user.
// The old identity coming back within the wait gets its mailbox back, stopped and asking
// for a new password, which starts it again.
func TestModuleForgetIdentity(t *testing.T) {
	e := newEnv(t, signInModule())
	e.signInAs("sub-old", "cara@example.test", "org-cara")
	acct := e.tenantAccount("cara@imap.example.test")
	tenant := e.tenantOf("cara@example.test")
	key := "sk-test-cara"
	if err := e.sett.Apply(t.Context(), tenant, settings.Patch{Keys: map[string]string{"openai_api_key": key}}); err != nil {
		t.Fatal(err)
	}

	server := &client{t: t, h: e.h, cookies: map[string]string{}, noCSRF: true}
	for range 2 { // the second call changes nothing
		if r := server.do(http.MethodPost, forgetPath+"?sub=sub-old", ""); r.status != http.StatusNoContent {
			t.Fatalf("forget = %d %s", r.status, r.raw)
		}
	}
	if r := e.do(http.MethodGet, "/api/accounts", ""); r.status != http.StatusUnauthorized {
		t.Errorf("after forget the old session answers %d %s", r.status, r.raw)
	}
	// Stopped at once, its password and the model key deleted, and nothing logs in again.
	if _, err := e.mgr.Mailbox(acct); err == nil {
		t.Error("the forgotten person's mailbox is still running")
	}
	if n := e.count(`SELECT COUNT(*) FROM accounts WHERE length(secret_enc) > 0 OR length(dek_enc) > 0`); n != 0 {
		t.Errorf("%d mailbox secrets left", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM settings WHERE key LIKE 'key.%'`); n != 0 {
		t.Errorf("%d model keys left", n)
	}
	logins := e.logins.Load()

	// The same person under a new id: no email_in_use, and nothing of the old user.
	fresh := e.browser()
	if to := fresh.signInAs("sub-new", "cara@example.test", "org-cara-2"); to != "/" {
		t.Fatalf("the new identity was sent to %q", to)
	}
	if got := fresh.call(http.MethodGet, "/api/accounts", "", http.StatusOK)["items"].([]any); len(got) != 0 {
		t.Errorf("the new identity sees %v", got)
	}

	// The old identity coming back keeps its user and mailbox (its email is taken now, so
	// it signs in with another one). The mailbox waits for a new password; Reconnect does
	// not start it; the model key stays unset.
	back := e.browser()
	if to := back.signInAs("sub-old", "cara@old.example.test", "org-cara"); to != "/" {
		t.Fatalf("the old identity coming back was sent to %q", to)
	}
	got := back.call(http.MethodGet, "/api/accounts", "", http.StatusOK)["items"].([]any)
	if len(got) != 1 || id(got[0].(map[string]any)["id"]) != acct {
		t.Fatalf("the old identity's accounts = %v, want %d", got, acct)
	}
	if a := got[0].(map[string]any); a["status"] != worker.StatusAuthFailed {
		t.Errorf("the mailbox after coming back = %v, want auth_failed", a)
	}
	if n := e.count(`SELECT COUNT(*) FROM forgotten`); n != 0 {
		t.Errorf("%d removals still wait", n)
	}
	path := fmt.Sprintf("/api/accounts/%d", acct)
	back.refuse(http.MethodPost, path+"/reconnect", "", http.StatusUnprocessableEntity, "auth_failed", "password")
	back.refuse(http.MethodPost, path+"/test", "", http.StatusUnprocessableEntity, "auth_failed", "password")
	time.Sleep(20 * time.Millisecond)
	if n := e.logins.Load(); n != logins {
		t.Errorf("%d logins after forget", n-logins)
	}
	if _, err := e.mgr.Mailbox(acct); err == nil {
		t.Error("the mailbox runs without a password")
	}
	if keys := back.call(http.MethodGet, "/api/settings", "", http.StatusOK)["keys"].(map[string]any); keys["openai_api_key"] != settings.KeyNone {
		t.Errorf("the model key after coming back = %v, want none", keys["openai_api_key"])
	}

	// A new password starts it again.
	back.call(http.MethodPatch, path, `{"password":"app-password-5678"}`, http.StatusOK)
	eventually(t, "the mailbox to be live again", func() bool {
		a, err := e.st.Account(t.Context(), acct)
		_, mbErr := e.mgr.Mailbox(acct)
		return err == nil && a.Status == worker.StatusLive && !a.SecretGone && mbErr == nil
	})
}
