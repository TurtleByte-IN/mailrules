package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// The retention job, on a clock the test moves: snippets go blank after retention_days,
// and after 180 days a message's row goes too, with its decisions and actions, unless
// something done to it can still be undone.
func TestRetentionJob(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	now := time.Unix(1_800_000_000, 0)
	days := 30
	job := Retention{Store: e.st, Days: func(context.Context) int { return days }, Now: func() time.Time { return now }}

	uid := uint32(0)
	// seen records an email first seen ago days back, with a snippet, a decision and one
	// action, made then too, or (acted) that many days back when it was sorted again later.
	seen := func(ago int, kind, status string, acted ...int) store.Message {
		t.Helper()
		uid++
		at := now.AddDate(0, 0, -ago).Unix()
		m, _, err := e.st.IngestMessage(ctx, mail.MsgRef{AccountID: e.sup.Account.ID, Folder: "INBOX", UIDValidity: 1, UID: uid}, at)
		if err != nil {
			t.Fatal(err)
		}
		m.FromAddr, m.Subject, m.Snippet = "a@b.example", "subject", "The first words of the email"
		if err := e.st.SaveMessageSummary(ctx, m); err != nil {
			t.Fatal(err)
		}
		dec, err := e.st.AddDecision(ctx, store.Decision{MessageID: m.ID, Stage: "condition", CreatedAt: at}, store.StateActed)
		if err != nil {
			t.Fatal(err)
		}
		did := at
		if len(acted) > 0 {
			did = now.AddDate(0, 0, -acted[0]).Unix()
		}
		if _, err := e.st.InsertAction(ctx, store.Action{DecisionID: dec, MessageID: m.ID, AccountID: m.AccountID, Kind: kind, Status: status, CreatedAt: did}); err != nil {
			t.Fatal(err)
		}
		return m
	}
	fresh := seen(10, "move", store.ActionDone)
	month := seen(40, "move", store.ActionDone)
	oldUndoable := seen(200, "move", store.ActionDone, 10) // an old email a cleanup moved 10 days ago: still undoable
	oldTooOld := seen(200, "move", store.ActionDone)       // moved 200 days ago: past the 30 days an undo is good for
	oldDryRun := seen(200, "move", store.ActionDryRun)
	oldUndone := seen(181, "move", store.ActionUndone)
	oldReviewTag := seen(200, "review", store.ActionDone)
	edge := seen(180, "move", store.ActionDryRun) // exactly 180 days: not yet older than that

	blanked, deleted, err := job.Once(ctx)
	if err != nil || blanked != 7 || deleted != 4 {
		t.Fatalf("first run: blanked %d, deleted %d, %v; want 7 and 4", blanked, deleted, err)
	}
	snippet := func(m store.Message) string {
		t.Helper()
		got, err := e.st.Message(ctx, m.ID)
		if err != nil {
			t.Fatalf("message %d: %v", m.ID, err)
		}
		return got.Snippet
	}
	if snippet(fresh) == "" || snippet(month) != "" || snippet(oldUndoable) != "" || snippet(edge) != "" {
		t.Errorf("snippets: fresh %q, 40 days %q, undoable %q", snippet(fresh), snippet(month), snippet(oldUndoable))
	}
	for _, m := range []store.Message{oldTooOld, oldDryRun, oldUndone, oldReviewTag} {
		if _, err := e.st.Message(ctx, m.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("message %d is still there: %v", m.ID, err)
		}
		if ds, _ := e.st.MessageDecisions(ctx, m.ID); len(ds) != 0 {
			t.Errorf("message %d left %d decisions behind", m.ID, len(ds))
		}
		if as, _ := e.st.MessageActions(ctx, m.ID); len(as) != 0 {
			t.Errorf("message %d left %d actions behind", m.ID, len(as))
		}
	}
	// What can still be undone keeps its row and its action, so the undo still works.
	if as, _ := e.st.MessageActions(ctx, oldUndoable.ID); len(as) != 1 || as[0].Status != store.ActionDone {
		t.Errorf("the undoable message's actions = %+v", as)
	}

	// Running again changes nothing; a day later the 180-day-old one is past the line.
	if blanked, deleted, err = job.Once(ctx); err != nil || blanked != 0 || deleted != 0 {
		t.Errorf("second run: blanked %d, deleted %d, %v", blanked, deleted, err)
	}
	now = now.AddDate(0, 0, 1)
	if _, deleted, _ = job.Once(ctx); deleted != 1 {
		t.Errorf("a day later: deleted %d, want the one that just turned 181 days old", deleted)
	}
	// The retention setting is read on every run: shortened to a week, the 11-day-old snippet goes.
	days = 7
	if blanked, _, _ = job.Once(ctx); blanked != 1 || snippet(fresh) != "" {
		t.Errorf("with 7 days retention: blanked %d, snippet %q", blanked, snippet(fresh))
	}

	// Run does the job at once and then on every tick, until it is stopped.
	late := seen(400, "read", store.ActionDone, 5) // marked read by a rule 5 days ago: that can still be undone
	stale := seen(400, "keep", store.ActionDone)   // a recorded keep changed nothing: nothing to undo
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	job.Every = time.Millisecond
	go func() { defer close(done); job.Run(runCtx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := e.st.Message(ctx, stale.ID); errors.Is(err, store.ErrNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run did not delete the stale message")
		}
		time.Sleep(time.Millisecond)
	}
	stop()
	<-done
	if _, err := e.st.Message(ctx, late.ID); err != nil {
		t.Errorf("a message with an action that can be undone was deleted: %v", err)
	}
}
