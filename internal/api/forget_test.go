package api

import (
	"net/http"
	"testing"
)

// A person the sign-in service deleted, forgotten through ext.Host.ForgetIdentity: signed
// out at once, their email free for the service's new id for them, who gets a new, empty
// user; the old identity coming back within the wait gets its mailbox back.
func TestModuleForgetIdentity(t *testing.T) {
	e := newEnv(t, signInModule())
	e.signInAs("sub-old", "cara@example.test", "org-cara")
	acct := e.tenantAccount("cara@imap.example.test")

	server := &client{t: t, h: e.h, cookies: map[string]string{}, noCSRF: true}
	for range 2 { // the second call changes nothing
		if r := server.do(http.MethodPost, forgetPath+"?sub=sub-old", ""); r.status != http.StatusNoContent {
			t.Fatalf("forget = %d %s", r.status, r.raw)
		}
	}
	if r := e.do(http.MethodGet, "/api/accounts", ""); r.status != http.StatusUnauthorized {
		t.Errorf("after forget the old session answers %d %s", r.status, r.raw)
	}

	// The same person under a new id: no email_in_use, and nothing of the old user.
	fresh := e.browser()
	if to := fresh.signInAs("sub-new", "cara@example.test", "org-cara-2"); to != "/" {
		t.Fatalf("the new identity was sent to %q", to)
	}
	if got := fresh.call(http.MethodGet, "/api/accounts", "", http.StatusOK)["items"].([]any); len(got) != 0 {
		t.Errorf("the new identity sees %v", got)
	}

	// The old identity coming back keeps its user and mailbox (its email is taken now, so
	// it signs in with another one).
	back := e.browser()
	if to := back.signInAs("sub-old", "cara@old.example.test", "org-cara"); to != "/" {
		t.Fatalf("the old identity coming back was sent to %q", to)
	}
	got := back.call(http.MethodGet, "/api/accounts", "", http.StatusOK)["items"].([]any)
	if len(got) != 1 || id(got[0].(map[string]any)["id"]) != acct {
		t.Errorf("the old identity's accounts = %v, want %d", got, acct)
	}
	if n := e.count(`SELECT COUNT(*) FROM forgotten`); n != 0 {
		t.Errorf("%d removals still wait", n)
	}
}
