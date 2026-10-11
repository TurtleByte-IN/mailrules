package store

import (
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"
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
	as, _ := s.VisibleAccounts(ctx, back.Viewer())
	if len(as) != 1 || as[0].ID != w.soloBox {
		t.Fatalf("accounts after coming back = %+v", as)
	}
	// The mailbox stays stopped and asks for its password again.
	if a := as[0]; !a.SecretGone || a.Status != "auth_failed" || a.LastError != forgottenMessage {
		t.Errorf("the mailbox after coming back = %+v", a)
	}
	if _, err := s.AccountSecret(ctx, testMaster, w.soloBox); !errors.Is(err, ErrNoSecret) {
		t.Errorf("its secret: %v, want ErrNoSecret", err)
	}
	if _, err := s.Setting(ctx, w.solo.TenantID, "key.anthropic_api_key"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the model key came back: %v", err)
	}
	// A new password brings the mailbox back.
	if err := s.SetAccountSecret(ctx, testMaster, w.soloBox, "new-pw"); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.Account(ctx, w.soloBox); a.SecretGone {
		t.Errorf("the mailbox has no secret after a new one: %+v", a)
	}
	if pw, err := s.AccountSecret(ctx, testMaster, w.soloBox); err != nil || pw != "new-pw" {
		t.Errorf("secret = %q, %v", pw, err)
	}
}

// Forgetting deletes the person's secrets at once: their mailboxes' passwords (all of
// them when they are their tenant's only member, else every one they own, shared or not)
// and, for an only member, the tenant's model keys. Nobody else's are touched.
func TestForgetIdentityDeletesSecrets(t *testing.T) {
	w := forgetWorld(t)
	s, ctx := w.s, t.Context()
	// The leaver's shared mailbox is a one-click one: it lands in reconnect_needed.
	if _, err := w.db.ExecContext(ctx, `UPDATE accounts SET oauth = 1 WHERE id = ?`, w.leaverShared); err != nil {
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
		if _, err := s.Setting(ctx, c.tenant, "seed_"+strings.TrimPrefix(c.sub, "user_")+map[bool]string{true: "", false: "priv"}[c.keys]); err != nil {
			t.Errorf("%s: an ordinary setting went: %v", c.sub, err)
		}
	}
	for _, c := range []struct {
		id          int64
		status, msg string
	}{
		{w.soloBox, "auth_failed", forgottenMessage},
		{w.leaverPriv, "auth_failed", forgottenMessage},
		{w.leaverShared, "reconnect_needed", forgottenOAuthMessage},
	} {
		id := c.id
		a, err := s.Account(ctx, id)
		if err != nil || !a.SecretGone || a.Status != c.status || a.LastError != c.msg {
			t.Errorf("mailbox %d = %+v, %v; want its secret gone and %s", id, a, err, c.status)
		}
		if _, err := s.AccountSecret(ctx, testMaster, id); !errors.Is(err, ErrNoSecret) {
			t.Errorf("mailbox %d secret: %v, want ErrNoSecret", id, err)
		}
	}
	for _, id := range []int64{w.adminBox, w.mateBox, w.otherBox} {
		if a, err := s.Account(ctx, id); err != nil || a.SecretGone || a.Status != "new" {
			t.Errorf("someone else's mailbox %d = %+v, %v", id, a, err)
		}
		if _, err := s.AccountSecret(ctx, testMaster, id); err != nil {
			t.Errorf("someone else's secret %d: %v", id, err)
		}
	}
	if _, err := s.Setting(ctx, w.other.TenantID, "key.anthropic_api_key"); err != nil {
		t.Errorf("another tenant's model key: %v", err)
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
		"accounts": 1, "messages": 1, "settings": before["settings"], "usage_daily": before["usage_daily"],
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
