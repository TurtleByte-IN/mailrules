package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// fakeBilling is the billing service a test's module talks to: it records what it was asked.
type fakeBilling struct {
	checkouts []ext.CheckoutRequest
	changes   []string // "tenant record plan interval"
	err       error
}

// termsGiving are the terms of a plan named name that give l and features.
func termsGiving(name string, l ext.Limits, features ...string) ext.PlanTerms {
	return ext.PlanTerms{Name: name, Prices: map[string]string{"month": "$1 a month", "year": "$10 a year"}, Includes: []string{name + " things"},
		Limits: l, Features: features}
}

// billingModule sells plans on fb with these terms. Its webhook route reports a plan as the
// billing service would (Host.SetPlan); its who route answers Host.Who.
func billingModule(fb *fakeBilling, plans map[string]ext.PlanTerms) ext.Module {
	return ext.Module{
		Name: "plans",
		Routes: func(h ext.Host) []ext.Route {
			return []ext.Route{
				{Method: http.MethodGet, Path: "/api/billing/checkout.js", Public: true, Handler: func(w http.ResponseWriter, _ *http.Request) {
					_, _ = w.Write([]byte("window.mailrulesCheckout = {open: (c, done) => done('closed')}"))
				}},
				{Method: http.MethodPost, Path: "/api/billing/webhook", Public: true, Webhook: true, Handler: func(w http.ResponseWriter, r *http.Request) {
					var in struct {
						Tenant                 int64
						Plan, Interval, Status string
						Record                 string
						PeriodEnd              int64
					}
					if !h.ReadJSON(w, r, &in) {
						return
					}
					p := ext.Plan{Plan: in.Plan, Interval: in.Interval, Status: in.Status, Record: in.Record}
					if in.PeriodEnd != 0 {
						p.PeriodEnd = time.Unix(in.PeriodEnd, 0)
					}
					if err := h.SetPlan(r.Context(), in.Tenant, p); err != nil {
						h.WriteJSON(w, http.StatusOK, map[string]string{"failed": err.Error()})
						return
					}
					w.WriteHeader(http.StatusNoContent)
				}},
				{Method: http.MethodGet, Path: "/api/billing/who", Handler: func(w http.ResponseWriter, r *http.Request) {
					h.WriteJSON(w, http.StatusOK, h.Who(r))
				}},
			}
		},
		Billing: &ext.Billing{
			Plans: plans,
			Checkout: func(_ context.Context, c ext.CheckoutRequest) (json.RawMessage, error) {
				fb.checkouts = append(fb.checkouts, c)
				return json.RawMessage(`{"price":"pri_` + c.Plan + `_` + c.Interval + `"}`), fb.err
			},
			ChangePlan: func(_ context.Context, tenantID int64, record, plan, interval string) error {
				fb.changes = append(fb.changes, strings.Join([]string{strconv.FormatInt(tenantID, 10), record, plan, interval}, " "))
				return fb.err
			},
			Portal: func(_ context.Context, _ int64, record string) (string, error) {
				return "https://billing.example/portal/" + record, fb.err
			},
			Script:  "/api/billing/checkout.js",
			Sources: ext.CheckoutSources{Scripts: []string{"https://cdn.billing.example"}, Frames: []string{"https://*.billing.example"}},
			Note:    "Billed by Example, taxes included at checkout.",
		},
	}
}

// limitsModule sells plans that all give l, so a tenant on any of them is held to l.
func limitsModule(l ext.Limits) ext.Module {
	return billingModule(&fakeBilling{}, map[string]ext.PlanTerms{
		store.PlanTrial: termsGiving("Trial", l), store.PlanPersonal: termsGiving("Personal", l), store.PlanPro: termsGiving("Pro", l),
	})
}

// salePlans are Personal (3 mailboxes) and Pro (10, with Suggest and notifications).
func salePlans() map[string]ext.PlanTerms {
	personal := termsGiving("Personal", ext.Limits{Mailboxes: 3, AIEmails: 3000})
	return map[string]ext.PlanTerms{
		store.PlanTrial: personal, store.PlanPersonal: personal,
		store.PlanPro: termsGiving("Pro", ext.Limits{Mailboxes: 10, AIEmails: 10_000}, "suggest", "notifications"),
	}
}

// onPlan puts tenant 1 on plan, as the billing module would.
func (e *env) onPlan(plan string) {
	e.t.Helper()
	if _, _, err := e.st.SetPlan(e.t.Context(), 1, store.Plan{Plan: plan, Interval: store.IntervalMonth, Status: store.StatusActive, Record: "sub_1"}, e.ck.t.Unix()); err != nil {
		e.t.Fatal(err)
	}
}

// madeAgo makes tenant 1 as if it was made d ago.
func (e *env) madeAgo(d time.Duration) {
	e.t.Helper()
	if _, err := e.db.ExecContext(e.t.Context(), `UPDATE tenants SET created_at = ? WHERE id = 1`, e.ck.t.Add(-d).Unix()); err != nil {
		e.t.Fatal(err)
	}
}

// report is the billing service's webhook telling the module of tenant 1's plan.
func (e *env) report(plan, interval, status, record string, periodEnd int64) {
	e.t.Helper()
	body, _ := json.Marshal(map[string]any{"Tenant": 1, "Plan": plan, "Interval": interval, "Status": status, "Record": record, "PeriodEnd": periodEnd})
	if r := e.do(http.MethodPost, "/api/billing/webhook", string(body)); r.status != http.StatusNoContent {
		e.t.Fatalf("webhook = %d %s", r.status, r.raw)
	}
}

func TestCheckModulesBilling(t *testing.T) {
	good := func() ext.Module { return billingModule(&fakeBilling{}, salePlans()) }
	// Pro's features need their modules in the build.
	pro := []ext.Module{{Name: "suggest"}, {Name: "notify", Notifications: &ext.Notifications{Kinds: []string{ext.DestinationWebhook},
		Notify: func(context.Context, ext.Notice, ext.Deliveries) error { return nil }}}}
	tests := []struct {
		name  string
		edit  func(m *ext.Module)
		extra []ext.Module
		want  string // "" = accepted
	}{
		{"a sound module", func(*ext.Module) {}, pro, ""},
		{"a plan missing", func(m *ext.Module) { delete(m.Billing.Plans, store.PlanTrial) }, pro, "needs the trial plan"},
		{"a plan the daemon does not know", func(m *ext.Module) { m.Billing.Plans["team"] = termsGiving("Team", ext.Limits{}) }, pro, `"team"`},
		{"a feature the build lacks", func(*ext.Module) {}, pro[1:], `feature "suggest"`},
		{"no checkout", func(m *ext.Module) { m.Billing.Checkout = nil }, pro, "needs Checkout"},
		{"a script that is not a public route", func(m *ext.Module) { m.Billing.Script = "/api/billing/who" }, pro, "Script"},
		{"a source that is not https", func(m *ext.Module) { m.Billing.Sources.Scripts = []string{"http://cdn.billing.example"} }, pro, "not an https origin"},
		{"a source with a path", func(m *ext.Module) { m.Billing.Sources.Frames = []string{"https://x.example/a"} }, pro, "not an https origin"},
		{"two modules", func(*ext.Module) {}, append(slices.Clone(pro), func() ext.Module { m := good(); m.Name = "other"; return m }()), "more than one module sells plans"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := good()
			tt.edit(&m)
			err := CheckModules("selfhost", append([]ext.Module{m}, tt.extra...))
			if (tt.want == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), tt.want) {
				t.Errorf("CheckModules = %v, want %q", err, tt.want)
			}
		})
	}
}

// A build without billing (self-host) has no plan: no screen, no limits, nothing read-only.
func TestSelfHostHasNoPlan(t *testing.T) {
	e := newEnv(t)
	e.madeAgo(365 * 24 * time.Hour)
	e.signIn()
	e.refuse(http.MethodGet, "/api/plan", "", http.StatusNotFound, "not_available", "")
	if f := e.call(http.MethodGet, "/api/settings", "", http.StatusOK)["features"].(map[string]any); f["billing"] != false {
		t.Errorf("features.billing = %v", f["billing"])
	}
	e.call(http.MethodPatch, "/api/settings", `{"min_confidence":0.8}`, http.StatusOK) // never read-only
	if r := e.do(http.MethodGet, "/", ""); r.header.Get("Content-Security-Policy") != "default-src 'self'" {
		t.Errorf("CSP = %q", r.header.Get("Content-Security-Policy"))
	}
}

// A tenant's trial, buying a plan through checkout, the billing service reporting it, moving
// between plans with the downgrade refused while over, and the portal.
func TestPlanTrialCheckoutAndChange(t *testing.T) {
	fb := &fakeBilling{}
	var started, stopped []int64
	e := newEnvWith(t, func(o *Options) {
		start, stop := o.StartAccount, o.StopAccount
		o.StartAccount = func(a store.Account) { started = append(started, a.ID); start(a) }
		o.StopAccount = func(id int64) { stopped = append(stopped, id); stop(id) }
	}, billingModule(fb, salePlans()), ext.Module{Name: "suggest"})
	e.madeAgo(2*24*time.Hour + time.Hour) // 2 days and an hour into the trial
	e.signIn()

	plan := func() map[string]any {
		t.Helper()
		p := e.call(http.MethodGet, "/api/plan", "", http.StatusOK)
		conform(t, e.doc, "PlanInfo", p)
		return p
	}
	p := plan()
	if p["plan"] != "trial" || p["trial_days_left"] != float64(12) || p["subscribed"] != false || p["read_only"] != false ||
		p["note"] != "Billed by Example, taxes included at checkout." || len(p["plans"].([]any)) != 2 {
		t.Fatalf("trial plan = %v", p)
	}
	if who := e.call(http.MethodGet, "/api/billing/who", "", http.StatusOK); who["TenantID"] != float64(1) || who["Email"] != "me@icloud.com" {
		t.Errorf("Who = %v", who)
	}
	if r := e.do(http.MethodGet, "/", ""); !strings.Contains(r.header.Get("Content-Security-Policy"), "script-src 'self' https://cdn.billing.example; frame-src 'self' https://*.billing.example") {
		t.Errorf("CSP = %q", r.header.Get("Content-Security-Policy"))
	}

	// Checkout for Pro, yearly; changing needs a subscription first.
	e.refuse(http.MethodPost, "/api/plan/change", `{"plan":"pro","interval":"year"}`, http.StatusConflict, "not_subscribed", "")
	e.refuse(http.MethodPost, "/api/plan/checkout", `{"plan":"team","interval":"year"}`, http.StatusBadRequest, "invalid_input", "plan")
	co := e.call(http.MethodPost, "/api/plan/checkout", `{"plan":"pro","interval":"year"}`, http.StatusOK)
	if co["script"] != "/api/billing/checkout.js" || co["checkout"].(map[string]any)["price"] != "pri_pro_year" ||
		len(fb.checkouts) != 1 || fb.checkouts[0] != (ext.CheckoutRequest{TenantID: 1, Email: "me@icloud.com", Plan: "pro", Interval: "year"}) {
		t.Fatalf("checkout = %v, asked %+v", co, fb.checkouts)
	}

	// The service reports the subscription: the plan runs, features follow.
	for i := range 4 {
		if _, err := e.st.CreateAccount(t.Context(), store.Account{UserID: 1, Label: "box" + string(rune('a'+i)), Preset: "generic",
			Host: "h", Port: 993, TLSMode: "implicit", Username: "m" + string(rune('a'+i))}, "pw"); err != nil {
			t.Fatal(err)
		}
	}
	e.report("pro", "year", "active", "sub_9", e.ck.t.AddDate(1, 0, 0).Unix())
	if p := plan(); p["plan"] != "pro" || p["interval"] != "year" || p["subscribed"] != true || p["trial_days_left"] != nil {
		t.Errorf("after the report = %v", p)
	}
	if f := e.call(http.MethodGet, "/api/settings", "", http.StatusOK)["features"].(map[string]any); f["suggest"] != true || f["billing"] != true {
		t.Errorf("features on Pro = %v", f)
	}
	e.refuse(http.MethodPost, "/api/plan/checkout", `{"plan":"personal","interval":"month"}`, http.StatusConflict, "subscribed", "")
	e.refuse(http.MethodPost, "/api/plan/change", `{"plan":"pro","interval":"year"}`, http.StatusConflict, "same_plan", "")

	// Four mailboxes do not fit Personal's three: the screen says which to remove.
	r := e.do(http.MethodPost, "/api/plan/change", `{"plan":"personal","interval":"year"}`)
	if r.status != http.StatusConflict || r.body.Error.Code != "downgrade_refused" ||
		r.body.Error.Message != "Personal allows 3 mailboxes, and 4 are connected: boxa, boxb, boxc, boxd. Remove one of them on Mailboxes first." {
		t.Errorf("downgrade = %d %s %q", r.status, r.body.Error.Code, r.body.Error.Message)
	}
	if _, err := e.st.DeleteAccount(t.Context(), 4, e.ck.t.Unix()); err != nil {
		t.Fatal(err)
	}
	e.call(http.MethodPost, "/api/plan/change", `{"plan":"personal","interval":"month"}`, http.StatusAccepted)
	if !slices.Equal(fb.changes, []string{"1 sub_9 personal month"}) {
		t.Errorf("changes asked = %v", fb.changes)
	}
	if u := e.call(http.MethodGet, "/api/plan/portal", "", http.StatusOK)["url"]; u != "https://billing.example/portal/sub_9" {
		t.Errorf("portal = %v", u)
	}
	fb.err = errors.New("service down")
	e.refuse(http.MethodGet, "/api/plan/portal", "", http.StatusBadGateway, "billing_error", "")
	if len(started) != 0 || len(stopped) != 0 {
		t.Errorf("a plan that kept running started %v and stopped %v", started, stopped)
	}
}

// Once the trial ends with no plan bought, the app is read-only and nothing new connects;
// buying a plan lifts it and starts the mailboxes again; a plan that ends stops them.
func TestPlanEndedIsReadOnly(t *testing.T) {
	var started, stopped []int64
	e := newEnvWith(t, func(o *Options) {
		start, stop := o.StartAccount, o.StopAccount
		o.StartAccount = func(a store.Account) { started = append(started, a.ID); start(a) }
		o.StopAccount = func(id int64) { stopped = append(stopped, id); stop(id) }
	}, billingModule(&fakeBilling{}, salePlans()), oneClickModule())
	e.madeAgo(14 * 24 * time.Hour)
	e.signIn() // public: signing in still works
	if _, err := e.st.CreateAccount(t.Context(), store.Account{UserID: 1, Label: "box", Preset: "generic",
		Host: "h", Port: 993, TLSMode: "implicit", Username: "me"}, "pw"); err != nil {
		t.Fatal(err)
	}
	p := e.call(http.MethodGet, "/api/plan", "", http.StatusOK)
	conform(t, e.doc, "PlanInfo", p)
	ended := e.ck.t.Unix()
	if p["plan"] != "ended" || p["read_only"] != true || p["ended_at"] != float64(ended) ||
		p["secrets_deleted_at"] != float64(ended+30*24*3600) || p["removed_at"] != float64(ended+90*24*3600) {
		t.Fatalf("ended plan = %v", p)
	}
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPatch, "/api/settings", `{"min_confidence":0.8}`},
		{http.MethodPost, "/api/rules/batch", `{"rules":[{"name":"x","conditions":{"field":"subject","op":"contains","value":"x"},"actions":[{"type":"keep"}]}]}`},
		{http.MethodPost, "/api/accounts", accountBody},
		{http.MethodDelete, "/api/accounts/1", ""},
	} {
		r := e.do(c.method, c.path, c.body)
		if r.status != http.StatusForbidden || r.body.Error.Code != "plan_ended" || r.body.Error.Message != ext.ErrPlanEnded.Error() {
			t.Errorf("%s %s = %d %s", c.method, c.path, r.status, r.body.Error.Code)
		}
	}
	e.call(http.MethodGet, "/api/rules", "", http.StatusOK) // reading stays open
	if r := e.do(http.MethodGet, "/api/oneclick/connect?provider=outlook&address=jo@outlook.test&secret=s", ""); !strings.Contains(string(r.raw), ext.ErrPlanEnded.Error()) {
		t.Errorf("one-click while ended = %d %s", r.status, r.raw)
	}
	e.call(http.MethodPost, "/api/plan/checkout", `{"plan":"personal","interval":"month"}`, http.StatusOK) // buying is not a change refused

	e.report("personal", "month", "active", "sub_2", 0)
	if p := e.call(http.MethodGet, "/api/plan", "", http.StatusOK); p["plan"] != "personal" || p["read_only"] != false {
		t.Errorf("bought again = %v", p)
	}
	e.call(http.MethodPatch, "/api/settings", `{"min_confidence":0.8}`, http.StatusOK)
	e.report("ended", "", "", "sub_2", 0)
	if !slices.Equal(started, []int64{1}) || !slices.Equal(stopped, []int64{1}) {
		t.Errorf("started %v, stopped %v; want mailbox 1 started when bought, stopped when it ended", started, stopped)
	}
	e.refuse(http.MethodPatch, "/api/settings", `{"min_confidence":0.7}`, http.StatusForbidden, "plan_ended", "")
}

// A feature a plan includes is a tenant's only on that plan: reported, served and, for
// notifications, written.
func TestPlanGatesFeatures(t *testing.T) {
	suggest := ext.Module{Name: "suggest", Routes: func(h ext.Host) []ext.Route {
		return []ext.Route{{Method: http.MethodPost, Path: "/api/rules/suggest", Handler: func(w http.ResponseWriter, _ *http.Request) {
			h.WriteJSON(w, http.StatusOK, map[string]string{"ok": "yes"})
		}}}
	}}
	notify := ext.Module{Name: "notify", Notifications: &ext.Notifications{Kinds: []string{ext.DestinationWebhook},
		Notify: func(context.Context, ext.Notice, ext.Deliveries) error { return nil }}}
	modules := []ext.Module{billingModule(&fakeBilling{}, salePlans()), suggest, notify}
	e := newEnv(t, modules...)
	e.onPlan(store.PlanPersonal)
	e.signIn()
	features := func() map[string]any {
		return e.call(http.MethodGet, "/api/settings", "", http.StatusOK)["features"].(map[string]any)
	}
	notices := PlanFeature(e.st, modules, "notifications")
	if f := features(); f["suggest"] != false || f["notifications"] != false || notices(t.Context(), 1) {
		t.Errorf("Personal: features %v, notices %v", f, notices(t.Context(), 1))
	}
	r := e.do(http.MethodPost, "/api/rules/suggest", `{}`)
	if r.status != http.StatusForbidden || r.body.Error.Code != "plan_required" ||
		r.body.Error.Message != "Suggest from my mail is part of Pro. Choose it on Plan and billing to use it." {
		t.Errorf("suggest on Personal = %d %s %q", r.status, r.body.Error.Code, r.body.Error.Message)
	}
	e.refuse(http.MethodGet, "/api/notifications", "", http.StatusForbidden, "plan_required", "")

	e.onPlan(store.PlanPro)
	if f := features(); f["suggest"] != true || f["notifications"] != true || !notices(t.Context(), 1) {
		t.Errorf("Pro: features %v", f)
	}
	e.call(http.MethodPost, "/api/rules/suggest", `{}`, http.StatusOK)
	e.call(http.MethodGet, "/api/notifications", "", http.StatusOK)
	if PlanFeature(e.st, nil, "notifications")(t.Context(), 1) != true {
		t.Error("a build that sells no plans gates nothing")
	}
}
