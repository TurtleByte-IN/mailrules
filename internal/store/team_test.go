package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/rules"
)

// audited lists the tenant's audit rows as "action target: detail (actor)".
func audited(t *testing.T, s *Store, tenantID int64) []string {
	t.Helper()
	es, err := s.AuditLog(t.Context(), tenantID, 0, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Action + " " + e.Target + ": " + e.Detail + " (" + e.Actor + ")"
	}
	return out
}

// seats is a SeatLimit that allows n whatever the plan.
func seats(n int) SeatLimit { return func(PlanState) int { return n } }

// team is a tenant made by its owner's first sign-in, with the store to reach it.
type team struct {
	s      *Store
	owner  User
	tenant int64
}

func newTeam(t *testing.T) team {
	t.Helper()
	s, _ := open(t)
	owner, err := s.SignInIdentity(t.Context(), identity("user_owner", "owner@team.test", "org_team"), 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	return team{s: s, owner: owner, tenant: owner.TenantID}
}

// as is ctx acting as u.
func as(ctx context.Context, u User) context.Context {
	return WithActor(ctx, Actor{UserID: u.ID, Name: u.Email})
}

func TestRolesAtCreation(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	admin, err := s.CreateFirstUser(ctx, "admin@self.test", "hash", 1)
	if err != nil || admin.Role != RoleOwner {
		t.Fatalf("CreateFirstUser = %+v, %v; want an owner", admin, err)
	}
	if u, err := s.User(ctx, admin.ID); err != nil || u.Role != RoleOwner {
		t.Errorf("User = %+v, %v; want an owner", u, err)
	}
	second, err := s.CreateUser(ctx, SelfHostTenant, "second@self.test", "hash", 1)
	if err != nil || second.Role != RoleMember {
		t.Errorf("CreateUser in a tenant with people = %+v, %v; want a member", second, err)
	}
	other, err := s.CreateTenant(ctx, "workos", "org_x", 1)
	if err != nil {
		t.Fatal(err)
	}
	if first, err := s.CreateUser(ctx, other, "first@x.test", "", 1); err != nil || first.Role != RoleOwner {
		t.Errorf("CreateUser in an empty tenant = %+v, %v; want an owner", first, err)
	}
	if err := s.CreateSession(ctx, "tok", second.ID, 1, 100); err != nil {
		t.Fatal(err)
	}
	if u, _, err := s.SessionUser(ctx, "tok", 2); err != nil || u.Role != RoleMember {
		t.Errorf("SessionUser = %+v, %v; want the member's role", u, err)
	}
	if p, o, err := s.TenantOrganisation(ctx, other); err != nil || p != "workos" || o != "org_x" {
		t.Errorf("TenantOrganisation = %q, %q, %v", p, o, err)
	}
	if _, _, err := s.TenantOrganisation(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("TenantOrganisation of no tenant = %v, want ErrNotFound", err)
	}
}

// Migration 0028: everyone who existed becomes an owner, so nothing they could do stops.
func TestMigrationEveryoneIsOwner(t *testing.T) {
	sqliteOnly(t, "migrates a SQLite database part of the way")
	ctx := t.Context()
	db := migratedTo(t, 27)
	for _, email := range []string{"a@self.test", "b@self.test"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO users (email, password_hash, created_at, tenant_id) VALUES (?, 'h', 1, 1)`, email); err != nil {
			t.Fatal(err)
		}
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	s := keyed(db)
	ms, err := s.Members(ctx, SelfHostTenant)
	if err != nil || len(ms) != 2 {
		t.Fatalf("Members = %v, %v", ms, err)
	}
	for _, m := range ms {
		if m.Role != RoleOwner {
			t.Errorf("%s is %s after the migration, want owner", m.Email, m.Role)
		}
	}
	u, err := s.CreateUser(ctx, SelfHostTenant, "c@self.test", "h", 2)
	if err != nil || u.Role != RoleMember {
		t.Errorf("a user made after = %+v, %v; want a member", u, err)
	}
}

func TestJoinRoles(t *testing.T) {
	tm := newTeam(t)
	s, ctx := tm.s, t.Context()
	if tm.owner.Role != RoleOwner {
		t.Fatalf("the person who made the team is %s, want owner", tm.owner.Role)
	}
	// Invited as an admin: joins as one, and the invitation is used up.
	if _, err := s.CreateInvitation(as(ctx, tm.owner), tm.tenant, Invitation{Email: "Ada@Team.test", Role: RoleAdmin, InvitedBy: tm.owner.ID}, seats(3), 110); err != nil {
		t.Fatal(err)
	}
	ada, err := s.SignInIdentity(ctx, identity("user_ada", "ada@team.test", "org_team"), 120, seats(3))
	if err != nil || ada.Role != RoleAdmin || ada.TenantID != tm.tenant {
		t.Fatalf("invited sign-in = %+v, %v; want an admin of the team", ada, err)
	}
	if inv, err := s.Invitations(ctx, tm.tenant, 120); err != nil || len(inv) != 0 {
		t.Errorf("invitations after the join = %v, %v; want none", inv, err)
	}
	// Without an invitation: a member, while a seat is free.
	bo, err := s.SignInIdentity(ctx, identity("user_bo", "bo@team.test", "org_team"), 130, seats(3))
	if err != nil || bo.Role != RoleMember {
		t.Fatalf("sign-in without an invitation = %+v, %v; want a member", bo, err)
	}
	_, err = s.SignInIdentity(ctx, identity("user_cy", "cy@team.test", "org_team"), 140, seats(3))
	var full *SeatLimitError
	if !errors.As(err, &full) || !errors.Is(err, ErrSeatLimit) || full.Limit != 3 || full.Used != 3 {
		t.Fatalf("sign-in over the seats = %v, want a SeatLimitError of 3", err)
	}
	if ms, _ := s.Members(ctx, tm.tenant); len(ms) != 3 {
		t.Errorf("members after the refused sign-in = %d, want 3", len(ms))
	}
	// A returning person keeps their role.
	if again, err := s.SignInIdentity(ctx, identity("user_ada", "ada@team.test", "org_team"), 150, seats(3)); err != nil || again.Role != RoleAdmin {
		t.Errorf("returning sign-in = %+v, %v; want the admin back", again, err)
	}
	want := []string{
		"member.joined owner@team.test: made the team (owner@team.test)",
		"invitation.created Ada@Team.test: as Admin (owner@team.test)",
		"member.joined ada@team.test: as Admin, invited by owner@team.test (ada@team.test)",
		"member.joined bo@team.test: as Member, without an invitation (bo@team.test)",
	}
	if got := audited(t, s, tm.tenant); !slices.Equal(got, want) {
		t.Errorf("audit log =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestSetRoleKeepsAnOwner(t *testing.T) {
	tm := newTeam(t)
	s, ctx := tm.s, as(t.Context(), tm.owner)
	mate, err := s.SignInIdentity(t.Context(), identity("user_mate", "mate@team.test", "org_team"), 110, nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		user int64
		role string
		want error
	}{
		{"an unknown role", mate.ID, "boss", ErrBadRole},
		{"no such member", 999, RoleAdmin, ErrNotFound},
		{"the last owner steps down", tm.owner.ID, RoleAdmin, ErrLastOwner},
		{"a member made admin", mate.ID, RoleAdmin, nil},
		{"the same role again", mate.ID, RoleAdmin, nil},
		{"a second owner", mate.ID, RoleOwner, nil},
		{"the first owner steps down", tm.owner.ID, RoleMember, nil},
		{"the last owner again", mate.ID, RoleMember, ErrLastOwner},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := s.SetRole(ctx, tm.tenant, tt.user, tt.role, 200); !errors.Is(err, tt.want) {
				t.Errorf("SetRole = %v, want %v", err, tt.want)
			}
		})
	}
	if last, err := s.LastOwner(ctx, User{ID: mate.ID, TenantID: tm.tenant, Role: RoleOwner}); err != nil || !last {
		t.Errorf("LastOwner of the only owner = %v, %v; want true", last, err)
	}
	if last, err := s.LastOwner(ctx, User{ID: tm.owner.ID, TenantID: tm.tenant, Role: RoleMember}); err != nil || last {
		t.Errorf("LastOwner of a member = %v, %v; want false", last, err)
	}
	got := audited(t, s, tm.tenant)
	want := []string{
		"member.role_changed mate@team.test: Member to Admin (owner@team.test)",
		"member.role_changed mate@team.test: Admin to Owner (owner@team.test)",
		"member.role_changed owner@team.test: Owner to Member (owner@team.test)",
	}
	if !slices.Equal(got[len(got)-3:], want) {
		t.Errorf("audit log ends\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestInvitations(t *testing.T) {
	tm := newTeam(t)
	s, ctx := tm.s, as(t.Context(), tm.owner)
	inv, err := s.CreateInvitation(ctx, tm.tenant, Invitation{Email: "new@team.test", Role: RoleMember, InvitedBy: tm.owner.ID}, seats(2), 1000)
	if err != nil || inv.ID == 0 || inv.ExpiresAt != 1000+InvitationDays*day {
		t.Fatalf("CreateInvitation = %+v, %v", inv, err)
	}
	if err := s.SetInvitationRecord(ctx, tm.tenant, inv.ID, "inv_123"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetInvitationRecord(ctx, tm.tenant, 999, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetInvitationRecord of none = %v, want ErrNotFound", err)
	}
	list, err := s.Invitations(ctx, tm.tenant, 1001)
	if err != nil || len(list) != 1 || list[0].Record != "inv_123" || list[0].InvitedByEmail != "owner@team.test" {
		t.Fatalf("Invitations = %+v, %v", list, err)
	}
	tests := []struct {
		name  string
		inv   Invitation
		seats SeatLimit
		at    int64
		want  error
	}{
		{"an unknown role", Invitation{Email: "x@team.test", Role: "boss"}, nil, 1001, ErrBadRole},
		{"a member", Invitation{Email: "OWNER@team.test", Role: RoleMember}, nil, 1001, ErrAlreadyMember},
		{"an address invited already", Invitation{Email: "new@team.test", Role: RoleAdmin}, nil, 1001, ErrAlreadyInvited},
		{"no seat left", Invitation{Email: "third@team.test", Role: RoleMember}, seats(2), 1001, ErrSeatLimit},
		{"no limit", Invitation{Email: "third@team.test", Role: RoleMember}, seats(NoLimit), 1001, nil},
		{"the first one expired", Invitation{Email: "new@team.test", Role: RoleAdmin}, nil, 1000 + InvitationDays*day, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := s.CreateInvitation(ctx, tm.tenant, tt.inv, tt.seats, tt.at); !errors.Is(err, tt.want) {
				t.Errorf("CreateInvitation = %v, want %v", err, tt.want)
			}
		})
	}
	later := int64(1000 + InvitationDays*day + 1)
	list, _ = s.Invitations(ctx, tm.tenant, later)
	if len(list) != 1 || list[0].Email != "new@team.test" || list[0].Role != RoleAdmin {
		t.Fatalf("invitations after the first expired = %+v; want the new one alone", list)
	}
	if _, err := s.RevokeInvitation(ctx, tm.tenant, 999, "", later); !errors.Is(err, ErrNotFound) {
		t.Errorf("RevokeInvitation of none = %v, want ErrNotFound", err)
	}
	got, err := s.RevokeInvitation(ctx, tm.tenant, list[0].ID, "it could not be sent", later)
	if err != nil || got.Email != "new@team.test" {
		t.Errorf("RevokeInvitation = %+v, %v", got, err)
	}
	// What retention keeps: nothing expired.
	if _, _, err := s.Retain(ctx, tm.tenant, later+1, 30); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.Invitations(ctx, tm.tenant, 0); len(list) != 0 {
		t.Errorf("invitations after retention = %+v, want none", list)
	}
	want := []string{
		"invitation.created new@team.test: as Member (owner@team.test)",
		"invitation.created third@team.test: as Member (owner@team.test)",
		"invitation.created new@team.test: as Admin (owner@team.test)",
		"invitation.withdrawn new@team.test: it could not be sent (owner@team.test)",
	}
	if got := audited(t, s, tm.tenant)[1:]; !slices.Equal(got, want) {
		t.Errorf("audit log =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestAcceptInvitation(t *testing.T) {
	tm := newTeam(t)
	s := tm.s
	owner := as(t.Context(), tm.owner)
	service := WithActor(t.Context(), Actor{Name: "Sign-in service"})
	for _, email := range []string{"early@team.test", "late@team.test"} {
		if _, err := s.CreateInvitation(owner, tm.tenant, Invitation{Email: email, Role: RoleAdmin, InvitedBy: tm.owner.ID}, nil, 100); err != nil {
			t.Fatal(err)
		}
	}
	// Accepted before the first sign-in: it waits, then the sign-in uses it.
	if err := s.AcceptInvitation(service, tm.tenant, "early@team.test", 200); err != nil {
		t.Fatal(err)
	}
	list, _ := s.Invitations(owner, tm.tenant, 200)
	if i := slices.IndexFunc(list, func(v Invitation) bool { return v.Email == "early@team.test" }); i < 0 || list[i].AcceptedAt != 200 || list[i].ExpiresAt != 200+InvitationDays*day {
		t.Errorf("accepted invitation = %+v", list)
	}
	if u, err := s.SignInIdentity(t.Context(), identity("user_early", "early@team.test", "org_team"), 300, seats(1)); err != nil || u.Role != RoleAdmin {
		t.Errorf("sign-in after acceptance = %+v, %v; want an admin, its seat held", u, err)
	}
	// Signed in before the service's report came: the report gives the role.
	late, err := s.SignInIdentity(t.Context(), identity("user_late", "late@team.test", "org_team"), 400, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The invitation was used by the sign-in; a new one for an existing member is refused,
	// so the report path is reached through a member who joined without using it.
	if err := s.AcceptInvitation(service, tm.tenant, "late@team.test", 410); !errors.Is(err, ErrNotFound) {
		t.Errorf("AcceptInvitation of a used invitation = %v, want ErrNotFound", err)
	}
	if late.Role != RoleAdmin {
		t.Errorf("late joiner = %s, want admin", late.Role)
	}
	if err := s.AcceptInvitation(service, tm.tenant, "nobody@team.test", 410); !errors.Is(err, ErrNotFound) {
		t.Errorf("AcceptInvitation of nobody = %v, want ErrNotFound", err)
	}
}

// The report of an acceptance can come after the person joined without the invitation (a
// sign-in under the organisation the service added them to, as company sign-in does): the
// role is given then.
func TestAcceptInvitationAfterJoining(t *testing.T) {
	tm := newTeam(t)
	s := tm.s
	owner := as(t.Context(), tm.owner)
	mate, err := s.SignInIdentity(t.Context(), identity("user_mate", "mate@team.test", "org_team"), 110, nil)
	if err != nil || mate.Role != RoleMember {
		t.Fatalf("join = %+v, %v", mate, err)
	}
	// Written by hand: CreateInvitation refuses an address that is a member already.
	if _, err := tm.s.db.ExecContext(WithTenant(t.Context(), tm.tenant), `INSERT INTO invitations (tenant_id, email, role, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		tm.tenant, "mate@team.test", RoleOwner, 100, 100+InvitationDays*day); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptInvitation(WithActor(t.Context(), Actor{Name: "Sign-in service"}), tm.tenant, "MATE@team.test", 200); err != nil {
		t.Fatal(err)
	}
	ms, _ := s.Members(owner, tm.tenant)
	if i := slices.IndexFunc(ms, func(m Member) bool { return m.UserID == mate.ID }); i < 0 || ms[i].Role != RoleOwner {
		t.Errorf("members = %+v; want the mate an owner", ms)
	}
	if list, _ := s.Invitations(owner, tm.tenant, 200); len(list) != 0 {
		t.Errorf("invitations = %+v, want it used up", list)
	}
	got := audited(t, s, tm.tenant)
	if last := got[len(got)-1]; last != "invitation.accepted MATE@team.test: joined as Owner (Sign-in service)" {
		t.Errorf("last audit row = %q", last)
	}
}

// A member removed from the team, or forgotten, is recorded; a team that loses its last
// owner gets the next one.
func TestRemovalRecordsAndKeepsAnOwner(t *testing.T) {
	tm := newTeam(t)
	s, ctx := tm.s, t.Context()
	admin, err := s.SignInIdentity(ctx, identity("user_admin", "admin@team.test", "org_team"), 110, nil)
	if err != nil {
		t.Fatal(err)
	}
	member, err := s.SignInIdentity(ctx, identity("user_member", "member@team.test", "org_team"), 105, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetRole(ctx, tm.tenant, admin.ID, RoleAdmin, 120); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveMember(as(ctx, tm.owner), "workos", "user_member", 200); err != nil {
		t.Fatal(err)
	}
	// The owner's sign-in is deleted: the admin, though younger than the member was, is next.
	if _, err := s.ForgetIdentity(WithActor(ctx, Actor{Name: "Sign-in service"}), "workos", "user_owner", 300); err != nil {
		t.Fatal(err)
	}
	ms, _ := s.Members(ctx, tm.tenant)
	if len(ms) != 1 || ms[0].UserID != admin.ID || ms[0].Role != RoleOwner {
		t.Errorf("members = %+v; want the admin alone, an owner", ms)
	}
	// Forgetting again records nothing more.
	if _, err := s.ForgetIdentity(WithActor(ctx, Actor{Name: "Sign-in service"}), "workos", "user_owner", 310); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"member.removed member@team.test: removed from the team (owner@team.test)",
		"member.role_changed admin@team.test: Admin to Owner: the team's last owner left (MailRules)",
		"member.removed owner@team.test: their sign-in was deleted (Sign-in service)",
	}
	if got := audited(t, s, tm.tenant); !slices.Equal(got[len(got)-3:], want) {
		t.Errorf("audit log ends\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	_ = member
}

// Every change to what the audit log covers is recorded under the actor, in its own
// transaction, and nothing is recorded without one.
func TestAuditRecordsChanges(t *testing.T) {
	tm := newTeam(t)
	s := tm.s
	ctx := as(t.Context(), tm.owner)
	start := len(audited(t, s, tm.tenant))

	// Nothing without an actor.
	if _, err := s.CreateRule(t.Context(), tm.tenant, rules.Rule{UserID: tm.owner.ID, Name: "Quiet", Actions: []rules.Action{{Type: rules.ActKeep}}}, 1); err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRule(ctx, tm.tenant, rules.Rule{UserID: tm.owner.ID, Name: "Bills", Intent: "bills", Actions: []rules.Action{{Type: rules.ActKeep}}, Enabled: true}, 2)
	if err != nil {
		t.Fatal(err)
	}
	r.Name, r.Enabled, r.Actions = "Invoices", false, []rules.Action{{Type: rules.ActArchive}}
	if err := s.UpdateRule(ctx, tm.tenant, r, 3); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRule(ctx, tm.tenant, rules.Rule{ID: 999, Name: "x"}, 3); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateRule of none = %v, want ErrNotFound", err)
	}
	all, _ := s.Rules(ctx, tm.tenant)
	ids := []int64{all[1].ID, all[0].ID}
	if err := s.ReorderRules(ctx, tm.tenant, ids); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutSenderRule(ctx, tm.tenant, rules.SenderRule{UserID: tm.owner.ID, MatchType: rules.MatchAddress, Value: "news@shop.test",
		Verdict: rules.VerdictMove, Folder: "Shops", Source: "user"}, 4); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSenderRule(ctx, tm.tenant, rules.MatchAddress, "news@shop.test"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSenderRule(ctx, tm.tenant, rules.MatchAddress, "never@shop.test"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRule(ctx, tm.tenant, r.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRule(ctx, tm.tenant, r.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteRule again = %v, want ErrNotFound", err)
	}
	a, err := s.CreateAccount(ctx, Account{UserID: tm.owner.ID, Label: "Work", Preset: "generic", Host: "imap.team.test", Port: 993,
		TLSMode: "implicit", Username: "owner@team.test", CreatedAt: 5}, "app-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountShared(ctx, a.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountShared(ctx, 999, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetAccountShared of none = %v, want ErrNotFound", err)
	}
	a.Label, a.Status = "Office", "paused"
	if err := s.UpdateAccount(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateAccount(ctx, a); err != nil { // the same again: nothing to record
		t.Fatal(err)
	}
	if err := s.SetAccountSecret(ctx, a.ID, "new-app-password"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountSecret(WithoutActor(ctx), a.ID, "rotated"); err != nil { // a provider's rotation
		t.Fatal(err)
	}
	if _, err := s.DeleteAccount(ctx, a.ID, 6); err != nil {
		t.Fatal(err)
	}
	on, key := "false", `"sealed"`
	if err := s.SetSettings(ctx, tm.tenant, map[string]*string{"dry_run": &on, "key.anthropic_api_key": &key, "decider_model": &key}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSettings(ctx, tm.tenant, map[string]*string{"dry_run": &on, "key.anthropic_api_key": nil}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSettings(ctx, tm.tenant, map[string]*string{"retention_days": nil}); err != nil { // nothing was stored
		t.Fatal(err)
	}
	d, err := s.CreateDestination(ctx, Destination{TenantID: tm.tenant, Name: "Ops", Kind: "webhook"}, DestinationSecret{URL: "https://hooks.test/x"}, 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteDestination(ctx, tm.tenant, d.ID, 8); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SetPlan(WithActor(t.Context(), Actor{Name: "Billing service"}), tm.tenant, Plan{Plan: PlanTeam, Interval: IntervalMonth,
		Status: StatusActive, Seats: 5}, 9); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SetPlan(WithActor(t.Context(), Actor{Name: "Billing service"}), tm.tenant, Plan{Plan: PlanTeam, Interval: IntervalMonth,
		Status: StatusActive, Seats: 5}, 10); err != nil { // the same again
		t.Fatal(err)
	}
	want := []string{
		"rule.created Bills: on (owner@team.test)",
		"rule.changed Invoices: renamed from Bills, actions, turned off (owner@team.test)",
		"rules.reordered : 2 rules (owner@team.test)",
		"sender_rule.set news@shop.test: move to Shops (owner@team.test)",
		"sender_rule.deleted news@shop.test:  (owner@team.test)",
		"rule.deleted Invoices:  (owner@team.test)",
		"mailbox.added owner@team.test: Work, imap.team.test (owner@team.test)",
		"mailbox.changed owner@team.test: shared with the team (owner@team.test)",
		"mailbox.changed owner@team.test: renamed from Work, paused (owner@team.test)",
		"mailbox.changed owner@team.test: new password or sign-in (owner@team.test)",
		"mailbox.removed owner@team.test:  (owner@team.test)",
		"settings.changed : anthropic_api_key, decider_model, dry_run: false (owner@team.test)",
		"settings.changed : anthropic_api_key removed (owner@team.test)",
		"notification.added Ops: webhook (owner@team.test)",
		"notification.removed Ops:  (owner@team.test)",
		"plan.changed team: trial to team, month, active, 5 seats (Billing service)",
	}
	got := audited(t, s, tm.tenant)[start:]
	if !slices.Equal(got, want) {
		t.Errorf("audit log =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// A plan sold by the seat needs seats, and only it has them.
	for _, p := range []Plan{{Plan: PlanTeam}, {Plan: PlanPro, Seats: 2}, {Plan: PlanTeam, Seats: -1}} {
		if _, _, err := s.SetPlan(ctx, tm.tenant, p, 11); !errors.Is(err, ErrBadPlan) {
			t.Errorf("SetPlan(%+v) = %v, want ErrBadPlan", p, err)
		}
	}
	if p, err := s.TenantPlan(ctx, tm.tenant, 12); err != nil || p.Seats != 5 {
		t.Errorf("TenantPlan = %+v, %v; want 5 seats", p, err)
	}
}

// Learned and "always" sender rules a person's correction or answer makes are recorded too.
func TestAuditRecordsLearnedSenderRules(t *testing.T) {
	tm := newTeam(t)
	s := tm.s
	ctx := as(t.Context(), tm.owner)
	r, err := s.CreateRule(ctx, tm.tenant, rules.Rule{UserID: tm.owner.ID, Name: "Food", Intent: "food", Actions: []rules.Action{{Type: rules.ActKeep}}, Enabled: true}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.AddLearnedSenderRule(ctx, tm.tenant, tm.owner.ID, "chef@food.test", r.ID, 2); err != nil || !ok {
		t.Fatalf("AddLearnedSenderRule = %v, %v", ok, err)
	}
	if ok, err := s.AddLearnedSenderRule(ctx, tm.tenant, tm.owner.ID, "chef@food.test", r.ID, 3); err != nil || ok {
		t.Errorf("AddLearnedSenderRule again = %v, %v; want nothing added", ok, err)
	}
	got := audited(t, s, tm.tenant)
	if last := got[len(got)-1]; last != "sender_rule.set chef@food.test: route, learned (owner@team.test)" {
		t.Errorf("last audit row = %q", last)
	}
}

func TestAuditLogIsAppendOnlyAndKeptAYear(t *testing.T) {
	tm := newTeam(t)
	s, ctx := tm.s, as(t.Context(), tm.owner)
	for _, at := range []int64{1000, 2000} {
		if _, err := s.CreateRule(ctx, tm.tenant, rules.Rule{UserID: tm.owner.ID, Name: "R" + string(rune('0'+at/1000)), Actions: []rules.Action{{Type: rules.ActKeep}}}, at); err != nil {
			t.Fatal(err)
		}
	}
	if es, err := s.AuditLog(ctx, tm.tenant, 1500, 2500); err != nil || len(es) != 1 || es[0].Target != "R2" || es[0].ActorID != tm.owner.ID {
		t.Errorf("AuditLog(1500, 2500) = %+v, %v; want R2 alone", es, err)
	}
	if es, err := s.AuditLog(ctx, tm.tenant, 0, 0); err != nil || len(es) != 3 {
		t.Errorf("AuditLog(0, now) = %d rows, %v; want 3", len(es), err)
	}
	if _, err := s.db.ExecContext(WithTenant(t.Context(), tm.tenant), `UPDATE audit_log SET actor = 'someone else'`); err == nil {
		t.Error("an audit row was changed: the log must be append-only")
	}
	if _, _, err := s.Retain(ctx, tm.tenant, 1500+AuditDays*day, 30); err != nil {
		t.Fatal(err)
	}
	if es, _ := s.AuditLog(ctx, tm.tenant, 0, 1<<40); len(es) != 1 || es[0].Target != "R2" {
		t.Errorf("after retention: %+v; want R2 alone", es)
	}
}
