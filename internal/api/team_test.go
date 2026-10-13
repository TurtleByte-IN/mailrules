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

// teamModules are a sign-in module that invites people (ft) and a billing module whose Team
// plan, sold by the seat, includes the team.
func teamModules(ft *fakeTeam) []ext.Module {
	one := ext.Limits{Mailboxes: 3, AIEmails: 100, Seats: 1}
	team := termsGiving("Team", ext.Limits{Mailboxes: 2, AIEmails: 5000}, "team")
	team.PerSeat, team.MinSeats = true, 2
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
	return []ext.Module{sso, billingModule(&fakeBilling{}, plans)}
}

// teamSignIn signs this browser in through the team module and returns where it was sent.
func (e *env) teamSignIn(sub, email string) string {
	e.t.Helper()
	q := url.Values{"sub": {sub}, "email": {email}, "org": {"org_team"}}
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
	owner, admin, member *env
	tenant               int64
	ids                  map[string]int64 // user ids by role
}

func newTeamWorld(t *testing.T) teamWorld {
	t.Helper()
	w := teamWorld{ft: &fakeTeam{}, ids: map[string]int64{}}
	e := newEnv(t, teamModules(w.ft)...)
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

	// Seats: every one is taken, so another invitation is refused with a sentence.
	o.refuse(http.MethodPost, "/api/invitations", `{"email":"fourth@team.test","role":"member"}`, http.StatusConflict, "seat_limit", "")
	// Someone the service lets in without an invitation is refused too.
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
		"admin@team.test,member.joined,admin@team.test,\"as Admin, invited by owner@team.test\"",
		"member@team.test,mailbox.added,me@example.test",
		"admin@team.test,member.removed,member@team.test,removed from the team",
		"owner@team.test,invitation.withdrawn,later@team.test,it could not be sent",
		"owner@team.test,invitation.revoked,later@team.test,",
	} {
		if !strings.Contains(csv, want) {
			t.Errorf("audit CSV lacks %q:\n%s", want, csv)
		}
	}
	entries := a.call(http.MethodGet, "/api/audit?format=json", "", http.StatusOK)["entries"].([]any)
	conform(t, a.doc, "AuditEntry", entries[0])
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
	w.admin.refuse(http.MethodGet, "/api/plan", "", http.StatusForbidden, "role_required", "")
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
