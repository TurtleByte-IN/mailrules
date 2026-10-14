package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// fakeTeam is the sign-in service a test's team module talks to: it records what it was
// asked.
type fakeTeam struct {
	invited, revoked []ext.Invitation
	removed          []ext.Identity
	inviteErr        error
}

const (
	teamStart  = "/api/testteam/start"  // signs in ?sub=&email=&org=
	teamAccept = "/api/testteam/accept" // a webhook: ?tenant=&email= accepted
)

// teamModules are a sign-in module that invites people (ft) and a billing module (fb) whose
// Team plan, sold by the seat from 2 seats at "$8 a seat a month", includes the team.
func teamModules(ft *fakeTeam, fb *fakeBilling) []ext.Module {
	one := ext.Limits{Mailboxes: 3, AIEmails: 100, Seats: 1}
	team := termsGiving("Team", ext.Limits{Mailboxes: 2, AIEmails: 5000}, "team")
	team.PerSeat, team.MinSeats = true, 2
	team.Prices = map[string]string{"month": "$8 a seat a month", "year": "$80 a seat a year"}
	plans := map[string]ext.PlanTerms{store.PlanTrial: termsGiving("Trial", one), store.PlanPersonal: termsGiving("Personal", one),
		store.PlanPro: termsGiving("Pro", one), store.PlanTeam: team}
	sso := ext.Module{
		Name: "testteam", SignIn: teamStart,
		Routes: func(h ext.Host) []ext.Route {
			return []ext.Route{
				{Method: http.MethodGet, Path: teamStart, Public: true, Handler: func(w http.ResponseWriter, r *http.Request) {
					q := r.URL.Query()
					_, err := h.SignIn(w, r, ext.Identity{Provider: testProvider, Subject: q.Get("sub"), Email: q.Get("email"), Tenant: q.Get("org")})
					switch {
					case errors.Is(err, ext.ErrSeatLimit):
						h.SignInFailed(w, r, ext.SignInSeatsFull)
					case errors.Is(err, ext.ErrRemovedMember):
						h.SignInFailed(w, r, ext.SignInRemoved)
					case errors.Is(err, ext.ErrTenantMismatch):
						h.SignInFailed(w, r, ext.SignInTenantMismatch)
					case err != nil:
						h.SignInFailed(w, r, ext.SignInRefused)
					default:
						http.Redirect(w, r, "/", http.StatusSeeOther)
					}
				}},
				{Method: http.MethodPost, Path: teamAccept, Public: true, Webhook: true, Handler: func(w http.ResponseWriter, r *http.Request) {
					tenant, _ := strconv.ParseInt(r.URL.Query().Get("tenant"), 10, 64)
					if err := h.AcceptInvitation(r.Context(), tenant, r.URL.Query().Get("email")); err != nil {
						h.WriteJSON(w, http.StatusOK, map[string]string{"failed": err.Error()})
						return
					}
					w.WriteHeader(http.StatusNoContent)
				}},
			}
		},
		Team: &ext.Team{
			Invite: func(_ context.Context, inv ext.Invitation) (string, error) {
				ft.invited = append(ft.invited, inv)
				return "inv_" + inv.Email, ft.inviteErr
			},
			Revoke: func(_ context.Context, inv ext.Invitation) error {
				ft.revoked = append(ft.revoked, inv)
				return nil
			},
			RemoveMember: func(_ context.Context, _ int64, id ext.Identity) error {
				ft.removed = append(ft.removed, id)
				return nil
			},
		},
	}
	return []ext.Module{sso, billingModule(fb, plans)}
}

// teamSignIn signs this browser in through the team module and returns where it was sent.
func (e *env) teamSignIn(sub, email string) string {
	e.t.Helper()
	return e.teamSignInAt(sub, email, "org_team")
}

// teamSignInAt is teamSignIn under the organisation org.
func (e *env) teamSignInAt(sub, email, org string) string {
	e.t.Helper()
	q := url.Values{"sub": {sub}, "email": {email}, "org": {org}}
	r := e.do(http.MethodGet, teamStart+"?"+q.Encode(), "")
	if r.status != http.StatusSeeOther {
		e.t.Fatalf("sign in as %s = %d %s", sub, r.status, r.raw)
	}
	return r.header.Get("Location")
}

// teamWorld is a team of three on the Team plan with 3 seats: an owner, an admin and a
// member, each in a browser of their own, the admin and member invited and accepted.
type teamWorld struct {
	ft                   *fakeTeam
	fb                   *fakeBilling
	modules              []ext.Module
	owner, admin, member *env
	tenant               int64
	ids                  map[string]int64 // user ids by role
}

func newTeamWorld(t *testing.T) teamWorld {
	t.Helper()
	return newTeamWorldOf(t, teamModules)
}

// newTeamWorldOf is newTeamWorld with the modules build makes.
func newTeamWorldOf(t *testing.T, build func(*fakeTeam, *fakeBilling) []ext.Module) teamWorld {
	t.Helper()
	w := teamWorld{ft: &fakeTeam{}, fb: &fakeBilling{}, ids: map[string]int64{}}
	w.modules = build(w.ft, w.fb)
	e := newEnv(t, w.modules...)
	w.owner = e.browser()
	w.owner.teamSignIn("user_o", "owner@team.test")
	me := w.owner.me()["user"].(map[string]any)
	if me["role"] != "owner" {
		t.Fatalf("the team's first person = %v, want an owner", me)
	}
	w.ids["owner"] = id(me["id"])
	w.tenant = e.tenantOf("owner@team.test")
	if _, _, err := e.st.SetPlan(t.Context(), w.tenant, store.Plan{Plan: store.PlanTeam, Interval: store.IntervalMonth,
		Status: store.StatusActive, Record: "sub_t", Seats: 3}, e.ck.t.Unix()); err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct{ role, sub string }{{"admin", "user_a"}, {"member", "user_m"}} {
		email := p.role + "@team.test"
		w.owner.call(http.MethodPost, "/api/invitations", `{"email":"`+email+`","role":"`+p.role+`"}`, http.StatusCreated)
		if r := e.do(http.MethodPost, teamAccept+"?tenant="+strconv.FormatInt(w.tenant, 10)+"&email="+email, ""); r.status != http.StatusNoContent {
			t.Fatalf("accept = %d %s", r.status, r.raw)
		}
		b := e.browser()
		b.teamSignIn(p.sub, email)
		me := b.me()["user"].(map[string]any)
		if me["role"] != p.role {
			t.Fatalf("invited %s signed in as %v", p.role, me)
		}
		w.ids[p.role] = id(me["id"])
		if p.role == "admin" {
			w.admin = b
		} else {
			w.member = b
		}
	}
	return w
}

// The route table and the contract agree on who may call each route.
func TestRolesMatchOpenAPI(t *testing.T) {
	doc := spec(t)
	inSpec := map[string]string{}
	for path, item := range doc["paths"].(map[string]any) {
		for method, op := range item.(map[string]any) {
			if !slices.Contains(methods, method) {
				continue
			}
			if role, ok := op.(map[string]any)["x-role"]; ok {
				inSpec[strings.ToUpper(method)+" "+path] = role.(string)
			}
		}
	}
	for _, r := range (&server{}).routes() {
		key := r.method + " " + r.path
		if inSpec[key] != r.role {
			t.Errorf("%s: x-role %q in the contract, %q in the route table", key, inSpec[key], r.role)
		}
		delete(inSpec, key)
	}
	for key := range inSpec {
		t.Errorf("%s has x-role but is not served", key)
	}
}

// The permission matrix: every signed-in route, called by each role, is refused with 403
// role_required exactly when the role is below the route's.
func TestPermissionMatrix(t *testing.T) {
	w := newTeamWorld(t)
	fill := strings.NewReplacer("{id}", "999999", "{message_id}", "999999", "{type}", "address", "{value}", "nobody@x.test")
	browsers := map[string]*env{store.RoleOwner: w.owner, store.RoleAdmin: w.admin, store.RoleMember: w.member}
	for _, r := range (&server{}).routes() {
		if r.public || r.path == "/api/events" {
			continue
		}
		for _, role := range store.Roles {
			t.Run(role+" "+r.method+" "+r.path, func(t *testing.T) {
				got := browsers[role].do(r.method, fill.Replace(r.path), "{}")
				refused := got.status == http.StatusForbidden && got.body.Error.Code == "role_required"
				if want := store.RoleRank(role) < store.RoleRank(r.role); refused != want {
					t.Errorf("= %d %s (%s), want refused %v", got.status, got.body.Error.Code, got.body.Error.Message, want)
				}
			})
		}
	}
}

func TestTeamMembersInvitationsAndAudit(t *testing.T) {
	w := newTeamWorld(t)
	o, a, m := w.owner, w.admin, w.member
	if len(w.ft.invited) != 2 || w.ft.invited[0].Organisation != "org_team" || w.ft.invited[0].InvitedBy != "owner@team.test" || w.ft.invited[0].Role != "admin" {
		t.Fatalf("invitations sent = %+v", w.ft.invited)
	}
	list := m.call(http.MethodGet, "/api/members", "", http.StatusOK)
	conform(t, m.doc, "Member", list["members"].([]any)[0])
	if seats := list["seats"].(map[string]any); seats["used"] != float64(3) || seats["limit"] != float64(3) || len(list["members"].([]any)) != 3 {
		t.Fatalf("members = %v", list)
	}
	if f := m.call(http.MethodGet, "/api/settings", "", http.StatusOK)["features"].(map[string]any); f["team"] != true {
		t.Errorf("features.team = %v", f["team"])
	}

	// Seats: every one is taken, so someone the service lets in without an invitation is
	// refused (an invitation adds a seat instead: TestInvitationAddsSeat).
	if loc := o.browser().teamSignIn("user_x", "x@team.test"); loc != "/?signin_error=seats_full" {
		t.Errorf("sign-in over the seats went to %q", loc)
	}

	// Roles: only an owner touches an owner, and the team keeps one.
	owner, member := strconv.FormatInt(w.ids["owner"], 10), strconv.FormatInt(w.ids["member"], 10)
	a.refuse(http.MethodPatch, "/api/members/"+owner, `{"role":"member"}`, http.StatusForbidden, "role_required", "")
	a.refuse(http.MethodPatch, "/api/members/"+member, `{"role":"owner"}`, http.StatusForbidden, "role_required", "")
	a.refuse(http.MethodDelete, "/api/members/"+owner, "", http.StatusForbidden, "role_required", "")
	a.refuse(http.MethodPatch, "/api/members/"+member, `{"role":"boss"}`, http.StatusBadRequest, "invalid_input", "role")
	o.refuse(http.MethodPatch, "/api/members/"+owner, `{"role":"admin"}`, http.StatusConflict, "last_owner", "")
	o.refuse(http.MethodDelete, "/api/members/"+owner, "", http.StatusConflict, "remove_self", "")
	o.refuse(http.MethodPost, "/api/auth/delete-account", `{"email":"owner@team.test"}`, http.StatusConflict, "last_owner", "")
	a.refuse(http.MethodPost, "/api/invitations", `{"email":"boss@team.test","role":"owner"}`, http.StatusForbidden, "role_required", "")

	// A member adds their own mailbox, but not a shared rule.
	m.call(http.MethodPost, "/api/accounts", accountBody, http.StatusCreated)
	m.refuse(http.MethodPost, "/api/rules/batch", `{"rules":[]}`, http.StatusForbidden, "role_required", "")
	m.refuse(http.MethodPost, "/api/activity/questions/1/answer", `{"answer":"yes"}`, http.StatusNotFound, "not_available", "")

	// The admin removes the member: the service is asked first, then their session ends.
	a.call(http.MethodDelete, "/api/members/"+member, "", http.StatusNoContent)
	if len(w.ft.removed) != 1 || w.ft.removed[0].Subject != "user_m" || w.ft.removed[0].Tenant != "org_team" {
		t.Errorf("removed at the service = %+v", w.ft.removed)
	}
	m.call(http.MethodGet, "/api/members", "", http.StatusUnauthorized)

	// An invitation that cannot be sent is withdrawn; one revoked is taken back.
	w.ft.inviteErr = errors.New("service down")
	o.refuse(http.MethodPost, "/api/invitations", `{"email":"later@team.test","role":"member"}`, http.StatusBadGateway, "invite_failed", "")
	w.ft.inviteErr = nil
	inv := o.call(http.MethodPost, "/api/invitations", `{"email":"later@team.test","role":"member"}`, http.StatusCreated)["invitation"].(map[string]any)
	conform(t, o.doc, "Invitation", inv)
	if l := a.call(http.MethodGet, "/api/invitations", "", http.StatusOK)["invitations"].([]any); len(l) != 1 {
		t.Errorf("invitations = %v", l)
	}
	o.call(http.MethodDelete, "/api/invitations/"+strconv.FormatInt(id(inv["id"]), 10), "", http.StatusNoContent)
	if len(w.ft.revoked) != 1 || w.ft.revoked[0].Record != "inv_later@team.test" {
		t.Errorf("revoked = %+v", w.ft.revoked)
	}

	// The audit log lists who changed what, as CSV by default and as JSON.
	r := a.do(http.MethodGet, "/api/audit", "")
	csv := string(r.raw)
	if r.status != http.StatusOK || !strings.HasPrefix(r.header.Get("Content-Type"), "text/csv") || !strings.Contains(r.header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("audit = %d %v", r.status, r.header)
	}
	for _, want := range []string{
		"time,actor,action,target,detail",
		"owner@team.test,invitation.created,admin@team.test,as Admin",
		"Sign-in service,invitation.accepted,admin@team.test,joins as Admin at their first sign-in",
		"admin@team.test,member.joined,admin@team.test,\"as Admin, invited by member #" + owner + "\"",
		// The removed member's address is gone from every row; a detail never holds one.
		"removed member #" + member + ",mailbox.added,me@example.test,\"an address, imap.example.test\"",
		"admin@team.test,member.removed,removed member #" + member + ",removed from the team",
		"owner@team.test,invitation.withdrawn,later@team.test,it could not be sent",
		"owner@team.test,invitation.revoked,later@team.test,",
	} {
		if !strings.Contains(csv, want) {
			t.Errorf("audit CSV lacks %q:\n%s", want, csv)
		}
	}
	entries := a.call(http.MethodGet, "/api/audit?format=json", "", http.StatusOK)["entries"].([]any)
	conform(t, a.doc, "AuditEntry", entries[0])
	for _, e := range entries {
		if e := e.(map[string]any); strings.Contains(e["detail"].(string), "@") || strings.Contains(e["actor"].(string)+e["target"].(string), "member@team.test") {
			t.Errorf("audit entry = %v; want no address in detail and none of the removed member's", e)
		}
	}
	a.refuse(http.MethodGet, "/api/audit?format=xml", "", http.StatusBadRequest, "invalid_input", "format")
	if cell("=SUM(A1)") != "'=SUM(A1)" || cell("plain") != "plain" {
		t.Error("cell does not defuse a formula")
	}
}

// Seats on Plan and billing: a per-seat plan needs seats, its limits are one seat's times
// them, and a team cannot move to fewer seats than it has people.
func TestPlanSeats(t *testing.T) {
	w := newTeamWorld(t)
	o := w.owner
	p := o.call(http.MethodGet, "/api/plan", "", http.StatusOK)
	conform(t, o.doc, "PlanInfo", p)
	if p["seats"] != float64(3) || len(p["plans"].([]any)) != 3 {
		t.Fatalf("plan = %v", p)
	}
	if l := o.call(http.MethodGet, "/api/limits", "", http.StatusOK)["mailboxes"].(map[string]any); l["limit"] != float64(6) {
		t.Errorf("mailboxes limit = %v, want 2 a seat times 3", l)
	}
	o.refuse(http.MethodPost, "/api/plan/change", `{"plan":"team","interval":"month"}`, http.StatusBadRequest, "invalid_input", "seats")
	o.refuse(http.MethodPost, "/api/plan/change", `{"plan":"team","interval":"month","seats":1}`, http.StatusBadRequest, "invalid_input", "seats")
	o.refuse(http.MethodPost, "/api/plan/change", `{"plan":"pro","interval":"month","seats":2}`, http.StatusBadRequest, "invalid_input", "seats")
	o.refuse(http.MethodPost, "/api/plan/change", `{"plan":"team","interval":"month","seats":2}`, http.StatusConflict, "downgrade_refused", "")
	o.refuse(http.MethodPost, "/api/plan/change", `{"plan":"pro","interval":"month"}`, http.StatusConflict, "downgrade_refused", "")
	o.call(http.MethodPost, "/api/plan/change", `{"plan":"team","interval":"month","seats":5}`, http.StatusAccepted)
}

// One count of seats on every screen (MAI-249): the members still in the team and the
// invitations waiting. A removed member, whose data waits ForgetDays for its removal, holds
// none on Members, on Plan and billing, in the session's member count or in the downgrade
// check.
func TestSeatsCountedOnce(t *testing.T) {
	w := newTeamWorld(t)
	o := w.owner
	o.call(http.MethodDelete, "/api/members/"+strconv.FormatInt(w.ids["member"], 10), "", http.StatusNoContent)
	inv := o.call(http.MethodPost, "/api/invitations", `{"email":"new@team.test","role":"member"}`, http.StatusCreated)["invitation"].(map[string]any)
	counts := func(people, used float64) {
		t.Helper()
		for _, b := range []*env{o, w.admin} {
			if s := b.call(http.MethodGet, "/api/members", "", http.StatusOK)["seats"].(map[string]any); s["used"] != used {
				t.Errorf("Members: seats = %v, want %v used", s, used)
			}
			p := b.call(http.MethodGet, "/api/plan", "", http.StatusOK)
			conform(t, b.doc, "PlanInfo", p)
			if p["seats_used"] != used {
				t.Errorf("Plan and billing: seats_used = %v, want %v", p["seats_used"], used)
			}
			if n := b.me()["members"]; n != people {
				t.Errorf("session: members = %v, want %v", n, people)
			}
		}
	}
	counts(2, 3)
	o.refuse(http.MethodPost, "/api/plan/change", `{"plan":"team","interval":"month","seats":2}`, http.StatusConflict, "downgrade_refused", "")
	o.call(http.MethodDelete, "/api/invitations/"+strconv.FormatInt(id(inv["id"]), 10), "", http.StatusNoContent)
	counts(2, 2)
	o.call(http.MethodPost, "/api/plan/change", `{"plan":"team","interval":"month","seats":2}`, http.StatusAccepted)
}

// An invitation past the paid seats adds one at once (MAI-247): the billing module is asked
// for the seats the team then needs, and only once it agrees is the invitation kept and
// sent. When it refuses nobody is invited. Joining without an invitation is still refused.
func TestInvitationAddsSeat(t *testing.T) {
	w := newTeamWorld(t)
	a := w.admin
	tenant := strconv.FormatInt(w.tenant, 10)
	if s := a.call(http.MethodGet, "/api/members", "", http.StatusOK)["seats"].(map[string]any); s["used"] != float64(3) || s["limit"] != float64(3) || s["price"] != "$8 a seat a month" {
		t.Fatalf("seats = %v", s)
	}
	a.call(http.MethodPost, "/api/invitations", `{"email":"fourth@team.test","role":"member"}`, http.StatusCreated)
	if want := []string{tenant + " sub_t team month 4"}; !slices.Equal(w.fb.changes, want) || len(w.ft.invited) != 3 {
		t.Fatalf("changes = %q, invited = %d; want %q and the invitation sent", w.fb.changes, len(w.ft.invited), want)
	}
	// Before the billing service reports the fourth seat, the next invitation asks for five.
	a.call(http.MethodPost, "/api/invitations", `{"email":"fifth@team.test","role":"member"}`, http.StatusCreated)
	if want := []string{tenant + " sub_t team month 4", tenant + " sub_t team month 5"}; !slices.Equal(w.fb.changes, want) {
		t.Fatalf("changes = %q, want %q", w.fb.changes, want)
	}
	w.fb.err = errors.New("card declined")
	r := a.do(http.MethodPost, "/api/invitations", `{"email":"sixth@team.test","role":"member"}`)
	if r.status != http.StatusBadGateway || r.body.Error.Code != "billing_error" || !strings.Contains(r.body.Error.Message, "nobody was invited") {
		t.Fatalf("invitation the billing service refused a seat for = %d %s", r.status, r.raw)
	}
	w.fb.err = nil
	if l := a.call(http.MethodGet, "/api/invitations", "", http.StatusOK)["invitations"].([]any); len(l) != 2 || len(w.ft.invited) != 4 {
		t.Errorf("invitations = %v, sent = %d; want fourth and fifth only", l, len(w.ft.invited))
	}
	if loc := a.browser().teamSignIn("user_x", "x@team.test"); loc != "/?signin_error=seats_full" {
		t.Errorf("sign-in without an invitation went to %q", loc)
	}
}

// An address with a MailRules account of its own, in any tenant, cannot be invited
// (MAI-248): it would belong to two organisations at the sign-in service and reach neither.
// A removed member's address is free again at once.
func TestInvitationRefusesAccount(t *testing.T) {
	w := newTeamWorld(t)
	o := w.owner
	q := url.Values{"sub": {"user_s"}, "email": {"solo@else.test"}, "org": {"org_solo"}}
	if r := o.browser().do(http.MethodGet, teamStart+"?"+q.Encode(), ""); r.status != http.StatusSeeOther || r.header.Get("Location") != "/" {
		t.Fatalf("sign-up elsewhere = %d %v", r.status, r.header)
	}
	r := o.do(http.MethodPost, "/api/invitations", `{"email":"Solo@Else.test","role":"member"}`)
	if r.status != http.StatusConflict || r.body.Error.Code != "has_account" || r.body.Error.Path != "email" ||
		!strings.Contains(r.body.Error.Message, "delete their own MailRules account") {
		t.Fatalf("invitation of an address with an account = %d %s", r.status, r.raw)
	}
	if len(w.fb.changes) != 0 || len(w.ft.invited) != 2 {
		t.Errorf("changes = %q, invited = %d; want neither", w.fb.changes, len(w.ft.invited))
	}
	o.call(http.MethodDelete, "/api/members/"+strconv.FormatInt(w.ids["member"], 10), "", http.StatusNoContent)
	o.call(http.MethodPost, "/api/invitations", `{"email":"member@team.test","role":"member"}`, http.StatusCreated)
}

// A removed member (MAI-253) signing in under the team's organisation again is refused until
// the team invites them; invited, they join as a new member. Removed again and put by the
// sign-in service in an organisation of their own, they get an account of their own: a new
// tenant they own, with nothing of the team, while the team keeps its people and seats.
func TestRemovedMemberSignsInAgain(t *testing.T) {
	w := newTeamWorld(t)
	o := w.owner
	o.call(http.MethodDelete, "/api/members/"+strconv.FormatInt(w.ids["member"], 10), "", http.StatusNoContent)
	if loc := o.browser().teamSignIn("user_m", "member@team.test"); loc != "/?signin_error=removed" {
		t.Fatalf("the removed member under the team without an invitation went to %q", loc)
	}
	o.call(http.MethodPost, "/api/invitations", `{"email":"member@team.test","role":"member"}`, http.StatusCreated)
	back := o.browser()
	if loc := back.teamSignIn("user_m", "member@team.test"); loc != "/" {
		t.Fatalf("the removed member invited again went to %q", loc)
	}
	again := id(back.me()["user"].(map[string]any)["id"])
	if again == w.ids["member"] {
		t.Errorf("invited again as the old user %d", again)
	}
	o.call(http.MethodDelete, "/api/members/"+strconv.FormatInt(again, 10), "", http.StatusNoContent)
	own := o.browser()
	if loc := own.teamSignInAt("user_m", "member@team.test", "org_m"); loc != "/" {
		t.Fatalf("the removed member under an organisation of their own went to %q", loc)
	}
	me := own.me()
	if u := me["user"].(map[string]any); u["role"] != "owner" || me["members"] != float64(1) || own.tenantOf("member@team.test") == w.tenant {
		t.Errorf("their own account = %v, want the owner of a new tenant of one", me)
	}
	if as := own.call(http.MethodGet, "/api/accounts", "", http.StatusOK)["items"].([]any); len(as) != 0 {
		t.Errorf("their own account sees mailboxes: %v", as)
	}
	if ms := o.call(http.MethodGet, "/api/members", "", http.StatusOK)["members"].([]any); len(ms) != 2 {
		t.Errorf("the team's members = %v, want the owner and the admin", ms)
	}
	// Their identity is now their own account's: the team's organisation is not theirs.
	if loc := o.browser().teamSignIn("user_m", "member@team.test"); loc != "/?signin_error=tenant_mismatch" {
		t.Errorf("their identity under the team's organisation went to %q", loc)
	}
}

// A seat freed is given back (MAI-247): removing a member, revoking an invitation, one
// withdrawn because it could not be sent and one expiring each queue a seat check, and the
// seat worker's LowerSeats asks the billing module for the seats then used, never below the
// plan's least (2), even when the daemon has not heard of seats bought meanwhile.
func TestFreedSeatsGoBack(t *testing.T) {
	w := newTeamWorld(t)
	o, ctx := w.owner, t.Context()
	lower := LowerSeats(o.st, w.modules)
	tenant := strconv.FormatInt(w.tenant, 10)
	paid := func(n int) {
		t.Helper()
		if _, _, err := o.st.SetPlan(ctx, w.tenant, store.Plan{Plan: store.PlanTeam, Interval: store.IntervalMonth,
			Status: store.StatusActive, Record: "sub_t", Seats: n}, o.ck.t.Unix()); err != nil {
			t.Fatal(err)
		}
	}
	// settle runs the seat worker's checks due at now, as it would, and returns the seat
	// changes the billing module was asked for meanwhile.
	settle := func(now time.Time) []string {
		t.Helper()
		checks, err := o.st.WaitingSeatChecks(ctx, now.Unix(), 10)
		if err != nil {
			t.Fatal(err)
		}
		before := len(w.fb.changes)
		for _, c := range checks {
			if err := lower(ctx, c.TenantID, now); err != nil {
				t.Fatal(err)
			}
			if err := o.st.SeatCheckDone(ctx, c); err != nil {
				t.Fatal(err)
			}
		}
		return slices.Clone(w.fb.changes[before:])
	}
	two, three := []string{tenant + " sub_t team month 2"}, []string{tenant + " sub_t team month 3"}
	if got := settle(o.ck.t); len(got) != 0 {
		t.Fatalf("seat changes before anything was freed = %q", got)
	}

	o.call(http.MethodDelete, "/api/members/"+strconv.FormatInt(w.ids["member"], 10), "", http.StatusNoContent)
	if got := settle(o.ck.t); !slices.Equal(got, two) {
		t.Errorf("after a member was removed: %q, want %q", got, two)
	}
	paid(2)
	o.call(http.MethodDelete, "/api/members/"+strconv.FormatInt(w.ids["admin"], 10), "", http.StatusNoContent)
	if got := settle(o.ck.t); !slices.Equal(got, two) {
		t.Errorf("down to one person, on the least: %q, want %q (which changes nothing)", got, two)
	}

	o.call(http.MethodPost, "/api/invitations", `{"email":"a@team.test","role":"member"}`, http.StatusCreated)
	b := o.call(http.MethodPost, "/api/invitations", `{"email":"b@team.test","role":"member"}`, http.StatusCreated)["invitation"].(map[string]any)
	if got := w.fb.changes[len(w.fb.changes)-1]; got != three[0] {
		t.Fatalf("the invitation past 2 seats asked for %q, want %q", got, three[0])
	}
	paid(3)
	o.call(http.MethodDelete, "/api/invitations/"+strconv.FormatInt(id(b["id"]), 10), "", http.StatusNoContent)
	if got := settle(o.ck.t); !slices.Equal(got, two) {
		t.Errorf("after an invitation was revoked: %q, want %q", got, two)
	}
	paid(2)

	w.ft.inviteErr = errors.New("service down")
	o.refuse(http.MethodPost, "/api/invitations", `{"email":"b@team.test","role":"member"}`, http.StatusBadGateway, "invite_failed", "")
	w.ft.inviteErr = nil
	if got := settle(o.ck.t); !slices.Equal(got, two) {
		t.Errorf("after a seat bought for an invitation that could not be sent: %q, want %q", got, two)
	}

	o.call(http.MethodPost, "/api/invitations", `{"email":"b@team.test","role":"member"}`, http.StatusCreated)
	paid(3)
	later := o.ck.t.Add((store.InvitationDays*24 + 1) * time.Hour)
	if _, _, err := o.st.Retain(ctx, w.tenant, later.Unix(), 30); err != nil {
		t.Fatal(err)
	}
	if got := settle(later); !slices.Equal(got, two) {
		t.Errorf("after the invitations expired: %q, want %q", got, two)
	}
}

// Self-host's admin is the owner of its tenant, and the team's routes are not there.
func TestSelfHostIsOwner(t *testing.T) {
	e := newEnv(t)
	e.signIn()
	if u := e.me()["user"].(map[string]any); u["role"] != "owner" {
		t.Errorf("self-host admin = %v, want an owner", u)
	}
	e.refuse(http.MethodGet, "/api/members", "", http.StatusNotFound, "not_available", "")
	e.refuse(http.MethodGet, "/api/audit", "", http.StatusNotFound, "not_available", "")
}
