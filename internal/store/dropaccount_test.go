package store

import (
	"encoding/json"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/rules"
)

func treeJSON(t *testing.T, c rules.Cond) string {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustRule(t *testing.T, s *Store, r rules.Rule) rules.Rule {
	t.Helper()
	if r.Actions == nil {
		r.Actions = []rules.Action{{Type: rules.ActKeep}}
	}
	saved, err := s.CreateRule(t.Context(), SelfHostTenant, r, 100)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

// Removing a mailbox keeps the tenant's rules that name it, rewritten, in the transaction
// that deletes it: a failed delete leaves the rules as they were. Rules are tenant-owned,
// so a teammate's rule naming the mailbox is rewritten too.
func TestDeleteAccountKeepsRules(t *testing.T) {
	s, db := open(t)
	ctx := t.Context()
	x := newAccount(t, s, "x@icloud.com", "pw")
	y := newAccount(t, s, "y@icloud.com", "pw")
	res, err := db.ExecContext(ctx, `INSERT INTO users (email, password_hash, created_at) VALUES ('other@example.com', 'hash', 1)`)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := res.LastInsertId()
	z, err := s.CreateAccount(ctx, testMaster, Account{UserID: other, Label: "Z", Preset: "icloud", Host: "imap.mail.me.com", Port: 993,
		TLSMode: "implicit", Username: "z@icloud.com", CreatedAt: 1}, "pw")
	if err != nil {
		t.Fatal(err)
	}
	id := func(a Account) float64 { return float64(a.ID) }

	scoped := mustRule(t, s, rules.Rule{UserID: x.UserID, AccountID: x.ID, Name: "Scoped", Intent: "x", Enabled: true})
	either := mustRule(t, s, rules.Rule{UserID: x.UserID, Name: "Either", Enabled: true, Conditions: rules.Cond{Any: []rules.Cond{
		{Field: "account", Op: rules.OpEq, Value: id(x)}, {Field: "from_domain", Op: rules.OpEq, Value: "a.com"}}}})
	unrelated := mustRule(t, s, rules.Rule{UserID: x.UserID, AccountID: y.ID, Name: "Unrelated", Enabled: true,
		Conditions: rules.Cond{Field: "account", Op: rules.OpEq, Value: id(y)}})
	theirs := mustRule(t, s, rules.Rule{UserID: other, Name: "Theirs", Intent: "t", Enabled: true,
		Conditions: rules.Cond{Field: "account", Op: rules.OpIn, Value: []any{id(x), id(z)}}})

	// A delete that fails after the rules were rewritten leaves them as they were.
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER refuse BEFORE DELETE ON accounts BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteAccount(ctx, x.ID, 500); err == nil {
		t.Fatal("DeleteAccount went through the refusing trigger")
	}
	if got, _ := s.Rule(ctx, SelfHostTenant, scoped.ID); got.AccountID != x.ID || !got.Enabled || got.MailboxRemoved || got.Version != 1 {
		t.Errorf("after a failed delete the scoped rule = %+v", got)
	}
	if got, _ := s.Rule(ctx, SelfHostTenant, theirs.ID); treeJSON(t, got.Conditions) != treeJSON(t, theirs.Conditions) {
		t.Errorf("after a failed delete their rule = %s", treeJSON(t, got.Conditions))
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER refuse`); err != nil {
		t.Fatal(err)
	}

	changed, err := s.DeleteAccount(ctx, x.ID, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 3 {
		t.Errorf("changed %d rules, want 3: %+v", len(changed), changed)
	}
	tests := []struct {
		name               string
		user, id           int64
		account            int64
		on, marked         bool
		conditions, unless rules.Cond
		version            int
	}{
		{"the rule for the mailbox is kept off, marked, for no mailbox", x.UserID, scoped.ID, 0, false, true, rules.Cond{}, rules.Cond{}, 2},
		{"a condition rule drops the leaf and stays on", x.UserID, either.ID, 0, true, false,
			rules.Cond{Any: []rules.Cond{{Field: "from_domain", Op: rules.OpEq, Value: "a.com"}}}, rules.Cond{}, 2},
		{"a rule naming another mailbox is untouched", x.UserID, unrelated.ID, y.ID, true, false, unrelated.Conditions, rules.Cond{}, 1},
		{"another user's rule is rewritten too", other, theirs.ID, 0, true, false,
			rules.Cond{Field: "account", Op: rules.OpIn, Value: []any{id(z)}}, rules.Cond{}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.Rule(ctx, SelfHostTenant, tt.id)
			if err != nil {
				t.Fatal(err)
			}
			if got.AccountID != tt.account || got.Enabled != tt.on || got.MailboxRemoved != tt.marked || got.Version != tt.version ||
				treeJSON(t, got.Conditions) != treeJSON(t, tt.conditions) || treeJSON(t, got.Exceptions) != treeJSON(t, tt.unless) {
				t.Errorf("got %+v", got)
			}
			if tt.version == 2 && got.UpdatedAt != 500 {
				t.Errorf("updated_at = %d, want 500", got.UpdatedAt)
			}
		})
	}
}

// Rules still naming a mailbox removed before DeleteAccount kept them are fixed once.
func TestReconcileRemovedMailboxes(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	a := newAccount(t, s, "a@icloud.com", "pw")
	const gone = 99.0 // no such account
	subj := rules.Cond{Field: "subject", Op: rules.OpContains, Value: "y"}
	either := mustRule(t, s, rules.Rule{UserID: a.UserID, Name: "Either", Intent: "e", Enabled: true,
		Conditions: rules.Cond{Any: []rules.Cond{{Field: "account", Op: rules.OpEq, Value: gone}, subj}},
		Exceptions: rules.Cond{Field: "account", Op: rules.OpEq, Value: float64(a.ID)}})
	only := mustRule(t, s, rules.Rule{UserID: a.UserID, Name: "Only", Enabled: true,
		Conditions: rules.Cond{All: []rules.Cond{{Field: "account", Op: rules.OpEq, Value: gone}, subj}}})
	fine := mustRule(t, s, rules.Rule{UserID: a.UserID, Name: "Fine", Enabled: true,
		Conditions: rules.Cond{Field: "account", Op: rules.OpIn, Value: []any{float64(a.ID)}}})

	changed, err := s.ReconcileRemovedMailboxes(ctx, 700)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 2 {
		t.Fatalf("changed %d rules, want 2: %+v", len(changed), changed)
	}
	got, _ := s.Rule(ctx, SelfHostTenant, either.ID)
	if !got.Enabled || got.MailboxRemoved || got.Version != 2 || treeJSON(t, got.Conditions) != treeJSON(t, rules.Cond{Any: []rules.Cond{subj}}) ||
		treeJSON(t, got.Exceptions) != treeJSON(t, either.Exceptions) {
		t.Errorf("either = %+v", got)
	}
	got, _ = s.Rule(ctx, SelfHostTenant, only.ID)
	if got.Enabled || !got.MailboxRemoved || got.Version != 2 || treeJSON(t, got.Conditions) != treeJSON(t, rules.Cond{All: []rules.Cond{subj}}) {
		t.Errorf("only = %+v", got)
	}
	if got, _ = s.Rule(ctx, SelfHostTenant, fine.ID); got.Version != 1 {
		t.Errorf("a rule naming a connected mailbox was changed: %+v", got)
	}

	again, err := s.ReconcileRemovedMailboxes(ctx, 800)
	if err != nil || len(again) != 0 {
		t.Errorf("a second run changed %+v, %v", again, err)
	}
	if got, _ = s.Rule(ctx, SelfHostTenant, only.ID); got.Version != 2 || got.UpdatedAt != 700 {
		t.Errorf("after a second run only = %+v", got)
	}
}
