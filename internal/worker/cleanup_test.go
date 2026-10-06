package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/composer"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/mailtest"
	"github.com/TurtleByte-IN/mailrules/internal/pipeline"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// logged is a log destination that can be read while the daemon's goroutines write to it.
type logged struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logged) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logged) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// lines are the lines whose message is msg.
func (l *logged) lines(t *testing.T, msg string) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(l.b.String()), "\n") {
		var v map[string]any
		if json.Unmarshal([]byte(line), &v) == nil && v["msg"] == msg {
			out = append(out, v)
		}
	}
	return out
}

// checkRows runs a read-only check of a folder and returns its rows, the input a Sort takes.
func checkRows(t *testing.T, st *store.Store, mb composer.Reader, account, userID int64, folder string) []composer.CheckRow {
	t.Helper()
	rs, err := st.Rules(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	senders, err := st.SenderRules(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	tester := composer.Tester{Store: st, Mailbox: mb, AccountID: account, Decider: pipeline.Decider{MinConfidence: 0.75}, BodyChars: 2000}
	rows, err := tester.Check(context.Background(), rs, senders, folder, time.Time{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestManagerSort(t *testing.T) {
	log, old := &logged{}, slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(log, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	e := newEnv(t)
	ctx := t.Context()
	id := e.sup.Account.ID
	e.mb.AddFolder("Old", "")
	for i := range 4 {
		e.mb.Deliver("Old", eml(fmt.Sprintf("old %d", i)))
	}
	m := &Manager{}
	rows := checkRows(t, e.st, e.mb, id, e.sup.Account.UserID, "Old")
	if len(rows) != 4 || rows[0].Outcome.Stage != "condition" || len(rows[0].Outcome.Actions) == 0 {
		t.Fatalf("check rows = %+v", rows)
	}

	// An account that is not running cannot be sorted.
	if _, err := m.Sort(ctx, SortRun{AccountID: id, Folder: "Old", Rows: rows}); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("sort of an account that is not running: %v", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	m.Start(runCtx, e.sup)
	eventually(t, "the account's mailbox", func() bool { _, err := m.Mailbox(id); return err == nil })
	status := func(batchID int64) store.Batch {
		t.Helper()
		b, err := e.st.Batch(ctx, batchID)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	// One email moved since the check: the Sort passes it over and counts it skipped.
	if _, err := e.mb.Move(ctx, rows[0].Ref, "Sent"); err != nil {
		t.Fatal(err)
	}

	// While the account is held, a second Sort for it is refused.
	unlock := m.Lock(id)
	b, err := m.Sort(ctx, SortRun{AccountID: id, Folder: "Old", Rows: rows})
	if err != nil || b.Kind != store.BatchCleanup || b.Status != store.BatchRunning || b.Total != 4 || b.AccountID != id || b.Folder != "Old" {
		t.Fatalf("started = %+v, %v", b, err)
	}
	if _, err := m.Sort(ctx, SortRun{AccountID: id, Folder: "Old", Rows: rows}); !errors.Is(err, ErrCleanupRunning) {
		t.Fatalf("a second sort: %v", err)
	}
	unlock()
	eventually(t, "the run to finish", func() bool { return status(b.ID).Status == store.BatchDone })
	if got := status(b.ID); got.Done != 4 || got.Total != 4 || got.Skipped != 1 {
		t.Errorf("finished = %+v", got)
	}
	// Three were sorted (the moved one was skipped), each by the Reading rule; the pipeline
	// here has no executor, so there are decisions and no actions.
	actRows, err := e.st.Activity(ctx, store.ActivityFilter{})
	if err != nil || len(actRows) != 3 || actRows[0].Message.State != store.StateActed {
		t.Fatalf("sorted = %d rows, %v", len(actRows), err)
	}
	// Sorting a folder leaves its watch position alone: the folder is known (it was
	// discovered on connect) but no last-seen UID was recorded for it.
	if f, err := e.st.Folder(ctx, id, "Old"); err != nil || f.LastUID != 0 {
		t.Errorf("cleanup recorded a watch position for the folder it sorted: %+v, %v", f, err)
	}

	// The finish line says what the run did, and carries no subject.
	finished := log.lines(t, "cleanup sort finished")
	if len(finished) != 1 || finished[0]["status"] != store.BatchDone || finished[0]["handled"] != float64(4) ||
		finished[0]["skipped"] != float64(1) || strings.Contains(log.String(), "old 0") {
		t.Errorf("finish line = %v", finished)
	}
	m.Stop(id)
	m.Wait()
}

// A run cut short by its account stopping ends as failed, with what it got through on record.
func TestManagerSortCutShort(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	id := e.sup.Account.ID
	e.mb.AddFolder("Old", "")
	for i := range 5 {
		e.mb.Deliver("Old", eml(fmt.Sprintf("old %d", i)))
	}
	rows := checkRows(t, e.st, e.mb, id, e.sup.Account.UserID, "Old")

	m := &Manager{}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	m.Start(runCtx, e.sup)
	eventually(t, "the account's mailbox", func() bool { _, err := m.Mailbox(id); return err == nil })

	unlock := m.Lock(id) // hold the run between emails
	b, err := m.Sort(ctx, SortRun{AccountID: id, Folder: "Old", Rows: rows})
	if err != nil {
		t.Fatal(err)
	}
	go func() { m.Stop(id); unlock() }() // Stop cancels the account; the run then fails fast
	eventually(t, "the run to end failed", func() bool {
		got, err := e.st.Batch(ctx, b.ID)
		return err == nil && got.Status == store.BatchFailed
	})
	if got, _ := e.st.Batch(ctx, b.ID); got.Done != 0 || got.Total != 5 {
		t.Errorf("cut short = %+v", got)
	}
	m.Wait()

	// What a daemon that stopped mid-run left behind is closed at the next start.
	left, err := e.st.CreateCleanupBatch(ctx, id, "Old", 0, 9, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.FailRunningBatches(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.st.Batch(ctx, left.ID); got.Status != store.BatchFailed {
		t.Errorf("after the restart sweep the leftover batch is %+v", got)
	}
}

// Two accounts can be sorted at once: a Sort of one is not refused while the other runs.
func TestManagerSortPerAccount(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	userID := e.sup.Account.UserID

	// A second account on the same user, with its own mailbox and the same rules.
	acct2, err := e.st.CreateAccount(ctx, make([]byte, 32),
		store.Account{UserID: userID, Label: "two", Preset: "generic", Host: "h", Port: 993, TLSMode: "implicit", Username: "me2"}, "pw")
	if err != nil {
		t.Fatal(err)
	}
	mb2 := mailtest.New(acct2.ID)
	mb2.AddFolder("Sent", mail.RoleSent)
	mb2.AddFolder("Old", "")
	for i := range 3 {
		mb2.Deliver("Old", eml(fmt.Sprintf("two %d", i)))
	}
	sup2 := &Supervisor{Account: acct2, Store: e.st, Hub: e.sup.Hub,
		Open:       func(context.Context) (mail.Mailbox, error) { return mb2, nil },
		Pipeline:   pipeline.Pipeline{Store: e.st, Hub: e.sup.Hub, MinConfidence: 0.75, BodyChars: 2000},
		BackoffMin: time.Millisecond, BackoffMax: 5 * time.Millisecond}

	e.mb.AddFolder("Old", "")
	for i := range 3 {
		e.mb.Deliver("Old", eml(fmt.Sprintf("one %d", i)))
	}

	m := &Manager{}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	m.Start(runCtx, e.sup)
	m.Start(runCtx, sup2)
	eventually(t, "both mailboxes", func() bool {
		_, e1 := m.Mailbox(e.sup.Account.ID)
		_, e2 := m.Mailbox(acct2.ID)
		return e1 == nil && e2 == nil
	})

	rows1 := checkRows(t, e.st, e.mb, e.sup.Account.ID, userID, "Old")
	rows2 := checkRows(t, e.st, mb2, acct2.ID, userID, "Old")

	// Hold both accounts' processors, then start a Sort for each: neither refuses the other.
	unlock1 := m.Lock(e.sup.Account.ID)
	unlock2 := m.Lock(acct2.ID)
	b1, err := m.Sort(ctx, SortRun{AccountID: e.sup.Account.ID, Folder: "Old", Rows: rows1})
	if err != nil {
		t.Fatalf("sort of account one: %v", err)
	}
	b2, err := m.Sort(ctx, SortRun{AccountID: acct2.ID, Folder: "Old", Rows: rows2})
	if err != nil {
		t.Fatalf("sort of account two while one runs: %v", err)
	}
	if b1.AccountID != e.sup.Account.ID || b2.AccountID != acct2.ID || b1.ID == b2.ID {
		t.Fatalf("batches = %+v, %+v", b1, b2)
	}
	unlock1()
	unlock2()
	eventually(t, "both runs to finish", func() bool {
		g1, _ := e.st.Batch(ctx, b1.ID)
		g2, _ := e.st.Batch(ctx, b2.ID)
		return g1.Status == store.BatchDone && g2.Status == store.BatchDone
	})
	m.Stop(e.sup.Account.ID)
	m.Stop(acct2.ID)
	m.Wait()
}
