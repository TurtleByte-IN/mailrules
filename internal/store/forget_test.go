package store

import (
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
)

// fworld is the self-host admin plus three hosted tenants, each with everything a tenant
// can hold: Solo (one person), Team (a member who will leave, and a teammate who joined
// before them) and Other (one person, who must stay untouched by anything done to the
// rest). The leaver owns a private mailbox and a shared one; the team's rules name both.
type fworld struct {
	s                 *Store
	db                *sql.DB
	admin, solo       User
	leaver, mate      User
	other             User
	adminBox, soloBox int64
	leaverPriv        int64 // the leaver's mailbox, not shared
	leaverShared      int64 // the leaver's mailbox, shared with the team
	mateBox           int64
	otherBox          int64
	scopedRule        rules.Rule // the team's rule limited to the leaver's private mailbox
}

const (
	fNow = 1_000_000 // when the identities are forgotten
	fDue = fNow + ForgetDays*day
)

func identity(sub, email, org string) Identity {
	return Identity{Provider: "workos", Subject: sub, Email: email, Tenant: org}
}

func forgetWorld(t *testing.T) fworld {
	t.Helper()
	s, db := open(t)
	ctx := t.Context()
	w := fworld{s: s, db: db}
	var err error
	if w.admin, err = s.CreateFirstUser(ctx, "admin@self.test", "hash", 1); err != nil {
		t.Fatal(err)
	}
	signIn := func(sub, email, org string, at int64) User {
		u, err := s.SignInIdentity(ctx, identity(sub, email, org), at, nil)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	w.solo = signIn("user_solo", "solo@example.com", "org_solo", 10)
	w.mate = signIn("user_mate", "mate@example.com", "org_team", 20)
	w.leaver = signIn("user_leaver", "leaver@example.com", "org_team", 30)
	w.other = signIn("user_other", "other@example.com", "org_other", 40)

	w.adminBox = w.seed(t, w.admin, "admin")
	w.soloBox = w.seed(t, w.solo, "solo")
	w.mateBox = w.seed(t, w.mate, "mate")
	w.leaverPriv = w.seed(t, w.leaver, "leaverpriv")
	w.leaverShared = w.seed(t, w.leaver, "leavershared")
	if err := s.SetAccountShared(ctx, w.leaverShared, true); err != nil {
		t.Fatal(err)
	}
	// Stored model API keys (settings rows under "key."): the solo tenant's go with its only
	// member; the team's stay when a member leaves.
	for _, tenant := range []int64{w.solo.TenantID, w.leaver.TenantID, w.other.TenantID} {
		if err := s.SetSetting(ctx, tenant, "key.anthropic_api_key", `{"sealed":"x"}`); err != nil {
			t.Fatal(err)
		}
	}
	w.otherBox = w.seed(t, w.other, "other")
	if w.scopedRule, err = s.CreateRule(ctx, w.leaver.TenantID, rules.Rule{UserID: w.leaver.ID, AccountID: w.leaverPriv,
		Name: "Leaver only", Intent: "x", Enabled: true, Actions: []rules.Action{{Type: rules.ActKeep}}}, 100); err != nil {
		t.Fatal(err)
	}
	return w
}

// seed gives the user a mailbox and, in their tenant, one of everything: a folder, a
// contact, an email decided, acted on and corrected, a rule for the mailbox and one for
// every mailbox, a sender rule, a batch, a setting, a usage row, a session, a summary, and a
// notification destination with a notice about the email and a delivery of it. It returns
// the mailbox.
func (w fworld) seed(t *testing.T, u User, name string) int64 {
	t.Helper()
	s, ctx := w.s, t.Context()
	a, err := s.CreateAccount(ctx, Account{UserID: u.ID, Label: name, Preset: "generic",
		Host: "imap.test", Port: 993, TLSMode: "implicit", Username: name + "@imap.test", CreatedAt: 1}, "pw-"+name)

	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SaveFolders(ctx, a.ID, []Folder{{Name: "INBOX"}}))
	_, err = w.db.ExecContext(ctx, `INSERT INTO contacts (account_id, address, last_sent_at) VALUES (?, ?, 1)`, a.ID, "friend@"+name+".test")
	must(err)
	m, _, err := s.IngestMessage(ctx, mail.MsgRef{AccountID: a.ID, Folder: "INBOX", UIDValidity: 1, UID: 1}, 1000)
	must(err)
	d, err := s.AddDecision(ctx, Decision{MessageID: m.ID, Stage: "sender", CreatedAt: 1000}, StateActed)
	must(err)
	batch, err := s.CreateBatch(ctx, u.TenantID, "live", "done", 1000)
	must(err)
	_, err = s.InsertAction(ctx, Action{DecisionID: d, BatchID: batch, MessageID: m.ID, AccountID: a.ID, Kind: "move",
		Folder: "Food", Status: ActionDone, CreatedAt: 1000, Before: Snapshot{Folder: "INBOX", UIDValidity: 1, UID: 1}})
	must(err)
	_, err = w.db.ExecContext(ctx, `INSERT INTO corrections (message_id, example, created_at) VALUES (?, '{}', 1000)`, m.ID)
	must(err)
	scoped, err := s.CreateRule(ctx, u.TenantID, rules.Rule{UserID: u.ID, AccountID: a.ID, Name: "Box " + name,
		Intent: "x", Enabled: true, Actions: []rules.Action{{Type: rules.ActKeep}}}, 100)
	must(err)
	_, err = s.CreateRule(ctx, u.TenantID, rules.Rule{UserID: u.ID, Name: "All " + name,
		Intent: "y", Enabled: true, Actions: []rules.Action{{Type: rules.ActKeep}}}, 100)
	must(err)
	_, err = s.PutSenderRule(ctx, u.TenantID, rules.SenderRule{UserID: u.ID, MatchType: rules.MatchDomain, Value: name + ".test",
		RuleID: scoped.ID, Verdict: rules.VerdictRoute, Source: "user"}, 100)
	must(err)
	must(s.SetSetting(ctx, u.TenantID, "seed_"+name, `"x"`))
	must(s.AddUsage(ctx, u.TenantID, false, "2026-10-01", "anthropic", "claude-haiku-4-5", "decide", 1, 10, 10, 0.01))
	must(s.SpendAIEmails(ctx, u.TenantID, time.Unix(1000, 0), 1))
	_, err = s.SpendCleanupEmail(ctx, u.TenantID, time.Unix(1000, 0), 10_000)
	must(err)
	must(s.CreateSession(ctx, "session-"+name, u.ID, 1000, fDue*2))
	_, _, _, err = s.ClaimSummary(ctx, u.ID, 2000, 2000, 3)
	must(err)
	dest, err := s.CreateDestination(ctx, Destination{TenantID: u.TenantID, Name: "Hook " + name, Kind: DestinationWebhook},
		DestinationSecret{URL: "https://hooks.test/" + name, Secret: "whsec_" + name}, 1000)

	must(err)
	var notice int64
	must(w.db.QueryRowContext(ctx, `INSERT INTO notices (tenant_id, destination_id, account_id, message_id, rule_id, rule_name, payload, created_at, next_at)
		VALUES (?, ?, ?, ?, ?, 'Box', '{}', 1000, 1000) RETURNING id`, u.TenantID, dest.ID, a.ID, m.ID, scoped.ID).Scan(&notice))
	_, err = s.RecordDelivery(ctx, notice, true, "", 1000)
	must(err)
	return a.ID
}

// tenantRows counts, per table, the rows that belong to a tenant.
func (w fworld) tenantRows(t *testing.T, tenantID int64) map[string]int {
	t.Helper()
	const (
		users    = `(SELECT id FROM users WHERE tenant_id = ?1)`
		accounts = `(SELECT id FROM accounts WHERE tenant_id = ?1)`
		messages = `(SELECT id FROM messages WHERE account_id IN ` + accounts + `)`
	)
	out := map[string]int{}
	for table, where := range map[string]string{
		"tenants":             `id = ?1`,
		"users":               `tenant_id = ?1`,
		"sessions":            `user_id IN ` + users,
		"identities":          `user_id IN ` + users,
		"summaries":           `user_id IN ` + users,
		"forgotten":           `user_id IN ` + users,
		"accounts":            `tenant_id = ?1`,
		"folders":             `account_id IN ` + accounts,
		"contacts":            `account_id IN ` + accounts,
		"messages":            `account_id IN ` + accounts,
		"actions":             `account_id IN ` + accounts,
		"decisions":           `message_id IN ` + messages,
		"corrections":         `message_id IN ` + messages,
		"rules":               `tenant_id = ?1`,
		"sender_rules":        `tenant_id = ?1`,
		"batches":             `tenant_id = ?1`,
		"settings":            `tenant_id = ?1`,
		"usage_daily":         `tenant_id = ?1`,
		"ai_emails":           `tenant_id = ?1`,
		"first_cleanup":       `tenant_id = ?1`,
		"notify_destinations": `tenant_id = ?1`,
		"notices":             `tenant_id = ?1`,
		"notify_deliveries":   `tenant_id = ?1`,
		"plans":               `tenant_id = ?1`,
		"removed_identities":  `tenant_id = ?1`,
	} {
		var n int
		if err := w.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+table+` WHERE `+where, tenantID).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		out[table] = n
	}
	return out
}

func TestForgetIdentityFreesEmailAndEndsSessions(t *testing.T) {
	w := forgetWorld(t)
	s, ctx := w.s, t.Context()
	if _, err := s.ForgetIdentity(ctx, "workos", "user_solo", fNow); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SessionUser(ctx, "session-solo", fNow); !errors.Is(err, ErrNotFound) {
		t.Errorf("the forgotten person's session still works: %v", err)
	}
	if _, _, err := s.SessionUser(ctx, "session-other", fNow); err != nil {
		t.Errorf("someone else's session ended: %v", err)
	}
	if _, err := s.UserByEmail(ctx, "solo@example.com"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the email is still held: %v", err)
	}
	// The sign-in service's new id for the same person signs up at once, as a new user.
	fresh, err := s.SignInIdentity(ctx, identity("user_solo_2", "solo@example.com", "org_solo_2"), fNow+1, nil)
	if err != nil {
		t.Fatalf("a new identity with the freed email: %v", err)
	}
	if fresh.ID == w.solo.ID || fresh.TenantID == w.solo.TenantID {
		t.Errorf("the new identity got the old user or tenant: %+v, old %+v", fresh, w.solo)
	}
	if as, _ := s.VisibleAccounts(ctx, fresh.Viewer()); len(as) != 0 {
		t.Errorf("the new user sees the old data: %+v", as)
	}
	// Nothing is removed yet, and the forgotten user gets no summary email.
	if got := w.tenantRows(t, w.solo.TenantID); got["accounts"] != 1 || got["rules"] != 2 || got["messages"] != 1 {
		t.Errorf("data went before its time: %v", got)
	}
	us, err := s.Users(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range us {
		if u.ID == w.solo.ID {
			t.Errorf("Users lists the forgotten user: %+v", u)
		}
	}
}

func TestForgetIdentityIsIdempotentAndMissesTheAdmin(t *testing.T) {
	w := forgetWorld(t)
	s, ctx := w.s, t.Context()
	for _, at := range []int64{fNow, fNow + 5*day} {
		if _, err := s.ForgetIdentity(ctx, "workos", "user_solo", at); err != nil {
			t.Fatalf("forget at %d: %v", at, err)
		}
	}
	// The second call did not move the removal.
	for _, c := range []struct {
		at   int64
		want int
	}{{fDue - 1, 0}, {fDue, 1}} {
		due, err := s.DueForRemoval(ctx, c.at)
		if err != nil {
			t.Fatal(err)
		}
		if len(due) != c.want {
			t.Errorf("due at %d: %+v, want %d", c.at, due, c.want)
		}
	}
	// Identities nobody has, the admin's address and the empty identity reach no one.
	for _, id := range [][2]string{{"workos", "nobody"}, {"", ""}, {"workos", w.admin.Email}, {"password", strconv.FormatInt(w.admin.ID, 10)}} {
		if f, err := s.ForgetIdentity(ctx, id[0], id[1], fNow); err != nil || f.UserID != 0 || len(f.Accounts) != 0 {
			t.Errorf("forget %v: %+v, %v", id, f, err)
		}
	}
	if a, err := s.Account(ctx, w.soloBox); err != nil || a.LastEventAt != fNow {
		t.Errorf("the second call touched the mailbox: %+v, %v", a, err)
	}
	if a, err := s.Account(ctx, w.adminBox); err != nil || a.SecretGone || a.Status != "new" {
		t.Errorf("the admin's mailbox = %+v, %v", a, err)
	}
	if due, _ := s.DueForRemoval(ctx, fDue*2); len(due) != 1 || due[0].UserID != w.solo.ID {
		t.Errorf("due = %+v, want only the solo user", due)
	}
	if u, err := s.User(ctx, w.admin.ID); err != nil || u.Email != w.admin.Email {
		t.Errorf("admin = %+v, %v", u, err)
	}
}

// The same identity signing in within the wait gets everything back and nothing is removed.
func TestForgottenIdentityComesBack(t *testing.T) {
	w := forgetWorld(t)
	s, ctx := w.s, t.Context()
	before := w.tenantRows(t, w.solo.TenantID)
	f, err := s.ForgetIdentity(ctx, "workos", "user_solo", fNow)
	if err != nil {
		t.Fatal(err)
	}
	before["settings"]-- // the model key went at once and does not come back
	if len(f.Accounts) != 1 || !f.Keys {
		t.Errorf("forget = %+v", f)
	}
	back, err := s.SignInIdentity(ctx, identity("user_solo", "solo@example.com", "org_solo"), fDue-1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if back.ID != w.solo.ID || back.Email != "solo@example.com" {
		t.Errorf("came back as %+v, want user %d with the email", back, w.solo.ID)
	}
	if due, _ := s.DueForRemoval(ctx, fDue*2); len(due) != 0 {
		t.Errorf("a removal still waits: %+v", due)
	}
	if _, err := s.RemoveForgotten(ctx, w.solo.ID, fDue*2); !errors.Is(err, ErrNotFound) {
		t.Errorf("RemoveForgotten after coming back: %v, want ErrNotFound", err)
	}
	after := w.tenantRows(t, w.solo.TenantID)
	before["sessions"] = 0 // forgetting ended it
	for table, n := range before {
		if after[table] != n {
			t.Errorf("%s: %d rows, want %d", table, after[table], n)
		}
	}
	as, _ := s.VisibleAccounts(ctx, back.Viewer())
	if len(as) != 1 || as[0].ID != w.soloBox {
		t.Fatalf("accounts after coming back = %+v", as)
	}
	// The mailbox stays stopped and asks for its password again.
	if a := as[0]; !a.SecretGone || a.Status != "auth_failed" || a.LastError != forgottenMessage {
		t.Errorf("the mailbox after coming back = %+v", a)
	}
	if _, err := s.AccountSecret(ctx, w.soloBox); !errors.Is(err, ErrNoSecret) {
		t.Errorf("its secret: %v, want ErrNoSecret", err)
	}
	if _, err := s.Setting(ctx, w.solo.TenantID, "key.anthropic_api_key"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the model key came back: %v", err)
	}
	// A new password brings the mailbox back.
	if err := s.SetAccountSecret(ctx, w.soloBox, "new-pw"); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.Account(ctx, w.soloBox); a.SecretGone {
		t.Errorf("the mailbox has no secret after a new one: %+v", a)
	}
	if pw, err := s.AccountSecret(ctx, w.soloBox); err != nil || pw != "new-pw" {
		t.Errorf("secret = %q, %v", pw, err)
	}
}

// A person who deleted their own account never gets it back: the same identity signing in
// again, within the wait or after the removal, gets a new, empty user in a new tenant, and
// the old data is still removed when its time comes. The service's own deletion notice
// afterwards reaches nobody.
func TestDeleteOwnAccountNeverComesBack(t *testing.T) {
	w := forgetWorld(t)
	s, ctx := w.s, t.Context()
	before := w.tenantRows(t, w.solo.TenantID)
	f, err := s.DeleteOwnAccount(ctx, "workos", "user_solo", fNow)
	if err != nil {
		t.Fatal(err)
	}
	if f.UserID != w.solo.ID || len(f.Accounts) != 1 || !f.Keys {
		t.Errorf("delete = %+v", f)
	}
	if _, _, err := s.SessionUser(ctx, "session-solo", fNow); !errors.Is(err, ErrNotFound) {
		t.Errorf("the person's session still works: %v", err)
	}
	if _, err := s.UserByEmail(ctx, "solo@example.com"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the email is still held: %v", err)
	}
	if _, err := s.AccountSecret(ctx, w.soloBox); !errors.Is(err, ErrNoSecret) {
		t.Errorf("the mailbox secret: %v, want ErrNoSecret", err)
	}
	// Deleting again, or the service's notice, reaches nobody and moves nothing.
	for _, forget := range []func() (Forget, error){
		func() (Forget, error) { return s.DeleteOwnAccount(ctx, "workos", "user_solo", fNow+day) },
		func() (Forget, error) { return s.ForgetIdentity(ctx, "workos", "user_solo", fNow+day) },
	} {
		if again, err := forget(); err != nil || again.UserID != 0 {
			t.Errorf("forgetting again = %+v, %v", again, err)
		}
	}
	if due, _ := s.DueForRemoval(ctx, fDue); len(due) != 1 || due[0].UserID != w.solo.ID || due[0].RemoveAt != fDue {
		t.Errorf("due = %+v, want the old user at %d", due, fDue)
	}

	// The same identity, under the same organisation, within the wait.
	fresh, err := s.SignInIdentity(ctx, identity("user_solo", "solo@example.com", "org_solo"), fDue-1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ID == w.solo.ID || fresh.TenantID == w.solo.TenantID {
		t.Fatalf("signing in again got the old user or tenant: %+v, old %+v", fresh, w.solo)
	}
	if as, _ := s.VisibleAccounts(ctx, fresh.Viewer()); len(as) != 0 {
		t.Errorf("the new user sees the old mailboxes: %+v", as)
	}
	if rs, _ := s.Rules(ctx, fresh.TenantID); len(rs) != 0 {
		t.Errorf("the new user sees the old rules: %+v", rs)
	}
	// The removal still waits, and the old data is all there until then.
	if got := w.tenantRows(t, w.solo.TenantID); got["accounts"] != before["accounts"] || got["rules"] != before["rules"] || got["identities"] != 0 || got["forgotten"] != 1 {
		t.Errorf("the old tenant before its removal: %v", got)
	}
	gone, err := s.RemoveForgotten(ctx, w.solo.ID, fDue)
	if err != nil {
		t.Fatal(err)
	}
	if !gone.Tenant || gone.TenantID != w.solo.TenantID {
		t.Errorf("removal = %+v, want the whole old tenant", gone)
	}
	for table, n := range w.tenantRows(t, w.solo.TenantID) {
		if n != 0 {
			t.Errorf("%s: %d rows of the old tenant left", table, n)
		}
	}
	// After the removal the identity is the new user's.
	later, err := s.SignInIdentity(ctx, identity("user_solo", "solo@example.com", "org_solo"), fDue+day, nil)
	if err != nil || later.ID != fresh.ID || later.TenantID != fresh.TenantID {
		t.Errorf("signing in after the removal = %+v, %v; want the new user %+v", later, err, fresh)
	}
}

// A team member who deleted their own account and signs in again joins the team as a new
// user, without their old mailboxes; the team keeps its organisation and its other member.
func TestDeleteOwnAccountTeamMember(t *testing.T) {
	w := forgetWorld(t)
	s, ctx := w.s, t.Context()
	team := w.leaver.TenantID
	if _, err := s.DeleteOwnAccount(ctx, "workos", "user_leaver", fNow); err != nil {
		t.Fatal(err)
	}
	back, err := s.SignInIdentity(ctx, identity("user_leaver", "leaver@example.com", "org_team"), fNow+1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if back.ID == w.leaver.ID || back.TenantID != team {
		t.Errorf("signing in again = %+v, want a new user in the team %d", back, team)
	}
	as, _ := s.VisibleAccounts(ctx, back.Viewer())
	for _, a := range as {
		if a.ID == w.leaverPriv {
			t.Errorf("the new user sees the old private mailbox: %+v", a)
		}
	}
	if mate, err := s.SignInIdentity(ctx, identity("user_mate", "mate@example.com", "org_team"), fNow+2, nil); err != nil || mate.ID != w.mate.ID {
		t.Errorf("the teammate = %+v, %v", mate, err)
	}
	gone, err := s.RemoveForgotten(ctx, w.leaver.ID, fDue)
	if err != nil || gone.Tenant || !slices.Equal(gone.Accounts, []int64{w.leaverPriv, w.leaverShared}) {
		t.Errorf("removal = %+v, %v; want the old user's two mailboxes, the team kept", gone, err)
	}
	if u, err := s.User(ctx, back.ID); err != nil || u.Email != "leaver@example.com" {
		t.Errorf("the new user after the removal = %+v, %v", u, err)
	}
}

// A member removed from their team (MAI-253) who signs in again under another organisation,
// as the sign-in service then puts them in one of their own, gets a new account of their
// own, as a deleted person would: a new tenant they own, which sees nothing of the team.
// The team keeps its data, and the removed user's goes on the team's schedule; its seats
// and members do not count them. The service deleting the person later reaches the new
// account only.
func TestRemovedMemberSignsInElsewhere(t *testing.T) {
	w := forgetWorld(t)
	s, ctx := w.s, t.Context()
	team := w.leaver.TenantID
	before := w.tenantRows(t, team)
	if _, err := s.RemoveMember(as(ctx, w.mate), "workos", "user_leaver", fNow); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.SignInIdentity(ctx, identity("user_leaver", "leaver@example.com", "org_leaver"), fNow+6*3600, nil)
	if err != nil {
		t.Fatalf("the removed member signing in under their own organisation: %v", err)
	}
	if fresh.ID == w.leaver.ID || fresh.TenantID == team || fresh.Role != RoleOwner || fresh.Email != "leaver@example.com" {
		t.Fatalf("signed in as %+v; want a new owner in a new tenant, not user %d of tenant %d", fresh, w.leaver.ID, team)
	}
	if as, _ := s.VisibleAccounts(ctx, fresh.Viewer()); len(as) != 0 {
		t.Errorf("the new account sees mailboxes: %+v", as)
	}
	if rs, _ := s.Rules(ctx, fresh.TenantID); len(rs) != 0 {
		t.Errorf("the new account sees rules: %+v", rs)
	}
	if ms, _ := s.Members(ctx, fresh.TenantID); len(ms) != 1 || ms[0].UserID != fresh.ID {
		t.Errorf("the new tenant's members = %+v", ms)
	}
	if got := audited(t, s, fresh.TenantID); len(got) != 1 {
		t.Errorf("the new tenant's audit log = %q, want its own join alone", got)
	}
	// The team: its data all there until the removal, the removed person counted nowhere.
	if ms, _ := s.Members(ctx, team); len(ms) != 1 || ms[0].UserID != w.mate.ID {
		t.Errorf("the team's members = %+v, want the mate alone", ms)
	}
	if n, err := s.SeatsUsed(ctx, team, fNow+1); err != nil || n != 1 {
		t.Errorf("the team's seats = %d, %v; want 1", n, err)
	}
	got := w.tenantRows(t, team)
	for _, table := range []string{"accounts", "messages", "rules", "sender_rules", "users"} {
		if got[table] != before[table] {
			t.Errorf("team %s: %d rows, want %d until the removal", table, got[table], before[table])
		}
	}
	if got["identities"] != 1 || got["forgotten"] != 1 {
		t.Errorf("team identities %d, forgotten %d; want the mate's identity alone and the removal waiting", got["identities"], got["forgotten"])
	}
	gone, err := s.RemoveForgotten(ctx, w.leaver.ID, fDue)
	if err != nil || gone.Tenant || gone.TenantID != team || !slices.Equal(gone.Accounts, []int64{w.leaverPriv, w.leaverShared}) {
		t.Errorf("removal = %+v, %v; want the removed user's two mailboxes, the team kept", gone, err)
	}
	if got := w.tenantRows(t, fresh.TenantID); got["users"] != 1 || got["identities"] != 1 || got["tenants"] != 1 {
		t.Errorf("the new tenant after the team's removal: %v", got)
	}
	again, err := s.SignInIdentity(ctx, identity("user_leaver", "leaver@example.com", "org_leaver"), fDue+1, nil)
	if err != nil || again.ID != fresh.ID {
		t.Errorf("signing in again = %+v, %v; want the new account %d", again, err, fresh.ID)
	}
	f, err := s.ForgetIdentity(ctx, "workos", "user_leaver", fDue+2)
	if err != nil || f.UserID != fresh.ID || f.TenantID != fresh.TenantID {
		t.Errorf("the service deleting the person reached %+v, %v; want the new account", f, err)
	}
}

// A member removed from their team who signs in under the team's organisation again (the
// sign-in service should not let them) is refused, changing nothing, until the team invites
// them again; then they join as a new user with the invitation's role, without their old
// data, which still goes on the team's schedule. The address MAI-248 freed can be invited.
func TestRemovedMemberUnderTheTeamAgain(t *testing.T) {
	w := forgetWorld(t)
	s, ctx := w.s, t.Context()
	team := w.leaver.TenantID
	if _, err := s.RemoveMember(as(ctx, w.mate), "workos", "user_leaver", fNow); err != nil {
		t.Fatal(err)
	}
	before := w.tenantRows(t, team)
	if before["removed_identities"] != 1 {
		t.Fatalf("removed identities = %d, want the leaver's", before["removed_identities"])
	}
	if _, err := s.SignInIdentity(ctx, identity("user_leaver", "leaver@example.com", "org_team"), fNow+1, seats(NoLimit)); !errors.Is(err, ErrRemovedMember) {
		t.Fatalf("signing in under the team without an invitation: %v, want ErrRemovedMember", err)
	}
	after := w.tenantRows(t, team)
	for table, n := range before {
		if after[table] != n {
			t.Errorf("the refused sign-in changed %s: %d rows, want %d", table, after[table], n)
		}
	}
	if ms, _ := s.Members(ctx, team); len(ms) != 1 {
		t.Errorf("members after the refused sign-in = %+v", ms)
	}
	if _, err := s.CreateInvitation(as(ctx, w.mate), team, Invitation{Email: "leaver@example.com", Role: RoleAdmin, InvitedBy: w.mate.ID}, nil, fNow+2); err != nil {
		t.Fatalf("inviting the removed member again: %v", err)
	}
	back, err := s.SignInIdentity(ctx, identity("user_leaver", "leaver@example.com", "org_team"), fNow+3, seats(2))
	if err != nil {
		t.Fatalf("signing in with the invitation: %v", err)
	}
	if back.ID == w.leaver.ID || back.TenantID != team || back.Role != RoleAdmin {
		t.Fatalf("joined again as %+v; want a new admin of the team, not user %d", back, w.leaver.ID)
	}
	// The old user's shared mailbox is the team's, as every member sees it, until it goes.
	visible, _ := s.VisibleAccounts(ctx, back.Viewer())
	for _, a := range visible {
		if a.ID == w.leaverPriv {
			t.Errorf("the new user sees the old user's private mailbox %+v", a)
		}
	}
	if n, _ := s.SeatsUsed(ctx, team, fNow+3); n != 2 {
		t.Errorf("seats = %d, want the mate and the new user, the invitation used up", n)
	}
	if got := w.tenantRows(t, team); got["removed_identities"] != 0 || got["identities"] != 2 {
		t.Errorf("after joining again: %v", got)
	}
	gone, err := s.RemoveForgotten(ctx, w.leaver.ID, fDue)
	if err != nil || gone.Tenant || !slices.Equal(gone.Accounts, []int64{w.leaverPriv, w.leaverShared}) {
		t.Errorf("the old user's removal = %+v, %v", gone, err)
	}
	if u, err := s.User(ctx, back.ID); err != nil || u.Email != "leaver@example.com" {
		t.Errorf("the new user after the old one's removal = %+v, %v", u, err)
	}
	// Removed again, the removal is remembered again.
	if _, err := s.RemoveMember(as(ctx, w.mate), "workos", "user_leaver", fDue+1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SignInIdentity(ctx, identity("user_leaver", "leaver@example.com", "org_team"), fDue+2, nil); !errors.Is(err, ErrRemovedMember) {
		t.Errorf("signing in after a second removal: %v, want ErrRemovedMember", err)
	}
}

// MAI-254: the last real member of a team forgotten while another forgotten member's
// removal still waits is the team's only member: the team's model keys and notification
// addresses go at once and no seat check is queued, as for a team of one, and when they
// deleted their own account the team lets go of its organisation, so a newcomer under it
// does not join the old data. The removals still keep their order: the first forgotten
// takes their mailboxes, the last the tenant.
func TestLastRealMemberForgottenWhileAnotherWaits(t *testing.T) {
	for _, own := range []bool{false, true} {
		t.Run(map[bool]string{false: "deleted by the service", true: "deleted their own account"}[own], func(t *testing.T) {
			w := forgetWorld(t)
			s, ctx := w.s, t.Context()
			team := w.leaver.TenantID
			if _, _, err := s.SetPlan(ctx, team, Plan{Plan: PlanTeam, Interval: IntervalMonth, Status: StatusActive, Record: "sub", Seats: 3}, 10); err != nil {
				t.Fatal(err)
			}
			if f, err := s.ForgetIdentity(ctx, "workos", "user_leaver", fNow); err != nil || f.Keys {
				t.Fatalf("forget the leaver = %+v, %v; want the team's keys kept for the mate", f, err)
			}
			checks, err := s.WaitingSeatChecks(ctx, 1<<40, 10)
			if err != nil || len(checks) != 1 {
				t.Fatalf("seat checks after the leaver = %+v, %v", checks, err)
			}
			all := []int64{w.mateBox, w.leaverPriv, w.leaverShared}
			if ids, err := s.IdentityMailboxes(ctx, "workos", "user_mate"); err != nil || !slices.Equal(ids, all) {
				t.Errorf("IdentityMailboxes of the last member = %v, %v; want every mailbox of the team", ids, err)
			}
			forget := s.ForgetIdentity
			if own {
				forget = s.DeleteOwnAccount
			}
			f, err := forget(ctx, "workos", "user_mate", fNow+day)
			if err != nil {
				t.Fatal(err)
			}
			if !f.Keys || !slices.Equal(f.Accounts, all) {
				t.Errorf("forget the last member = %+v; want every mailbox of the team and its keys", f)
			}
			if _, err := s.Setting(ctx, team, "key.anthropic_api_key"); !errors.Is(err, ErrNotFound) {
				t.Errorf("the team's model key: %v, want it gone", err)
			}
			if ds, _ := s.Destinations(ctx, team); len(ds) == 0 || !ds[0].AddressDeleted {
				t.Errorf("the team's destinations = %+v, want their addresses gone", ds)
			}
			if cs, _ := s.WaitingSeatChecks(ctx, 1<<40, 10); len(cs) != 1 || cs[0].Version != checks[0].Version {
				t.Errorf("seat checks after the last member = %+v, want only the leaver's %+v", cs, checks)
			}
			if own {
				newcomer, err := s.SignInIdentity(ctx, identity("user_new", "new@example.com", "org_team"), fNow+2*day, nil)
				if err != nil || newcomer.TenantID == team {
					t.Errorf("a newcomer under the team's organisation = %+v, %v; want a new tenant", newcomer, err)
				}
			}
			first, err := s.RemoveForgotten(ctx, w.leaver.ID, fDue)
			if err != nil || first.Tenant || !slices.Equal(first.Accounts, []int64{w.leaverPriv, w.leaverShared}) {
				t.Errorf("the leaver's removal = %+v, %v; want their mailboxes, the tenant kept for the mate", first, err)
			}
			last, err := s.RemoveForgotten(ctx, w.mate.ID, fDue+day)
			if err != nil || !last.Tenant {
				t.Errorf("the mate's removal = %+v, %v; want the whole tenant", last, err)
			}
			for table, n := range w.tenantRows(t, team) {
				if n != 0 {
					t.Errorf("%s: %d rows of the removed team left", table, n)
				}
			}
		})
	}
}

// Forgetting deletes the person's secrets at once: their mailboxes' passwords (all of
// them when they are their tenant's only member, else every one they own, shared or not)
// and, for an only member, the tenant's model keys. Nobody else's are touched.
func TestForgetIdentityDeletesSecrets(t *testing.T) {
	w := forgetWorld(t)
	s, ctx := w.s, t.Context()
	// The leaver's shared mailbox is a one-click one, and their private one a module's
	// sign-in form added with a password: both land in reconnect_needed.
	if _, err := w.db.ExecContext(ctx, `UPDATE accounts SET oauth = TRUE WHERE id = ?`, w.leaverShared); err != nil {
		t.Fatal(err)
	}
	if _, err := w.db.ExecContext(ctx, `UPDATE accounts SET module = TRUE WHERE id = ?`, w.leaverPriv); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		sub      string
		tenant   int64
		accounts []int64
		keys     bool
	}{
		{"user_solo", w.solo.TenantID, []int64{w.soloBox}, true},
		{"user_leaver", w.leaver.TenantID, []int64{w.leaverPriv, w.leaverShared}, false},
	} {
		f, err := s.ForgetIdentity(ctx, "workos", c.sub, fNow)
		if err != nil {
			t.Fatal(err)
		}
		if f.TenantID != c.tenant || f.Keys != c.keys || !slices.Equal(f.Accounts, c.accounts) {
			t.Errorf("forget %s = %+v, want accounts %v keys %v", c.sub, f, c.accounts, c.keys)
		}
		_, err = s.Setting(ctx, c.tenant, "key.anthropic_api_key")
		if gone := errors.Is(err, ErrNotFound); gone != c.keys {
			t.Errorf("%s: model key gone = %v (%v), want %v", c.sub, gone, err, c.keys)
		}
		ds, err := s.Destinations(ctx, c.tenant)
		if err != nil || len(ds) == 0 {
			t.Fatalf("%s: destinations = %v, %v", c.sub, ds, err)
		}
		for _, d := range ds {
			_, err := s.DestinationSecret(ctx, d.ID)
			if gone := errors.Is(err, ErrNoSecret); gone != c.keys || d.AddressDeleted != c.keys {
				t.Errorf("%s: destination %+v address gone = %v (%v), want %v", c.sub, d, gone, err, c.keys)
			}
		}
		if _, err := s.Setting(ctx, c.tenant, "seed_"+strings.TrimPrefix(c.sub, "user_")+map[bool]string{true: "", false: "priv"}[c.keys]); err != nil {
			t.Errorf("%s: an ordinary setting went: %v", c.sub, err)
		}
	}
	for _, c := range []struct {
		id          int64
		status, msg string
	}{
		{w.soloBox, "auth_failed", forgottenMessage},
		{w.leaverPriv, "reconnect_needed", forgottenOAuthMessage},
		{w.leaverShared, "reconnect_needed", forgottenOAuthMessage},
	} {
		id := c.id
		a, err := s.Account(ctx, id)
		if err != nil || !a.SecretGone || a.Status != c.status || a.LastError != c.msg {
			t.Errorf("mailbox %d = %+v, %v; want its secret gone and %s", id, a, err, c.status)
		}
		if _, err := s.AccountSecret(ctx, id); !errors.Is(err, ErrNoSecret) {
			t.Errorf("mailbox %d secret: %v, want ErrNoSecret", id, err)
		}
	}
	for _, id := range []int64{w.adminBox, w.mateBox, w.otherBox} {
		if a, err := s.Account(ctx, id); err != nil || a.SecretGone || a.Status != "new" {
			t.Errorf("someone else's mailbox %d = %+v, %v", id, a, err)
		}
		if _, err := s.AccountSecret(ctx, id); err != nil {
			t.Errorf("someone else's secret %d: %v", id, err)
		}
	}
	if _, err := s.Setting(ctx, w.other.TenantID, "key.anthropic_api_key"); err != nil {
		t.Errorf("another tenant's model key: %v", err)
	}
	if ds, _ := s.Destinations(ctx, w.other.TenantID); len(ds) != 1 || ds[0].AddressDeleted {
		t.Errorf("another tenant's destinations = %+v", ds)
	} else if sec, err := s.DestinationSecret(ctx, ds[0].ID); err != nil || sec.URL != "https://hooks.test/other" || sec.Secret != "whsec_other" {
		t.Errorf("another tenant's destination secret = %+v, %v", sec, err)
	}
	ids, err := s.IdentityMailboxes(ctx, "workos", "user_leaver")
	if err != nil || !slices.Equal(ids, []int64{w.leaverPriv, w.leaverShared}) {
		t.Errorf("IdentityMailboxes = %v, %v", ids, err)
	}
	if ids, err := s.IdentityMailboxes(ctx, "workos", "nobody"); err != nil || len(ids) != 0 {
		t.Errorf("IdentityMailboxes of nobody = %v, %v", ids, err)
	}
}

// After the wait, a tenant's only member takes the whole tenant; every other tenant keeps
// every row.
func TestRemoveForgottenOnlyMember(t *testing.T) {
	w := forgetWorld(t)
	s, ctx := w.s, t.Context()
	others := map[int64]map[string]int{}
	for _, tenant := range []int64{SelfHostTenant, w.leaver.TenantID, w.other.TenantID} {
		others[tenant] = w.tenantRows(t, tenant)
	}
	if _, err := s.ForgetIdentity(ctx, "workos", "user_solo", fNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveForgotten(ctx, w.solo.ID, fDue-1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removal before its time: %v, want ErrNotFound", err)
	}
	ids, err := s.RemovalAccounts(ctx, w.solo.ID)
	if err != nil || len(ids) != 1 || ids[0] != w.soloBox {
		t.Fatalf("RemovalAccounts = %v, %v", ids, err)
	}
	gone, err := s.RemoveForgotten(ctx, w.solo.ID, fDue)
	if err != nil {
		t.Fatal(err)
	}
	if !gone.Tenant || gone.TenantID != w.solo.TenantID || len(gone.Accounts) != 1 || gone.Accounts[0] != w.soloBox {
		t.Errorf("removal = %+v", gone)
	}
	for table, n := range w.tenantRows(t, w.solo.TenantID) {
		if n != 0 {
			t.Errorf("%s: %d rows of the removed tenant left", table, n)
		}
	}
	for tenant, want := range others {
		got := w.tenantRows(t, tenant)
		for table, n := range want {
			if got[table] != n {
				t.Errorf("tenant %d %s: %d rows, want %d", tenant, table, got[table], n)
			}
		}
	}
	if _, err := s.RemoveForgotten(ctx, w.solo.ID, fDue); !errors.Is(err, ErrNotFound) {
		t.Errorf("a second removal: %v, want ErrNotFound", err)
	}
	if _, err := s.RemovalAccounts(ctx, w.solo.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("RemovalAccounts of a removed user: %v, want ErrNotFound", err)
	}
}

// A member leaving a team takes all their mailboxes, the shared one too; the team keeps
// its tenant, its model keys and its rules (the ones naming a removed mailbox rewritten),
// now authored by the teammate.
func TestRemoveForgottenTeamMember(t *testing.T) {
	w := forgetWorld(t)
	s, ctx := w.s, t.Context()
	team := w.leaver.TenantID
	before := w.tenantRows(t, team)
	if _, err := s.ForgetIdentity(ctx, "workos", "user_leaver", fNow); err != nil {
		t.Fatal(err)
	}
	gone, err := s.RemoveForgotten(ctx, w.leaver.ID, fDue)
	if err != nil {
		t.Fatal(err)
	}
	if gone.Tenant || !slices.Equal(gone.Accounts, []int64{w.leaverPriv, w.leaverShared}) {
		t.Errorf("removal = %+v, want both of the leaver's mailboxes", gone)
	}
	if len(gone.Rules) != 3 {
		t.Errorf("rules rewritten = %+v, want the three naming one of them", gone.Rules)
	}
	if _, err := s.User(ctx, w.leaver.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("the leaver's user row: %v", err)
	}
	for _, id := range []int64{w.leaverPriv, w.leaverShared} {
		if _, err := s.Account(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("the leaver's mailbox %d: %v", id, err)
		}
	}
	scoped, err := s.Rule(ctx, team, w.scopedRule.ID)
	if err != nil || !scoped.MailboxRemoved || scoped.Enabled || scoped.UserID != w.mate.ID {
		t.Errorf("the rule naming the private mailbox = %+v, %v", scoped, err)
	}
	srs, err := s.SenderRules(ctx, team)
	if err != nil {
		t.Fatal(err)
	}
	for _, sr := range srs {
		if sr.UserID != w.mate.ID {
			t.Errorf("sender rule %+v not handed over", sr)
		}
	}
	got := w.tenantRows(t, team)
	for table, want := range map[string]int{
		"tenants": 1, "users": 1, "identities": 1, "sessions": 1, "summaries": 1, "forgotten": 0,
		"accounts": 1, "messages": 1, "settings": before["settings"], "usage_daily": before["usage_daily"], "ai_emails": before["ai_emails"],
		"first_cleanup": before["first_cleanup"],
		"batches":       before["batches"], "rules": before["rules"],
		// The team's destinations and their log stay; the notices about the leaver's mail go.
		"notify_destinations": before["notify_destinations"], "notify_deliveries": before["notify_deliveries"],
		"notices": before["notices"] - 2,
	} {
		if got[table] != want {
			t.Errorf("team %s: %d rows, want %d", table, got[table], want)
		}
	}
	if u, err := s.User(ctx, w.mate.ID); err != nil || u.Email != "mate@example.com" {
		t.Errorf("teammate = %+v, %v", u, err)
	}
}
