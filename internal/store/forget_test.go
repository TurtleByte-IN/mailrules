package store

import (
	"database/sql"
	"errors"
	"strconv"
	"testing"

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
		u, err := s.SignInIdentity(ctx, identity(sub, email, org), at)
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
	w.otherBox = w.seed(t, w.other, "other")
	if w.scopedRule, err = s.CreateRule(ctx, w.leaver.TenantID, rules.Rule{UserID: w.leaver.ID, AccountID: w.leaverPriv,
		Name: "Leaver only", Intent: "x", Enabled: true, Actions: []rules.Action{{Type: rules.ActKeep}}}, 100); err != nil {
		t.Fatal(err)
	}
	return w
}

// seed gives the user a mailbox and, in their tenant, one of everything: a folder, a
// contact, an email decided, acted on and corrected, a rule for the mailbox and one for
// every mailbox, a sender rule, a batch, a setting, a usage row, a session and a summary.
// It returns the mailbox.
func (w fworld) seed(t *testing.T, u User, name string) int64 {
	t.Helper()
	s, ctx := w.s, t.Context()
	a, err := s.CreateAccount(ctx, testMaster, Account{UserID: u.ID, Label: name, Preset: "generic",
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
	must(s.CreateSession(ctx, "session-"+name, u.ID, 1000, fDue*2))
	_, _, _, err = s.ClaimSummary(ctx, u.ID, 2000, 2000, 3)
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
		"tenants":      `id = ?1`,
		"users":        `tenant_id = ?1`,
		"sessions":     `user_id IN ` + users,
		"identities":   `user_id IN ` + users,
		"summaries":    `user_id IN ` + users,
		"forgotten":    `user_id IN ` + users,
		"accounts":     `tenant_id = ?1`,
		"folders":      `account_id IN ` + accounts,
		"contacts":     `account_id IN ` + accounts,
		"messages":     `account_id IN ` + accounts,
		"actions":      `account_id IN ` + accounts,
		"decisions":    `message_id IN ` + messages,
		"corrections":  `message_id IN ` + messages,
		"rules":        `tenant_id = ?1`,
		"sender_rules": `tenant_id = ?1`,
		"batches":      `tenant_id = ?1`,
		"settings":     `tenant_id = ?1`,
		"usage_daily":  `tenant_id = ?1`,
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
	if err := s.ForgetIdentity(ctx, "workos", "user_solo", fNow); err != nil {
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
	fresh, err := s.SignInIdentity(ctx, identity("user_solo_2", "solo@example.com", "org_solo_2"), fNow+1)
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
		if err := s.ForgetIdentity(ctx, "workos", "user_solo", at); err != nil {
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
		if err := s.ForgetIdentity(ctx, id[0], id[1], fNow); err != nil {
			t.Errorf("forget %v: %v", id, err)
		}
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
	if err := s.ForgetIdentity(ctx, "workos", "user_solo", fNow); err != nil {
		t.Fatal(err)
	}
	back, err := s.SignInIdentity(ctx, identity("user_solo", "solo@example.com", "org_solo"), fDue-1)
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
	if as, _ := s.VisibleAccounts(ctx, back.Viewer()); len(as) != 1 || as[0].ID != w.soloBox {
		t.Errorf("accounts after coming back = %+v", as)
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
	if err := s.ForgetIdentity(ctx, "workos", "user_solo", fNow); err != nil {
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

// A member leaving a team takes their private mailbox; the team keeps its tenant, its
// rules (the one naming the private mailbox rewritten), and the leaver's shared mailbox,
// now the teammate's.
func TestRemoveForgottenTeamMember(t *testing.T) {
	w := forgetWorld(t)
	s, ctx := w.s, t.Context()
	team := w.leaver.TenantID
	before := w.tenantRows(t, team)
	if err := s.ForgetIdentity(ctx, "workos", "user_leaver", fNow); err != nil {
		t.Fatal(err)
	}
	gone, err := s.RemoveForgotten(ctx, w.leaver.ID, fDue)
	if err != nil {
		t.Fatal(err)
	}
	if gone.Tenant || len(gone.Accounts) != 1 || gone.Accounts[0] != w.leaverPriv {
		t.Errorf("removal = %+v, want only the private mailbox", gone)
	}
	if len(gone.Rules) != 2 {
		t.Errorf("rules rewritten = %+v, want the two naming the private mailbox", gone.Rules)
	}
	if _, err := s.User(ctx, w.leaver.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("the leaver's user row: %v", err)
	}
	if _, err := s.Account(ctx, w.leaverPriv); !errors.Is(err, ErrNotFound) {
		t.Errorf("the private mailbox: %v", err)
	}
	shared, err := s.Account(ctx, w.leaverShared)
	if err != nil || shared.UserID != w.mate.ID || !shared.Shared {
		t.Errorf("the shared mailbox = %+v, %v; want it shared and the teammate's", shared, err)
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
		"accounts": 2, "messages": 2, "settings": before["settings"], "usage_daily": before["usage_daily"],
		"batches": before["batches"], "rules": before["rules"],
	} {
		if got[table] != want {
			t.Errorf("team %s: %d rows, want %d", table, got[table], want)
		}
	}
	if u, err := s.User(ctx, w.mate.ID); err != nil || u.Email != "mate@example.com" {
		t.Errorf("teammate = %+v, %v", u, err)
	}
}
