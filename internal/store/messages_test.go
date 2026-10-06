package store

import (
	"errors"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
)

func TestMessagesAndDecisions(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	a := newAccount(t, s, "me@icloud.com", "pw")
	ref := mail.MsgRef{AccountID: a.ID, Folder: "INBOX", UIDValidity: 9, UID: 4}

	m, fresh, err := s.IngestMessage(ctx, ref, 100)
	if err != nil || !fresh || m.ID == 0 || m.State != StateNew || m.Location() != ref {
		t.Fatalf("ingest = %+v, %v, %v", m, fresh, err)
	}
	if again, fresh, err := s.IngestMessage(ctx, ref, 200); err != nil || fresh || again.ID != m.ID || again.CreatedAt != 100 {
		t.Fatalf("second ingest = %+v, %v, %v", again, fresh, err)
	}
	m.MessageID, m.FromAddr, m.ToAddrs, m.Subject, m.Signals = "a@b", "x@y.example", []string{"me@icloud.com"}, "Hi", Signals{Bulk: true, DMARC: "pass"}
	if err := s.SaveMessageSummary(ctx, m); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Message(ctx, m.ID); err != nil || got.FromAddr != "x@y.example" || got.ToAddrs[0] != "me@icloud.com" || !got.Signals.Bulk || got.Signals.DMARC != "pass" {
		t.Fatalf("message = %+v, %v", got, err)
	}
	if _, err := s.Message(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing message: %v", err)
	}

	// The retry queue: only failed messages whose time has come.
	if err := s.SetMessageState(ctx, m.ID, StateError, 2, 500); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.DueMessages(ctx, a.ID, 499); len(due) != 0 {
		t.Errorf("due too early: %+v", due)
	}
	if due, err := s.DueMessages(ctx, a.ID, 500); err != nil || len(due) != 1 || due[0].Attempts != 2 || due[0].NextAttemptAt != 500 {
		t.Errorf("due = %+v, %v", due, err)
	}

	u, _ := s.FirstUser(ctx)
	rule, err := s.CreateRule(ctx, rules.Rule{UserID: u.ID, Name: "Food", Intent: "food", Actions: []rules.Action{{Type: rules.ActKeep}}, Enabled: true}, 1)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.AddDecision(ctx, Decision{MessageID: m.ID, Stage: "decider", RuleID: rule.ID, RuleVersion: 1, Confidence: 0.9, Reason: "r", Model: "jev", TokensIn: 7, CreatedAt: 300}, StateDecided)
	if err != nil {
		t.Fatal(err)
	}
	row, err := s.ActivityFor(ctx, m.ID)
	if err != nil || row.Decision == nil || row.Decision.ID != id || row.Decision.RuleID != rule.ID || row.Message.State != StateDecided || row.Message.NextAttemptAt != 0 {
		t.Fatalf("activity row = %+v / %+v, %v", row.Message, row.Decision, err)
	}
	if _, err := s.ActivityFor(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing activity row: %v", err)
	}
	for name, f := range map[string]ActivityFilter{
		"account": {AccountID: a.ID}, "rule": {RuleID: rule.ID}, "stage": {Stage: "decider"}, "state": {State: StateDecided}, "cursor": {Before: m.ID + 1},
	} {
		if rows, err := s.Activity(ctx, f); err != nil || len(rows) != 1 {
			t.Errorf("filter by %s: %d rows, %v", name, len(rows), err)
		}
	}
	for name, f := range map[string]ActivityFilter{
		"account": {AccountID: a.ID + 1}, "rule": {RuleID: rule.ID + 1}, "stage": {Stage: "sender"}, "state": {State: StateReview}, "cursor": {Before: m.ID},
	} {
		if rows, err := s.Activity(ctx, f); err != nil || len(rows) != 0 {
			t.Errorf("filter by another %s: %d rows, %v", name, len(rows), err)
		}
	}

	// The folder position only moves forward, except across a UIDVALIDITY change.
	for _, step := range []struct{ validity, uid, want uint32 }{{9, 4, 4}, {9, 2, 4}, {9, 6, 6}, {10, 1, 1}} {
		if err := s.AdvanceFolder(ctx, mail.MsgRef{AccountID: a.ID, Folder: "INBOX", UIDValidity: step.validity, UID: step.uid}); err != nil {
			t.Fatal(err)
		}
		if f, _ := s.Folder(ctx, a.ID, "INBOX"); f.LastUID != step.want || f.UIDValidity != step.validity {
			t.Errorf("after uid %d (validity %d): %+v, want last_uid %d", step.uid, step.validity, f, step.want)
		}
	}

	// History outlives its rule; nothing outlives its account.
	if _, err := s.AddCorrection(ctx, u.ID, Correction{MessageID: m.ID, WrongRuleID: rule.ID, Example: "{}", CreatedAt: 400}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddCorrection(ctx, u.ID, Correction{MessageID: 999, Example: "{}"}, false); !errors.Is(err, ErrNotFound) {
		t.Errorf("correcting a missing message: %v", err)
	}
	if err := s.DeleteRule(ctx, u.ID, rule.ID); err != nil {
		t.Fatalf("deleting a rule that has decisions and corrections: %v", err)
	}
	if row, _ := s.ActivityFor(ctx, m.ID); row.Decision == nil || row.Decision.RuleID != 0 || row.Decision.RuleVersion != 1 {
		t.Errorf("decision after its rule was deleted = %+v", row.Decision)
	}
	scoped, err := s.CreateRule(ctx, rules.Rule{UserID: u.ID, AccountID: a.ID, Name: "Scoped", Intent: "x", Actions: []rules.Action{{Type: rules.ActKeep}}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAccount(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rule(ctx, u.ID, scoped.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a rule scoped to a deleted account is still there: %v", err)
	}
	var left int
	if err := s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM messages) + (SELECT COUNT(*) FROM decisions) + (SELECT COUNT(*) FROM corrections)`).Scan(&left); err != nil || left != 0 {
		t.Errorf("%d rows left behind, %v", left, err)
	}
}
