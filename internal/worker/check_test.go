package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/composer"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
)

// The store keeps one check per account: a new one replaces and cancels the previous, Take
// removes only the named check, Discard removes any, and two accounts stay independent.
func TestCheckStore(t *testing.T) {
	s := NewCheckStore()
	cancelled := map[string]bool{}
	mk := func(account int64) *Check {
		id := s.newID()
		return &Check{ID: id, AccountID: account, status: CheckRunning, cancel: func() { cancelled[id] = true }}
	}

	first := mk(1)
	s.put(first)
	if s.Get(1) != first {
		t.Fatal("the account's check was not stored")
	}

	second := mk(1)
	s.put(second)
	if !cancelled[first.ID] {
		t.Error("replacing a check did not cancel the previous one")
	}
	if s.Get(1) != second {
		t.Error("the newer check is not current")
	}

	other := mk(2)
	s.put(other)
	if s.Get(1) != second || s.Get(2) != other {
		t.Error("the two accounts' checks are not kept apart")
	}

	// Take removes only when the id matches: a newer check that replaced it is left alone.
	if got := s.Take(1, first.ID); got != nil || s.Get(1) != second {
		t.Errorf("Take with a stale id removed the current check: %v", got)
	}
	if got := s.Take(1, second.ID); got != second || s.Get(1) != nil {
		t.Errorf("Take with the right id = %v", got)
	}

	if got := s.Discard(2); got != other || !cancelled[other.ID] || s.Get(2) != nil {
		t.Errorf("Discard = %v, cancelled %v", got, cancelled[other.ID])
	}
	if s.Discard(9) != nil {
		t.Error("discarding an account with no check returned something")
	}
}

// A ready check hands its rows to every reader; a progress snapshot omits them.
func TestCheckStateRows(t *testing.T) {
	rows := []composer.CheckRow{
		{Ref: mail.MsgRef{Folder: "INBOX", UID: 1}, From: "a@x.example", Subject: "one"},
		{Ref: mail.MsgRef{Folder: "INBOX", UID: 2}, From: "b@x.example", Subject: "two"},
	}
	chk := &Check{ID: "1", AccountID: 1, status: CheckReady, done: 2, total: 2, rows: rows}
	st := chk.State()
	if st.Status != CheckReady || len(st.Rows) != 2 || st.Rows[0].Subject != "one" || st.Rows[1].Ref.UID != 2 {
		t.Errorf("State rows = %+v", st.Rows)
	}
	if ps := chk.progressState(); len(ps.Rows) != 0 || ps.Done != 2 || ps.Total != 2 {
		t.Errorf("progress state carried rows: %+v", ps)
	}

	// MarkStale moves a ready check to stale and leaves a failed one.
	chk.MarkStale()
	if chk.State().Status != CheckStale {
		t.Error("a ready check did not go stale")
	}
	failed := &Check{ID: "2", status: CheckFailed}
	failed.MarkStale()
	if failed.State().Status != CheckFailed {
		t.Error("a failed check must not be marked stale")
	}
}

// StartCheck runs on the account's background context, so it keeps going after the request
// that began it is gone and finishes once its work is done.
func TestStartCheckRunsInBackground(t *testing.T) {
	e := newEnv(t)
	m := &Manager{}

	// No supervisor: not connected.
	if _, err := m.StartCheck(Cleanup{AccountID: e.sup.Account.ID}, "fp", func(context.Context, func(composer.CheckProgress)) ([]composer.CheckRow, error) {
		return nil, nil
	}); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("StartCheck without a supervisor = %v", err)
	}

	runCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m.Start(runCtx, e.sup)
	eventually(t, "the account's mailbox", func() bool { _, err := m.Mailbox(e.sup.Account.ID); return err == nil })

	release := make(chan struct{})
	rows := []composer.CheckRow{{Ref: mail.MsgRef{Folder: "INBOX", UID: 1}, Subject: "kept"}}
	run := func(ctx context.Context, report func(composer.CheckProgress)) ([]composer.CheckRow, error) {
		report(composer.CheckProgress{Total: 1})
		select {
		case <-release:
			report(composer.CheckProgress{Done: 1, Total: 1})
			return rows, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	chk, err := m.StartCheck(Cleanup{AccountID: e.sup.Account.ID, Folder: "INBOX"}, "fp", run)
	if err != nil || chk.State().Status != CheckRunning {
		t.Fatalf("StartCheck = %+v, %v", chk.State(), err)
	}

	// The request that began the check is gone; an unrelated context is cancelled. The
	// check is tied to the account's background context, so it runs on regardless.
	caller, cancelCaller := context.WithCancel(t.Context())
	cancelCaller()
	<-caller.Done()
	time.Sleep(20 * time.Millisecond)
	if chk.State().Status != CheckRunning {
		t.Fatalf("the check ended when an unrelated context was cancelled: %s", chk.State().Status)
	}
	if m.Checks().Get(e.sup.Account.ID) != chk {
		t.Error("the running check is not the account's current one")
	}

	close(release)
	eventually(t, "the check to finish", func() bool { return chk.State().Status == CheckReady })
	if got := chk.State(); len(got.Rows) != 1 || got.Rows[0].Subject != "kept" || got.Done != 1 {
		t.Errorf("finished check = %+v", got)
	}
	cancel()
	m.Wait()
}

// Starting a second check for an account cancels the first and makes the second current.
func TestStartCheckReplacesRunning(t *testing.T) {
	e := newEnv(t)
	m := &Manager{}
	runCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m.Start(runCtx, e.sup)
	eventually(t, "the account's mailbox", func() bool { _, err := m.Mailbox(e.sup.Account.ID); return err == nil })

	cancelled := make(chan struct{})
	blocking := func(ctx context.Context, _ func(composer.CheckProgress)) ([]composer.CheckRow, error) {
		<-ctx.Done() // runs until it is replaced
		close(cancelled)
		return nil, ctx.Err()
	}
	first, err := m.StartCheck(Cleanup{AccountID: e.sup.Account.ID, Folder: "INBOX"}, "fp", blocking)
	if err != nil {
		t.Fatal(err)
	}

	second, err := m.StartCheck(Cleanup{AccountID: e.sup.Account.ID, Folder: "INBOX"}, "fp2", func(context.Context, func(composer.CheckProgress)) ([]composer.CheckRow, error) {
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the replaced check was not cancelled")
	}
	if first.ID == second.ID {
		t.Error("the replacement reused the first check's id")
	}
	eventually(t, "the second check to become current", func() bool { return m.Checks().Get(e.sup.Account.ID) == second })
	cancel()
	m.Wait()
}
