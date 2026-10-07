package worker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/composer"
	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// ErrCleanupRunning means the account is already being sorted.
var ErrCleanupRunning = errors.New("a cleanup is already running for this account")

// Cleanup describes which mail a cleanup check, and the Sort that follows it, covers.
type Cleanup struct {
	AccountID int64
	Folder    string
	Since     time.Time // only mail received from this time on; zero = all of it
	Limit     int       // only the newest Limit emails; 0 = no limit
}

// SortRun is the Sort that follows a finished check: the rows the user kept, applied with
// the answers the check already settled, as one undoable cleanup batch. It makes no model
// calls — the decisions are replayed, never asked again.
type SortRun struct {
	AccountID int64
	Folder    string
	Since     int64 // only mail received from this time on; 0 = all of it. Recorded on the batch.
	// Limit and Matched are the check's: the most emails it covered and how many the range
	// held before that cut. Recorded on the batch, so it can say what it covered (MAI-48).
	Limit, Matched int
	Rows           []composer.CheckRow
}

// Sort starts applying a check's kept rows in the background and returns the run's batch at
// once; the run goes on until it is through, the account's supervisor stops or the daemon
// does. Each email is verified still where the check found it (BODY.PEEK, never marking it
// read) and then its saved decision is handed to the executor, exactly as a live email's
// is, so dry-run is honoured and every action is on record; all of them belong to the one
// batch, which makes the run undoable as a whole. One Sort per account at a time. ctx only
// covers opening the batch.
func (m *Manager) Sort(ctx context.Context, sr SortRun) (store.Batch, error) {
	m.mu.Lock()
	r := m.sups[sr.AccountID]
	switch {
	case r == nil:
		m.mu.Unlock()
		return store.Batch{}, fmt.Errorf("account %d: %w", sr.AccountID, ErrNotConnected)
	case m.cleaning[sr.AccountID]:
		m.mu.Unlock()
		return store.Batch{}, ErrCleanupRunning
	}
	if m.cleaning == nil {
		m.cleaning = map[int64]bool{}
	}
	m.cleaning[sr.AccountID] = true
	m.mu.Unlock()
	started := false
	finish := func() {
		m.mu.Lock()
		delete(m.cleaning, sr.AccountID)
		m.mu.Unlock()
	}
	defer func() {
		if !started {
			finish()
		}
	}()

	s := r.sup
	mb := s.Mailbox()
	if mb == nil {
		return store.Batch{}, fmt.Errorf("account %d: %w", sr.AccountID, ErrNotConnected)
	}
	now := time.Now
	if s.Pipeline.Now != nil {
		now = s.Pipeline.Now
	}
	// Oldest first, as the run has always sorted.
	rows := append([]composer.CheckRow(nil), sr.Rows...)
	slices.SortStableFunc(rows, func(a, b composer.CheckRow) int {
		if a.ReceivedAt != b.ReceivedAt {
			return cmp.Compare(a.ReceivedAt, b.ReceivedAt)
		}
		return cmp.Compare(a.Ref.UID, b.Ref.UID)
	})
	batch, err := s.Store.CreateCleanupBatch(ctx, sr.AccountID, sr.Folder, sr.Since, sr.Limit, sr.Matched, len(rows), now().Unix())
	if err != nil {
		return store.Batch{}, err
	}
	started = true
	slog.InfoContext(ctx, "cleanup sort started", "account", sr.AccountID, "batch", batch.ID, "folder", sr.Folder, "messages", len(rows))
	m.wg.Go(func() {
		defer finish()
		s.sort(r.ctx, mb, batch.ID, rows)
	})
	return batch, nil
}

// sort applies the kept rows, oldest first. Between two emails it lets go of the account,
// so new mail, an undo or a correction is never kept waiting for the whole run. It asks no
// model: the decisions were settled by the check. An email that is no longer where the
// check found it is passed over and counted as skipped.
func (s *Supervisor) sort(ctx context.Context, mb mail.Mailbox, batchID int64, rows []composer.CheckRow) {
	acct := s.account() // the supervisor may change its status fields while this runs
	p := s.Pipeline
	p.Mailbox, p.Account, p.Batch = mb, acct, batchID
	p.Rediscover = func(ctx context.Context) error { return s.discover(ctx, mb) }
	var handled, skipped int
	began := time.Now()
	var total struct { // what the whole run did; the counters above are cleared at each report
		handled, skipped int
	}

	// What the run did must be on record even while the daemon stops.
	recCtx := context.WithoutCancel(ctx)
	report := func(status string) {
		total.handled, total.skipped = total.handled+handled, total.skipped+skipped
		err := s.Store.AddBatchProgress(recCtx, batchID, handled, skipped, 0, 0)
		handled, skipped = 0, 0
		if status != "" {
			err = errors.Join(err, s.Store.SetBatchStatus(recCtx, batchID, status))
		}
		b, berr := s.Store.Batch(recCtx, batchID)
		if err = errors.Join(err, berr); err != nil {
			slog.ErrorContext(ctx, "could not record cleanup sort progress", "account", acct.ID, "batch", batchID, "error", err.Error())
			return
		}
		s.Hub.Publish(events.BatchProgress, b)
		if status != "" {
			slog.InfoContext(ctx, "cleanup sort finished", "account", acct.ID, "batch", batchID, "status", status,
				"handled", total.handled, "of", len(rows), "skipped", total.skipped, "duration_ms", time.Since(began).Milliseconds())
		}
	}
	// About a hundred progress events per run, however long it is.
	every := max(1, len(rows)/100)
	for i, row := range rows {
		// A connection that was lost or replaced would fail every remaining email.
		if ctx.Err() != nil || s.Mailbox() != mb {
			report(store.BatchFailed)
			return
		}
		s.work.Lock()
		applied, err := p.SortSaved(ctx, row.Ref, row.Outcome)
		s.work.Unlock()
		if err != nil { // the outcome could not be recorded at all, or the daemon is stopping
			slog.WarnContext(ctx, "cleanup sort stopped", "account", acct.ID, "batch", batchID, "error", err.Error())
			report(store.BatchFailed)
			return
		}
		handled++
		if !applied {
			skipped++
		}
		if (i+1)%every == 0 && i+1 < len(rows) {
			report("")
		}
	}
	report(store.BatchDone)
}
