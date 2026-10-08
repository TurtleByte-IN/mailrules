package worker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
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
	// RuleIDs limits the check to these rules, checked in their usual order; nil = every
	// enabled rule. Sender rules apply either way (MAI-43).
	RuleIDs []int64
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
	bs, err := m.SortAll(ctx, []SortRun{sr})
	if err != nil {
		return store.Batch{}, err
	}
	return bs[0], nil
}

// SortAll starts the Sort of several mailboxes together, one batch each, and returns the
// batches in the order of runs. They are all started or none is: when a mailbox is not
// connected or already being sorted, nothing starts and the error says so. More than one
// batch makes a manual run over several mailboxes: they share a store.Batch.RunID, so they
// read back as one run, and each is undone on its own like any cleanup batch (MAI-43). A
// mailbox may be named only once.
func (m *Manager) SortAll(ctx context.Context, runs []SortRun) ([]store.Batch, error) {
	if len(runs) == 0 {
		return nil, nil
	}
	type taken struct {
		sr   SortRun
		r    *running
		mb   mail.Mailbox
		sort *Supervisor
	}
	var (
		parts   = make([]taken, len(runs))
		claimed []int64
		started bool
	)
	release := func(ids ...int64) {
		m.mu.Lock()
		for _, id := range ids {
			delete(m.cleaning, id)
		}
		m.mu.Unlock()
	}
	// Anything claimed and not handed to a goroutine is let go again.
	defer func() {
		if !started {
			release(claimed...)
		}
	}()

	m.mu.Lock()
	for i, sr := range runs {
		r := m.sups[sr.AccountID]
		var err error
		switch {
		case r == nil:
			err = fmt.Errorf("account %d: %w", sr.AccountID, ErrNotConnected)
		case m.cleaning[sr.AccountID]:
			err = ErrCleanupRunning
		}
		if err != nil {
			m.mu.Unlock()
			return nil, err
		}
		if m.cleaning == nil {
			m.cleaning = map[int64]bool{}
		}
		m.cleaning[sr.AccountID] = true
		claimed = append(claimed, sr.AccountID)
		parts[i] = taken{sr: sr, r: r}
	}
	m.mu.Unlock()

	now := time.Now
	specs := make([]store.CleanupBatch, len(runs))
	for i := range parts {
		p := &parts[i]
		p.sort = p.r.sup
		if p.mb = p.sort.Mailbox(); p.mb == nil {
			return nil, fmt.Errorf("account %d: %w", p.sr.AccountID, ErrNotConnected)
		}
		if i == 0 && p.sort.Pipeline.Now != nil {
			now = p.sort.Pipeline.Now
		}
		specs[i] = store.CleanupBatch{AccountID: p.sr.AccountID, Folder: p.sr.Folder, Since: p.sr.Since,
			ScanLimit: p.sr.Limit, ScanMatched: p.sr.Matched, Total: len(p.sr.Rows)}
	}
	batches, err := parts[0].sort.Store.CreateCleanupRun(ctx, specs, now().Unix())
	if err != nil {
		return nil, err
	}
	started = true
	for i, p := range parts {
		// Oldest first, as the run has always sorted.
		rows := append([]composer.CheckRow(nil), p.sr.Rows...)
		slices.SortStableFunc(rows, func(a, b composer.CheckRow) int {
			if a.ReceivedAt != b.ReceivedAt {
				return cmp.Compare(a.ReceivedAt, b.ReceivedAt)
			}
			return cmp.Compare(a.Ref.UID, b.Ref.UID)
		})
		slog.InfoContext(ctx, "cleanup sort started", "account", p.sr.AccountID, "batch", batches[i].ID, "folder", p.sr.Folder, "messages", len(rows), "run_size", len(runs))
		m.wg.Go(func() {
			// The mailbox is let go before the run is marked finished, so anyone who sees
			// it finished can start the next one at once.
			free := sync.OnceFunc(func() { release(p.sr.AccountID) })
			defer free()
			p.sort.sort(p.r.ctx, p.mb, batches[i].ID, rows, free)
		})
	}
	return batches, nil
}

// sort applies the kept rows, oldest first. Between two emails it lets go of the account,
// so new mail, an undo or a correction is never kept waiting for the whole run. It asks no
// model: the decisions were settled by the check. An email that is no longer where the
// check found it is passed over and counted as skipped. finish is called once the last
// email is handled and before the run is recorded as finished.
func (s *Supervisor) sort(ctx context.Context, mb mail.Mailbox, batchID int64, rows []composer.CheckRow, finish func()) {
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
			finish()
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
