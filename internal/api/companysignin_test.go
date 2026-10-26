package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// fakePortal is the sign-in service's admin portal a test's team module talks to.
type fakePortal struct {
	asked  []ext.PortalRequest
	looked []string // the organisations whose connection was looked up
	state  string
	err    error
}

// ssoModules are teamModules whose sign-in module also offers company sign-in (fp), and
// whose Team plan sells it from 10 seats or as an add-on.
func ssoModules(fp *fakePortal) func(*fakeTeam, *fakeBilling) []ext.Module {
	return func(ft *fakeTeam, fb *fakeBilling) []ext.Module {
		ms := teamModules(ft, fb)
		ms[0].Team.AdminPortal = func(_ context.Context, p ext.PortalRequest) (string, error) {
			fp.asked = append(fp.asked, p)
			return "https://portal.example/" + p.Intent + "?token=one-time", fp.err
		}
		ms[0].Team.Connection = func(_ context.Context, _ int64, org string) (string, error) {
			fp.looked = append(fp.looked, org)
			return fp.state, fp.err
		}
		team := ms[1].Billing.Plans[store.PlanTeam]
		team.CompanySignIn = &ext.CompanySignInTerms{IncludedSeats: 10, Prices: map[string]string{"month": "$180 a month", "year": "$1,800 a year"}}
		ms[1].Billing.Plans[store.PlanTeam] = team
		// Pro takes any number of people, so a team may move to it.
		pro := ms[1].Billing.Plans[store.PlanPro]
		pro.Limits.Seats = ext.NoLimit
		ms[1].Billing.Plans[store.PlanPro] = pro
		ms[1].Billing.CompanySignIn = func(_ context.Context, c ext.CompanySignInChange) error {
			fb.addOns = append(fb.addOns, c)
			return fb.err
		}
		return ms
	}
}

// reportTeam is the billing service's webhook reporting the team's Team plan.
func (w teamWorld) reportTeam(t *testing.T, interval string, seats int, addOn bool) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"Tenant": w.tenant, "Plan": store.PlanTeam, "Interval": interval, "Status": store.StatusActive,
		"Record": "sub_t", "Seats": seats, "CompanySignInAddOn": addOn})
	if r := w.owner.do(http.MethodPost, "/api/billing/webhook", string(body)); r.status != http.StatusNoContent {
		t.Fatalf("webhook = %d %s", r.status, r.raw)
	}
}

// companySignInFlag is features.company_sign_in in this browser's GET /api/settings.
func (e *env) companySignInFlag() bool {
	e.t.Helper()
	return e.call(http.MethodGet, "/api/settings", "", http.StatusOK)["features"].(map[string]any)["company_sign_in"].(bool)
}

// Who may use company sign-in: a Team tenant whose plan runs with 10 paid seats or more, or
// with the add-on; no other plan, and nobody once the plan ended. In a build that sells no
// plans everyone may; with plans, a Team plan that does not sell it gives it to nobody.
func TestCompanySignInEntitlement(t *testing.T) {
	sold := &ext.CompanySignInTerms{IncludedSeats: 10, Prices: map[string]string{"month": "$180", "year": "$1,800"}}
	onlyAddOn := &ext.CompanySignInTerms{Prices: sold.Prices}
	billing := func(c *ext.CompanySignInTerms) *ext.Billing {
		return &ext.Billing{Plans: map[string]ext.PlanTerms{store.PlanPro: {Name: "Pro"}, store.PlanTeam: {Name: "Team", PerSeat: true, CompanySignIn: c}}}
	}
	team := func(seats int, addOn bool) store.PlanState {
		return store.PlanState{Plan: store.Plan{Plan: store.PlanTeam, Interval: store.IntervalMonth, Status: store.StatusActive, Record: "sub",
			Seats: seats, CompanySignInAddOn: addOn}}
	}
	ended := team(12, true)
	ended.EndedAt = 1
	for _, c := range []struct {
		name string
		b    *ext.Billing
		p    store.PlanState
		want bool
	}{
		{"no plans sold", nil, store.PlanState{Plan: store.Plan{Plan: store.PlanTrial}}, true},
		{"team, 9 seats", billing(sold), team(9, false), false},
		{"team, 9 seats and the add-on", billing(sold), team(9, true), true},
		{"team, 10 seats", billing(sold), team(10, false), true},
		{"team, 10 seats and the add-on", billing(sold), team(10, true), true},
		{"team, 2 seats and the add-on", billing(sold), team(2, true), true},
		{"pro", billing(sold), store.PlanState{Plan: store.Plan{Plan: store.PlanPro, Status: store.StatusActive}}, false},
		{"the trial", billing(sold), store.PlanState{Plan: store.Plan{Plan: store.PlanTrial}}, false},
		{"team, ended", billing(sold), ended, false},
		{"team, never included, 50 seats", billing(onlyAddOn), team(50, false), false},
		{"team, never included, the add-on", billing(onlyAddOn), team(2, true), true},
		{"team that does not sell it, 50 seats", billing(nil), team(50, true), false},
	} {
		if got := companySignInAllowed(c.b, c.p); got != c.want {
			t.Errorf("%s: allowed = %t, want %t", c.name, got, c.want)
		}
	}

	// The same, end to end: features.company_sign_in and GET /api/plan follow the seats and
	// the add-on as the billing service reports them, for every role; Host.Plan too.
	fp := &fakePortal{state: ext.ConnectionNone}
	w := newTeamWorldOf(t, ssoModules(fp))
	doc := spec(t)
	for _, c := range []struct {
		seats int
		addOn bool
		want  bool
	}{{3, false, false}, {9, false, false}, {10, false, true}, {9, true, true}, {3, true, true}} {
		w.reportTeam(t, store.IntervalMonth, c.seats, c.addOn)
		for _, b := range []*env{w.owner, w.admin, w.member} {
			if got := b.companySignInFlag(); got != c.want {
				t.Errorf("%d seats, add-on %t: features.company_sign_in = %t, want %t", c.seats, c.addOn, got, c.want)
			}
			p := b.call(http.MethodGet, "/api/plan", "", http.StatusOK)
			conform(t, doc, "PlanInfo", p)
			got := p["company_sign_in"].(map[string]any)
			if got["allowed"] != c.want || got["add_on"] != c.addOn || got["included_seats"] != 10.0 {
				t.Errorf("%d seats, add-on %t: company_sign_in = %v", c.seats, c.addOn, got)
			}
		}
	}
	// Pro: not allowed, and the add-on cannot be reported with it.
	if r := w.owner.call(http.MethodPost, "/api/billing/webhook", `{"Tenant":`+itoa(w.tenant)+`,"Plan":"pro","Interval":"month","Status":"active","Record":"sub_t","CompanySignInAddOn":true}`, http.StatusOK); r["failed"] == nil {
		t.Errorf("the add-on was taken on Pro: %v", r)
	}
	w.reportTeam(t, store.IntervalMonth, 3, false)
	if _, _, err := w.owner.st.SetPlan(t.Context(), w.tenant, store.Plan{Plan: store.PlanPro, Interval: store.IntervalMonth,
		Status: store.StatusActive, Record: "sub_t"}, w.owner.ck.t.Unix()); err != nil {
		t.Fatal(err)
	}
	if w.owner.companySignInFlag() {
		t.Error("Pro: features.company_sign_in is on")
	}
	plans := w.owner.call(http.MethodGet, "/api/plan", "", http.StatusOK)["plans"].([]any)
	for _, p := range plans {
		p := p.(map[string]any)
		terms, _ := p["company_sign_in"].(map[string]any)
		if (p["id"] == store.PlanTeam) != (terms != nil) {
			t.Errorf("plan %v: company_sign_in = %v; only Team sells it", p["id"], p["company_sign_in"])
		}
		if terms != nil && (terms["included_seats"] != 10.0 || terms["prices"].(map[string]any)["year"] != "$1,800 a year") {
			t.Errorf("Team's company sign-in terms = %v", terms)
		}
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// What a module is told of the tenant's plan carries the add-on and whether the tenant may
// use company sign-in.
func TestHostPlanCompanySignIn(t *testing.T) {
	fp := &fakePortal{state: ext.ConnectionNone}
	w := newTeamWorldOf(t, ssoModules(fp))
	s := &server{Options: Options{Store: w.owner.st, Modules: w.modules}, store: w.owner.st, now: w.owner.ck.now}
	for _, c := range []struct {
		seats       int
		addOn, want bool
	}{{3, false, false}, {3, true, true}, {10, false, true}} {
		w.reportTeam(t, store.IntervalYear, c.seats, c.addOn)
		p, err := host{s}.Plan(t.Context(), w.tenant)
		if err != nil {
			t.Fatal(err)
		}
		if p.CompanySignInAddOn != c.addOn || p.CompanySignIn != c.want || p.Seats != c.seats {
			t.Errorf("%d seats, add-on %t: Host.Plan = add-on %t, may use %t, seats %d", c.seats, c.addOn, p.CompanySignInAddOn, p.CompanySignIn, p.Seats)
		}
	}
}

// Only an Owner buys or removes the add-on, from Plan and billing; the billing module is asked
// with the plan's interval, and the request is in the audit log. The add-on changes when the
// module reports it (Host.SetPlan), and is refused where it makes no sense.
func TestCompanySignInAddOn(t *testing.T) {
	fp := &fakePortal{state: ext.ConnectionNone}
	w := newTeamWorldOf(t, ssoModules(fp))
	add, remove := `{"add_on":true}`, `{"add_on":false}`
	for _, b := range []*env{w.admin, w.member} {
		b.refuse(http.MethodPost, "/api/plan/company-sign-in", add, http.StatusForbidden, "role_required", "")
	}
	w.owner.refuse(http.MethodPost, "/api/plan/company-sign-in", `{}`, http.StatusBadRequest, "invalid_input", "add_on")
	w.owner.refuse(http.MethodPost, "/api/plan/company-sign-in", remove, http.StatusConflict, "same_plan", "")
	if len(w.fb.addOns) != 0 {
		t.Fatalf("asked %+v before anything was allowed", w.fb.addOns)
	}

	p := w.owner.call(http.MethodPost, "/api/plan/company-sign-in", add, http.StatusAccepted)
	conform(t, spec(t), "PlanInfo", p)
	if want := []ext.CompanySignInChange{{TenantID: w.tenant, Record: "sub_t", Add: true, Interval: store.IntervalMonth}}; !slices.Equal(w.fb.addOns, want) {
		t.Fatalf("asked %+v, want %+v", w.fb.addOns, want)
	}
	if w.owner.companySignInFlag() {
		t.Fatal("company sign-in is on before the billing service reported the add-on")
	}
	w.reportTeam(t, store.IntervalMonth, 3, true)
	if !w.owner.companySignInFlag() || !w.admin.companySignInFlag() {
		t.Fatal("company sign-in is off once the add-on was reported")
	}
	w.owner.refuse(http.MethodPost, "/api/plan/company-sign-in", add, http.StatusConflict, "same_plan", "")

	// The add-on stays with seat changes and with a new interval on Team, and goes with Pro.
	w.owner.call(http.MethodPost, "/api/plan/change", `{"plan":"team","interval":"year","seats":4}`, http.StatusAccepted)
	w.owner.call(http.MethodPost, "/api/invitations", `{"email":"fourth@team.test","role":"member"}`, http.StatusCreated)
	w.owner.call(http.MethodPost, "/api/plan/change", `{"plan":"pro","interval":"month"}`, http.StatusAccepted)
	if want := []string{itoa(w.tenant) + " sub_t team year 4 sso", itoa(w.tenant) + " sub_t team month 4 sso", itoa(w.tenant) + " sub_t pro month"}; !slices.Equal(w.fb.changes, want) {
		t.Errorf("changes = %q, want %q", w.fb.changes, want)
	}

	w.owner.call(http.MethodPost, "/api/plan/company-sign-in", remove, http.StatusAccepted)
	if got := w.fb.addOns[len(w.fb.addOns)-1]; got.Add || got.Interval != store.IntervalMonth {
		t.Errorf("remove asked %+v", got)
	}
	w.reportTeam(t, store.IntervalMonth, 4, false)
	if w.owner.companySignInFlag() {
		t.Error("company sign-in is on after the add-on went at 4 seats")
	}

	// From 10 seats it comes with the plan: no add-on to buy.
	w.reportTeam(t, store.IntervalMonth, 10, false)
	w.owner.refuse(http.MethodPost, "/api/plan/company-sign-in", add, http.StatusConflict, "included", "")

	// The billing service failing changes nothing and records nothing.
	w.reportTeam(t, store.IntervalMonth, 4, false)
	w.fb.err = errors.New("down")
	w.owner.refuse(http.MethodPost, "/api/plan/company-sign-in", add, http.StatusBadGateway, "billing_error", "")
	w.fb.err = nil

	csv := w.owner.do(http.MethodGet, "/api/audit", "").raw
	for _, want := range []string{
		"owner@team.test,company_sign_in.add_on,Company sign-in add-on,\"asked to add it, billed monthly\"",
		"owner@team.test,company_sign_in.add_on,Company sign-in add-on,asked to remove it",
		"Billing service,plan.changed,team,\"team, month, active, 3 seats to team, month, active, 3 seats, company sign-in add-on\"",
	} {
		if !strings.Contains(string(csv), want) {
			t.Errorf("audit log lacks %q:\n%s", want, csv)
		}
	}
	if n := strings.Count(string(csv), "company_sign_in.add_on"); n != 2 {
		t.Errorf("%d add-on requests in the audit log, want 2 (the refused ones are not)", n)
	}

	// Not on Team, or not subscribed.
	if _, _, err := w.owner.st.SetPlan(t.Context(), w.tenant, store.Plan{Plan: store.PlanPro, Interval: store.IntervalMonth,
		Status: store.StatusActive, Record: "sub_t"}, w.owner.ck.t.Unix()); err != nil {
		t.Fatal(err)
	}
	w.owner.refuse(http.MethodPost, "/api/plan/company-sign-in", add, http.StatusConflict, "team_only", "")
	if _, _, err := w.owner.st.SetPlan(t.Context(), w.tenant, store.Plan{Plan: store.PlanEnded}, w.owner.ck.t.Unix()); err != nil {
		t.Fatal(err)
	}
	w.owner.refuse(http.MethodPost, "/api/plan/company-sign-in", add, http.StatusConflict, "not_subscribed", "")

	// A build whose Team plan does not sell it.
	plain := newTeamWorld(t)
	plain.owner.refuse(http.MethodPost, "/api/plan/company-sign-in", add, http.StatusNotFound, "not_available", "")
	if p := plain.owner.call(http.MethodGet, "/api/plan", "", http.StatusOK); p["company_sign_in"] != nil {
		t.Errorf("company_sign_in = %v in a build that does not sell it", p["company_sign_in"])
	}
	if plain.owner.companySignInFlag() {
		t.Error("features.company_sign_in in a build without it")
	}
}

// Owners and Admins see how company sign-in stands and open the admin portal while the team
// may use it; Members may not. The link reaches the browser only; who opened one is audited.
func TestCompanySignInPortal(t *testing.T) {
	fp := &fakePortal{state: ext.ConnectionDomainPending}
	w := newTeamWorldOf(t, ssoModules(fp))
	sso := `{"intent":"sso"}`

	// 3 seats, no add-on: not allowed yet.
	w.admin.refuse(http.MethodGet, "/api/company-sign-in", "", http.StatusForbidden, "plan_required", "")
	r := w.owner.do(http.MethodPost, "/api/company-sign-in/portal", sso)
	if r.status != http.StatusForbidden || !strings.Contains(r.body.Error.Message, "from 10 seats") {
		t.Fatalf("portal before it is allowed = %d %s", r.status, r.raw)
	}
	w.reportTeam(t, store.IntervalMonth, 3, true)

	w.member.refuse(http.MethodGet, "/api/company-sign-in", "", http.StatusForbidden, "role_required", "")
	w.member.refuse(http.MethodPost, "/api/company-sign-in/portal", sso, http.StatusForbidden, "role_required", "")
	if len(fp.asked) != 0 || len(fp.looked) != 0 {
		t.Fatalf("the module was asked %+v %v for refused requests", fp.asked, fp.looked)
	}

	if got := w.admin.call(http.MethodGet, "/api/company-sign-in", "", http.StatusOK); got["status"] != ext.ConnectionDomainPending {
		t.Errorf("status = %v", got)
	}
	if !slices.Equal(fp.looked, []string{"org_team"}) {
		t.Errorf("looked up %v, want the team's organisation", fp.looked)
	}
	got := w.admin.call(http.MethodPost, "/api/company-sign-in/portal", `{"intent":"domain_verification"}`, http.StatusOK)
	if got["url"] != "https://portal.example/domain_verification?token=one-time" {
		t.Errorf("portal = %v", got)
	}
	w.owner.call(http.MethodPost, "/api/company-sign-in/portal", sso, http.StatusOK)
	if want := []ext.PortalRequest{{TenantID: w.tenant, Organisation: "org_team", Intent: ext.PortalDomain}, {TenantID: w.tenant, Organisation: "org_team", Intent: ext.PortalSSO}}; !slices.Equal(fp.asked, want) {
		t.Errorf("asked %+v, want %+v", fp.asked, want)
	}
	w.admin.refuse(http.MethodPost, "/api/company-sign-in/portal", `{"intent":"dsync"}`, http.StatusBadRequest, "invalid_input", "intent")

	// A module that fails, or reports a state it should not, is a 502; nothing is audited.
	fp.err = errors.New("workos down")
	w.admin.refuse(http.MethodPost, "/api/company-sign-in/portal", sso, http.StatusBadGateway, "sign_in_error", "")
	w.admin.refuse(http.MethodGet, "/api/company-sign-in", "", http.StatusBadGateway, "sign_in_error", "")
	fp.err, fp.state = nil, "validating"
	w.admin.refuse(http.MethodGet, "/api/company-sign-in", "", http.StatusBadGateway, "sign_in_error", "")

	csv := string(w.owner.do(http.MethodGet, "/api/audit", "").raw)
	for _, want := range []string{
		"admin@team.test,company_sign_in.portal,Company sign-in,opened a setup link to verify the domain",
		"owner@team.test,company_sign_in.portal,Company sign-in,opened a setup link to set up the connection",
	} {
		if !strings.Contains(csv, want) {
			t.Errorf("audit log lacks %q:\n%s", want, csv)
		}
	}
	if n := strings.Count(csv, "company_sign_in.portal"); n != 2 || strings.Contains(csv, "portal.example") {
		t.Errorf("audit log has %d portal rows, or the link:\n%s", n, csv)
	}

	// A build that does not offer company sign-in.
	plain := newTeamWorld(t)
	plain.admin.refuse(http.MethodGet, "/api/company-sign-in", "", http.StatusNotFound, "not_available", "")
	plain.admin.refuse(http.MethodPost, "/api/company-sign-in/portal", sso, http.StatusNotFound, "not_available", "")
}

// Dropping below the seats company sign-in comes with, with no add-on, is not refused: the
// change goes ahead, company sign-in ends once it is reported, and the daemon asks nothing of
// the sign-in service (a connection made stays). The seat worker keeps the add-on as it is.
func TestCompanySignInDowngrade(t *testing.T) {
	fp := &fakePortal{state: ext.ConnectionActive}
	w := newTeamWorldOf(t, ssoModules(fp))
	w.reportTeam(t, store.IntervalMonth, 10, false)
	if !w.owner.companySignInFlag() {
		t.Fatal("10 seats do not allow company sign-in")
	}
	w.admin.call(http.MethodGet, "/api/company-sign-in", "", http.StatusOK)
	asked, looked, removed := len(fp.asked), len(fp.looked), len(w.ft.removed)

	w.owner.call(http.MethodPost, "/api/plan/change", `{"plan":"team","interval":"month","seats":9}`, http.StatusAccepted)
	if !w.owner.companySignInFlag() {
		t.Error("company sign-in ended before the billing service reported 9 seats")
	}
	w.reportTeam(t, store.IntervalMonth, 9, false)
	if w.owner.companySignInFlag() || w.admin.companySignInFlag() {
		t.Error("company sign-in stays at 9 seats with no add-on")
	}
	w.admin.refuse(http.MethodGet, "/api/company-sign-in", "", http.StatusForbidden, "plan_required", "")
	w.admin.refuse(http.MethodPost, "/api/company-sign-in/portal", `{"intent":"sso"}`, http.StatusForbidden, "plan_required", "")
	if len(fp.asked) != asked || len(fp.looked) != looked || len(w.ft.removed) != removed {
		t.Errorf("the sign-in service was asked something after the downgrade: %+v %v %+v", fp.asked[asked:], fp.looked, w.ft.removed)
	}
	if want := itoa(w.tenant) + " sub_t team month 9"; w.fb.changes[len(w.fb.changes)-1] != want {
		t.Errorf("change = %q, want %q", w.fb.changes[len(w.fb.changes)-1], want)
	}

	// The seat worker gives back freed seats with the add-on as it is.
	w.reportTeam(t, store.IntervalMonth, 9, true)
	lower := LowerSeats(w.owner.st, w.modules)
	if err := lower(t.Context(), w.tenant, w.owner.ck.t); err != nil {
		t.Fatal(err)
	}
	if want := itoa(w.tenant) + " sub_t team month 3 sso"; w.fb.changes[len(w.fb.changes)-1] != want {
		t.Errorf("seat worker change = %q, want %q", w.fb.changes[len(w.fb.changes)-1], want)
	}
}

// A build's company sign-in is checked before it serves: Team needs AdminPortal and
// Connection together, the Team plan alone sells it, with both prices, through
// Billing.CompanySignIn, and only in a build that offers it.
func TestCheckModulesCompanySignIn(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(ms []ext.Module)
		want string
	}{
		{"sound", func([]ext.Module) {}, ""},
		{"portal without connection", func(ms []ext.Module) { ms[0].Team.Connection = nil }, "AdminPortal and Connection"},
		{"no portal, but sold", func(ms []ext.Module) { ms[0].Team.AdminPortal, ms[0].Team.Connection = nil, nil }, "no module of the build offers it"},
		{"no billing hook", func(ms []ext.Module) { ms[1].Billing.CompanySignIn = nil }, "Billing has no CompanySignIn"},
		{"a price missing", func(ms []ext.Module) { delete(ms[1].Billing.Plans[store.PlanTeam].CompanySignIn.Prices, "year") }, "a monthly and a yearly price"},
		{"on Pro", func(ms []ext.Module) {
			pro := ms[1].Billing.Plans[store.PlanPro]
			pro.CompanySignIn = ms[1].Billing.Plans[store.PlanTeam].CompanySignIn
			ms[1].Billing.Plans[store.PlanPro] = pro
		}, "only the team plan"},
		{"hook without terms", func(ms []ext.Module) {
			team := ms[1].Billing.Plans[store.PlanTeam]
			team.CompanySignIn = nil
			ms[1].Billing.Plans[store.PlanTeam] = team
		}, "no CompanySignIn terms"},
		{"listed as a feature", func(ms []ext.Module) {
			team := ms[1].Billing.Plans[store.PlanTeam]
			team.Features = append(team.Features, "company_sign_in")
			ms[1].Billing.Plans[store.PlanTeam] = team
		}, "PlanTerms.CompanySignIn"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ms := ssoModules(&fakePortal{})(&fakeTeam{}, &fakeBilling{})
			c.edit(ms)
			err := CheckModules("cloud", ms)
			if (c.want == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), c.want) {
				t.Errorf("CheckModules = %v, want %q", err, c.want)
			}
		})
	}
}

// When a team loses company sign-in the daemon saves when it stops: the end of the paid
// period in force at the loss, kept however often the plan renews, cleared when it comes
// back, or the day the plan ended. Host.Plan and GET /api/plan both report it. A loss a
// release brings (terms that no longer include the team's seats) gets the period's end the
// first time Host.Plan sees it.
func TestCompanySignInEnds(t *testing.T) {
	fp := &fakePortal{state: ext.ConnectionActive}
	w := newTeamWorldOf(t, ssoModules(fp))
	ck := w.owner.ck
	s := &server{Options: Options{Store: w.owner.st, Modules: w.modules}, store: w.owner.st, now: ck.now}
	p1, p2, p3 := ck.t.Add(30*24*time.Hour), ck.t.Add(60*24*time.Hour), ck.t.Add(90*24*time.Hour)
	team := func(seats int, addOn bool, end time.Time) ext.Plan {
		return ext.Plan{Plan: store.PlanTeam, Interval: store.IntervalMonth, Status: store.StatusActive, Record: "sub_t",
			Seats: seats, CompanySignInAddOn: addOn, PeriodEnd: end}
	}
	doc := spec(t)
	endedNow := time.Unix(1, 0) // the moment the plan is reported ended
	for _, c := range []struct {
		name string
		p    ext.Plan
		want time.Time // zero: none
	}{
		{"10 seats", team(10, false, p1), time.Time{}},
		{"down to 9 seats", team(9, false, p1), p1},
		{"renewed at 9 seats", team(9, false, p2), p1},
		{"the add-on bought", team(9, true, p2), time.Time{}},
		{"moved to Pro", ext.Plan{Plan: store.PlanPro, Interval: store.IntervalMonth, Status: store.StatusActive, Record: "sub_t", PeriodEnd: p2}, p2},
		{"the plan ended before the date", ext.Plan{Plan: store.PlanEnded, Record: "sub_t"}, endedNow},
		{"bought again", team(10, false, p3), time.Time{}},
		{"the plan ended", ext.Plan{Plan: store.PlanEnded, Record: "sub_t"}, endedNow},
	} {
		if err := (host{s}).SetPlan(t.Context(), w.tenant, c.p); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if c.want.Equal(endedNow) {
			c.want = ck.t
		}
		got, err := host{s}.Plan(t.Context(), w.tenant)
		if err != nil {
			t.Fatal(err)
		}
		if !got.CompanySignInEnds.Equal(c.want) || got.CompanySignIn != c.want.IsZero() {
			t.Errorf("%s: Host.Plan = may use %t, ends %v; want ends %v", c.name, got.CompanySignIn, got.CompanySignInEnds, c.want)
		}
		r := w.owner.call(http.MethodGet, "/api/plan", "", http.StatusOK)
		conform(t, doc, "PlanInfo", r)
		var want any
		if !c.want.IsZero() {
			want = float64(c.want.Unix())
		}
		if ends := r["company_sign_in"].(map[string]any)["ends_at"]; ends != want {
			t.Errorf("%s: GET /api/plan company_sign_in.ends_at = %v, want %v", c.name, ends, want)
		}
		ck.t = ck.t.Add(time.Hour)
	}

	// A release that includes company sign-in only from 20 seats: the team at 10 loses it,
	// and the first check saves the end of its period.
	if err := (host{s}).SetPlan(t.Context(), w.tenant, team(10, false, p3)); err != nil {
		t.Fatal(err)
	}
	if got, err := (host{s}).Plan(t.Context(), w.tenant); err != nil || !got.CompanySignIn {
		t.Fatalf("10 seats: Host.Plan = %+v, %v", got, err)
	}
	ms := ssoModules(fp)(w.ft, w.fb)
	terms := ms[1].Billing.Plans[store.PlanTeam]
	terms.CompanySignIn = &ext.CompanySignInTerms{IncludedSeats: 20, Prices: terms.CompanySignIn.Prices}
	ms[1].Billing.Plans[store.PlanTeam] = terms
	released := &server{Options: Options{Store: w.owner.st, Modules: ms}, store: w.owner.st, now: ck.now}
	for range 2 {
		got, err := host{released}.Plan(t.Context(), w.tenant)
		if err != nil || got.CompanySignIn || !got.CompanySignInEnds.Equal(p3) {
			t.Errorf("after the release: Host.Plan = may use %t, ends %v, %v; want ends %v", got.CompanySignIn, got.CompanySignInEnds, err, p3)
		}
		ck.t = ck.t.Add(24 * time.Hour)
	}
}
