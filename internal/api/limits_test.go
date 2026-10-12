package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/pipeline"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// limitsModule limits every tenant to what *l says when asked, as the cloud module does
// until billing sets a plan per tenant.
func limitsModule(l *ext.Limits) ext.Module {
	return ext.Module{Name: "limits", Limits: func(context.Context, int64) (ext.Limits, error) { return *l, nil }}
}

// operatorKeys makes every model the tenant's routers ask run on the operator's keys.
func operatorKeys(string) bool { return true }

func TestCheckModulesLimits(t *testing.T) {
	limits := ext.Limits{Mailboxes: 3, AIEmails: 3000}
	if err := CheckModules("selfhost", []ext.Module{limitsModule(&limits)}); err != nil {
		t.Errorf("one module that limits tenants: %v", err)
	}
	err := CheckModules("selfhost", []ext.Module{limitsModule(&limits), {Name: "other", Limits: limitsModule(&limits).Limits}})
	if err == nil || !strings.Contains(err.Error(), "more than one module limits tenants") {
		t.Errorf("two modules that limit tenants: %v", err)
	}
}

// A tenant with all the mailboxes its limits allow is refused another on every path that
// adds one, with the sentence the screen shows, and before anything is dialed. Raising the
// limit lets the next one in.
func TestMailboxLimitOnEveryPath(t *testing.T) {
	const full = "Your plan allows 3 mailboxes, and 3 are connected already. Remove one to connect another."
	limits := ext.Limits{Mailboxes: 3, AIEmails: ext.NoLimit}
	oauth := 0 // one-click logins
	var e *env
	e = newEnvWith(t, func(o *Options) {
		o.ConnectOAuth = func(_ context.Context, acct store.Account, _ *string) (mail.Mailbox, error) {
			oauth++
			return e.box(acct), nil
		}
	}, oneClickModule(), formModule(), limitsModule(&limits))
	e.signIn()
	form := func(username string) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(ext.MailboxFormInput{Step: "sign_in", Username: username, Password: formProtonPassword})
		return e.call(http.MethodPost, "/api/proton/sign-in", string(raw), http.StatusOK)
	}
	oneClick := func(address string) string {
		t.Helper()
		r := e.do(http.MethodGet, "/api/oneclick/connect?provider=outlook&address="+address+"&secret=refresh", "")
		return r.header.Get("Location")
	}
	wizard := func(username string) string {
		return `{"preset":"generic","host":"imap.example.test","username":"` + username + `","password":"app-password-1234"}`
	}

	// Three mailboxes, one through each path.
	e.call(http.MethodPost, "/api/accounts", wizard("one@example.test"), http.StatusCreated)
	if got := oneClick("two@outlook.test"); got != "/#/accounts?added=2" {
		t.Fatalf("one-click = %s", got)
	}
	if a := form("three@proton.test"); a["account_id"] != float64(3) {
		t.Fatalf("form = %v", a)
	}

	// The fourth is refused on every path, before any login.
	e.dialed, oauth = store.Account{}, 0
	for _, path := range []string{"/api/accounts/test", "/api/accounts"} {
		r := e.do(http.MethodPost, path, wizard("four@example.test"))
		if r.status != http.StatusConflict || r.body.Error.Code != "mailbox_limit" || r.body.Error.Message != full {
			t.Errorf("POST %s = %d %s %q", path, r.status, r.body.Error.Code, r.body.Error.Message)
		}
	}
	if got := oneClick("four@outlook.test"); got != "/#/accounts?mailbox_error=mailbox_limit" {
		t.Errorf("one-click = %s", got)
	}
	if a := form("four@proton.test"); a["message"] != full || a["account_id"] != nil {
		t.Errorf("form = %v", a)
	}
	if e.dialed.Username != "" || oauth != 0 {
		t.Errorf("a refused mailbox was dialed: %+v, %d one-click logins", e.dialed, oauth)
	}
	if all, _ := e.st.TenantAccounts(t.Context(), 1); len(all) != 3 {
		t.Errorf("%d mailboxes stored, want 3", len(all))
	}
	got := e.call(http.MethodGet, "/api/limits", "", http.StatusOK)
	conform(t, e.doc, "Limits", got)
	if mb := got["mailboxes"].(map[string]any); mb["used"] != float64(3) || mb["limit"] != float64(3) || mb["refusal"] != full {
		t.Errorf("GET /api/limits mailboxes = %v, want 3 of 3 with the refusal", mb)
	}

	// A plan with room for one more, as billing will set it.
	limits.Mailboxes = 4
	e.call(http.MethodPost, "/api/accounts", wizard("four@example.test"), http.StatusCreated)
	if got := oneClick("five@outlook.test"); got != "/#/accounts?mailbox_error=mailbox_limit" {
		t.Errorf("one-click over the raised limit = %s", got)
	}
}

// A module that cannot say what a tenant may have refuses the mailbox rather than letting it
// in unchecked.
func TestMailboxLimitUnknown(t *testing.T) {
	e := newEnv(t, ext.Module{Name: "limits", Limits: func(context.Context, int64) (ext.Limits, error) {
		return ext.Limits{}, errors.New("plan store down")
	}})
	e.signIn()
	e.refuse(http.MethodPost, "/api/accounts", accountBody, http.StatusInternalServerError, "internal", "")
	if all, _ := e.st.TenantAccounts(t.Context(), 1); len(all) != 0 {
		t.Errorf("%d mailboxes stored", len(all))
	}
}

func TestLimitsSelfHostHasNone(t *testing.T) {
	e := newEnv(t)
	e.signIn()
	for i := range 4 {
		if _, err := e.st.CreateAccount(t.Context(), e.sett.Master, store.Account{UserID: 1, Label: "x", Preset: "generic", Host: "h", Port: 993,
			TLSMode: "implicit", Username: fmt.Sprintf("m%d@example.test", i)}, "pw"); err != nil {
			t.Fatal(err)
		}
	}
	e.call(http.MethodPost, "/api/accounts", accountBody, http.StatusCreated) // a fifth: nothing limits self-host
	got := e.call(http.MethodGet, "/api/limits", "", http.StatusOK)
	conform(t, e.doc, "Limits", got)
	if got["mailboxes"] != nil || got["ai_emails"] != nil || got["upgrade_url"] != nil {
		t.Errorf("self-host limits = %v, want none", got)
	}
}

// GET /api/limits shows a tenant's use against its limits, counted per UTC calendar month,
// and whether mail that needs a model now waits in Needs review.
func TestLimitsShowTheMonthsUse(t *testing.T) {
	limits := ext.Limits{Mailboxes: 3, AIEmails: 3000, UpgradeURL: "#/settings/plan"}
	e := newEnv(t, limitsModule(&limits))
	e.signIn()
	e.call(http.MethodPost, "/api/accounts", accountBody, http.StatusCreated)
	jan := e.ck.t // 15 January 2027, UTC
	spend := func(n int) {
		if err := e.st.SpendAIEmails(t.Context(), 1, e.ck.t, n); err != nil {
			t.Fatal(err)
		}
	}
	get := func() (mailboxes, ai map[string]any, upgrade any) {
		t.Helper()
		got := e.call(http.MethodGet, "/api/limits", "", http.StatusOK)
		conform(t, e.doc, "Limits", got)
		mailboxes, _ = got["mailboxes"].(map[string]any)
		ai, _ = got["ai_emails"].(map[string]any)
		return mailboxes, ai, got["upgrade_url"]
	}
	feb := float64(time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC).Unix())
	tests := []struct {
		name    string
		setup   func()
		used    float64
		month   string
		resets  float64
		counted bool
		paused  bool
	}{
		{"its own keys: not counted", func() { spend(2999) }, 2999, "2027-01", feb, false, false},
		{"the operator's keys, below the allowance", func() { e.metered = operatorKeys }, 2999, "2027-01", feb, true, false},
		{"at the allowance, mail that needs a model waits", func() { spend(1) }, 3000, "2027-01", feb, true, true},
		{"a new month starts from nothing", func() {
			e.metered = operatorKeys
			e.ck.t = time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)
		}, 0, "2027-02", float64(time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC).Unix()), true, false},
	}
	for _, tt := range tests {
		tt.setup()
		mb, ai, upgrade := get()
		if mb["used"] != float64(1) || mb["limit"] != float64(3) || upgrade != "#/settings/plan" {
			t.Errorf("%s: mailboxes %v, upgrade %v", tt.name, mb, upgrade)
		}
		want := map[string]any{"used": tt.used, "limit": float64(3000), "month": tt.month, "resets_at": tt.resets, "counted": tt.counted, "paused": tt.paused}
		for k, v := range want {
			if ai[k] != v {
				t.Errorf("%s: ai_emails.%s = %v, want %v", tt.name, k, ai[k], v)
			}
		}
	}
	if n, _ := e.st.AIEmails(t.Context(), 1, store.Month(jan)); n != 3000 {
		t.Errorf("January's count = %d, want it kept", n)
	}
}

// A cleanup check counts every email a model decides on the operator's keys, and once the
// allowance is used up leaves those for Needs review while condition rules still sort.
func TestCleanupCheckAgainstTheAllowance(t *testing.T) {
	for _, tc := range []struct {
		name               string
		used               int
		wantModel, wantRev int // rows a model decided, rows left for review because of the allowance
		wantUsed           int
	}{
		{"below the allowance", 0, 3, 0, 3},
		{"at the allowance", 100, 0, 3, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limits := ext.Limits{Mailboxes: ext.NoLimit, AIEmails: 100}
			e := newEnv(t, limitsModule(&limits))
			e.metered = operatorKeys
			for range 3 {
				e.deliver("hello@news.example", "reading issue")
			}
			e.deliver("noreply@swiggy.in", "food order")
			e.connect()
			e.call(http.MethodPost, "/api/rules/batch", cleanupRules, http.StatusCreated)
			e.decider.DecideFunc = cleanupDeciderFunc
			if err := e.st.SpendAIEmails(t.Context(), 1, e.ck.t, tc.used); err != nil {
				t.Fatal(err)
			}
			chk := e.checkReady(`{"account_id":1}`)
			model, review, food := 0, 0, 0
			for _, r := range rowsOf(chk) {
				switch {
				case r["confidence"] != nil:
					model++
				case r["reason"] == pipeline.ReasonAllowance && r["review"] == true:
					review++
				case r["rule_name"] == "Food":
					food++
				}
			}
			if model != tc.wantModel || review != tc.wantRev || food != 1 {
				t.Errorf("rows: %d by a model, %d left for review, %d by Food; want %d, %d, 1", model, review, food, tc.wantModel, tc.wantRev)
			}
			if n, _ := e.st.AIEmails(t.Context(), 1, store.Month(e.ck.t)); n != tc.wantUsed {
				t.Errorf("AI-sorted emails = %d, want %d", n, tc.wantUsed)
			}
		})
	}
}
