package store

import (
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
)

// A draft job is queued with the action that ends a rule's live run, waits until it is due,
// is tried again later when asked, and is gone once done; what it answers to (the rule's
// run in effect, its draft action) reads back as recorded.
func TestDraftJobLifecycle(t *testing.T) {
	w := newMovesWorld(t)
	ctx := w.ctx
	w.rule.Draft = &rules.Draft{Instruction: "say thanks", ReadPrevious: true}
	if err := w.s.UpdateRule(ctx, w.user.TenantID, w.rule, 1); err != nil {
		t.Fatal(err)
	}
	m, _, err := w.s.IngestMessage(ctx, mail.MsgRef{AccountID: w.acct.ID, Folder: "INBOX", UIDValidity: 1, UID: 1}, 1)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := w.s.AddDecision(ctx, Decision{MessageID: m.ID, Stage: "condition", RuleID: w.rule.ID, RuleName: "Receipts", CreatedAt: 100}, StateActed)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := w.s.LiveBatch(ctx, w.user.TenantID, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	a := Action{DecisionID: dec, BatchID: batch, MessageID: m.ID, AccountID: w.acct.ID, Kind: rules.ActMove, Folder: "Receipts",
		Before: Snapshot{Folder: "INBOX", UIDValidity: 1, UID: 1}, Status: ActionFailed, CreatedAt: 100}
	if a.ID, err = w.s.InsertAction(ctx, a); err != nil {
		t.Fatal(err)
	}
	if on, err := w.s.RunInEffect(ctx, w.user.TenantID, dec); err != nil || on {
		t.Errorf("a run whose action has not finished is in effect: %v, %v", on, err)
	}
	a.Status, a.After = ActionDone, &Snapshot{Folder: "Receipts", UIDValidity: 9, UID: 1}
	if err := w.s.FinishAction(ctx, a, FollowUps{Draft: true}); err != nil {
		t.Fatal(err)
	}

	if due, _ := w.s.WaitingDrafts(ctx, 99, 10); len(due) != 0 {
		t.Errorf("a job is due before its rule acted: %+v", due)
	}
	jobs, err := w.s.WaitingDrafts(ctx, 100, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v, %v", jobs, err)
	}
	j := jobs[0]
	if j.RuleID != w.rule.ID || j.Instruction != "say thanks" || !j.ReadPrevious || len(j.Key) < 20 || j.DecisionID != dec || j.BatchID != batch {
		t.Errorf("job = %+v", j)
	}
	if on, err := w.s.RunInEffect(ctx, w.user.TenantID, dec); err != nil || !on {
		t.Errorf("RunInEffect = %v, %v", on, err)
	}

	if err := w.s.DraftRetry(ctx, j, 500); err != nil {
		t.Fatal(err)
	}
	if due, _ := w.s.WaitingDrafts(ctx, 499, 10); len(due) != 0 {
		t.Errorf("a job retried at 500 is due at 499: %+v", due)
	}
	if due, _ := w.s.WaitingDrafts(ctx, 500, 10); len(due) != 1 || due[0].Attempts != 1 || due[0].Key != j.Key {
		t.Errorf("retried job = %+v", due)
	}

	if _, found, err := w.s.DecisionDraft(ctx, w.user.TenantID, dec); err != nil || found {
		t.Errorf("a draft action before any: %v, %v", found, err)
	}
	draft := Action{DecisionID: dec, BatchID: batch, MessageID: m.ID, AccountID: w.acct.ID, Kind: ActionDraft, Folder: "Drafts",
		Before: *a.After, Status: ActionFailed, Error: ActionInterrupted, CreatedAt: 200}
	if draft.ID, err = w.s.InsertAction(ctx, draft); err != nil {
		t.Fatal(err)
	}
	draft.Status, draft.Error, draft.After = ActionDone, "", a.After
	draft.Draft = &DraftRef{UIDValidity: 7, UID: 3, MessageID: "mailrules-draft-" + j.Key + "@example.test"}
	if err := w.s.FinishAction(ctx, draft, FollowUps{Draft: true}); err != nil { // a draft action queues no other draft
		t.Fatal(err)
	}
	got, found, err := w.s.DecisionDraft(ctx, w.user.TenantID, dec)
	if err != nil || !found || got.ID != draft.ID || got.Status != ActionDone || got.Folder != "Drafts" || *got.Draft != *draft.Draft {
		t.Errorf("DecisionDraft = %+v, %v, %v", got, found, err)
	}
	if due, _ := w.s.WaitingDrafts(ctx, 1000, 10); len(due) != 1 {
		t.Errorf("jobs after the draft action = %+v, want the one", due)
	}
	if err := w.s.MarkActionUndone(ctx, a, mail.MsgRef{AccountID: w.acct.ID, Folder: "INBOX", UIDValidity: 1, UID: 2}, 300); err != nil {
		t.Fatal(err)
	}
	if on, _ := w.s.RunInEffect(ctx, w.user.TenantID, dec); on {
		t.Error("an undone run is in effect")
	}

	if err := w.s.DraftDone(ctx, j); err != nil {
		t.Fatal(err)
	}
	if due, _ := w.s.WaitingDrafts(ctx, 1000, 10); len(due) != 0 {
		t.Errorf("a done job still waits: %+v", due)
	}
}
