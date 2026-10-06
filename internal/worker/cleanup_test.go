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

	"github.com/TurtleByte-IN/mailrules/internal/mail"
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

func TestCleanupRun(t *testing.T) {
	log, old := &logged{}, slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(log, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	e := newEnv(t)
	ctx := t.Context()
	id := e.sup.Account.ID
	e.mb.AddFolder("Old", "")
	for i := range 5 {
		e.mb.Deliver("Old", eml(fmt.Sprintf("old %d", i)))
	}
	m := &Manager{}
	if _, err := m.Cleanup(ctx, Cleanup{AccountID: id, Folder: "Old"}); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("cleanup of an account that is not running: %v", err)
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

	if _, err := m.Cleanup(ctx, Cleanup{AccountID: id, Folder: "Nope"}); !errors.Is(err, mail.ErrNoFolder) {
		t.Fatalf("cleanup of a folder that does not exist: %v", err)
	}
	// While the account is held (an undo is at work, say) the run waits between emails;
	// a second run for the same account is refused meanwhile.
	unlock := m.Lock(id)
	b, err := m.Cleanup(ctx, Cleanup{AccountID: id, Folder: "Old", Limit: 3})
	if err != nil || b.Kind != store.BatchCleanup || b.Status != store.BatchRunning || b.Total != 3 || b.AccountID != id || b.Folder != "Old" {
		t.Fatalf("started = %+v, %v", b, err)
	}
	if _, err := m.Cleanup(ctx, Cleanup{AccountID: id, Folder: "Old"}); !errors.Is(err, ErrCleanupRunning) {
		t.Fatalf("a second cleanup: %v", err)
	}
	unlock()
	eventually(t, "the run to finish", func() bool { return status(b.ID).Status == store.BatchDone })
	if got := status(b.ID); got.Done != 3 || got.Total != 3 {
		t.Errorf("finished = %+v", got)
	}
	// Only the newest three were sorted, each by the Reading rule. (This test's pipeline
	// has no executor, so there are decisions and no actions.)
	rows, err := e.st.Activity(ctx, store.ActivityFilter{})
	if err != nil || len(rows) != 3 || rows[0].Message.Subject != "old 4" || rows[2].Message.Subject != "old 2" || rows[0].Message.State != store.StateActed {
		t.Fatalf("sorted = %+v, %v", rows, err)
	}
	// Sorting another folder leaves the watched folder's position alone.
	if f, err := e.st.Folder(ctx, id, "Old"); err != nil || f.LastUID != 0 {
		t.Errorf("cleanup recorded a watch position for the folder it sorted: %+v, %v", f, err)
	}

	// A run cut short by its account stopping ends as failed, with what it got through.
	unlock = m.Lock(id)
	b, err = m.Cleanup(ctx, Cleanup{AccountID: id, Folder: "Old"})
	if err != nil {
		t.Fatal(err)
	}
	go func() { m.Stop(id); unlock() }() // Stop waits for the supervisor; the run still holds on the lock
	eventually(t, "the run to end", func() bool { return status(b.ID).Status == store.BatchFailed })
	if got := status(b.ID); got.Done != 0 || got.Total != 5 {
		t.Errorf("cut short = %+v", got)
	}
	m.Wait()
	// Each run wrote a start line and a finish line with what it did.
	started, finished := log.lines(t, "cleanup started"), log.lines(t, "cleanup finished")
	if len(started) != 2 || len(finished) != 2 || started[0]["messages"] != float64(3) || started[0]["limit"] != float64(3) || started[0]["level"] != "INFO" {
		t.Fatalf("start lines %v, finish lines %v", started, finished)
	}
	if f := finished[0]; f["status"] != store.BatchDone || f["handled"] != float64(3) || f["of"] != float64(3) || f["batch"] != started[0]["batch"] || f["level"] != "INFO" {
		t.Errorf("finish line of the first run = %v", f)
	}
	if _, ok := finished[0]["duration_ms"].(float64); !ok {
		t.Errorf("finish line has no duration_ms: %v", finished[0])
	}
	if f := finished[1]; f["status"] != store.BatchFailed || f["handled"] != float64(0) || f["of"] != float64(5) {
		t.Errorf("finish line of the cut-short run = %v", f)
	}
	// What a daemon that stopped mid-run left behind is closed at the next start.
	left, err := e.st.CreateCleanupBatch(ctx, id, "Old", 0, 9, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.FailRunningBatches(ctx); err != nil || status(left.ID).Status != store.BatchFailed || status(1).Status != store.BatchDone {
		t.Errorf("after the restart sweep: %+v, first batch %+v, %v", status(left.ID), status(1), err)
	}
}
