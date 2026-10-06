package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// ErrCleanupRunning means the account is already being cleaned up.
var ErrCleanupRunning = errors.New("a cleanup is already running for this account")

// Cleanup asks for existing mail to be sorted with the current rules.
type Cleanup struct {
	AccountID int64
	Folder    string
	Since     time.Time // only mail received from this time on; zero = all of it
	Limit     int       // only the newest Limit emails; 0 = no limit
}

// Cleanup starts a cleanup run and returns its batch at once; the run goes on in the
// background until it is through, the account's supervisor stops or the daemon does. Each
// email goes through the account's own pipeline and the executor, exactly as new mail
// does, so dry-run is honoured and every action is on record; all of them belong to the
// one batch, which makes the run undoable as a whole. ctx only covers listing the mail.
func (m *Manager) Cleanup(ctx context.Context, c Cleanup) (store.Batch, error) {
	m.mu.Lock()
	r := m.sups[c.AccountID]
	switch {
	case r == nil:
		m.mu.Unlock()
		return store.Batch{}, fmt.Errorf("account %d: %w", c.AccountID, ErrNotConnected)
	case m.cleaning[c.AccountID]:
		m.mu.Unlock()
		return store.Batch{}, ErrCleanupRunning
	}
	if m.cleaning == nil {
		m.cleaning = map[int64]bool{}
	}
	m.cleaning[c.AccountID] = true
	m.mu.Unlock()
	started := false
	finish := func() {
		m.mu.Lock()
		delete(m.cleaning, c.AccountID)
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
		return store.Batch{}, fmt.Errorf("account %d: %w", c.AccountID, ErrNotConnected)
	}
	refs, err := mb.FetchSince(ctx, c.Folder, c.Since, c.Limit)
	if err != nil {
		return store.Batch{}, fmt.Errorf("list %s: %w", c.Folder, err)
	}
	now := time.Now
	if s.Pipeline.Now != nil {
		now = s.Pipeline.Now
	}
	var since int64
	if !c.Since.IsZero() {
		since = c.Since.Unix()
	}
	batch, err := s.Store.CreateCleanupBatch(ctx, c.AccountID, c.Folder, since, len(refs), now().Unix())
	if err != nil {
		return store.Batch{}, err
	}
	started = true
	slog.InfoContext(ctx, "cleanup started", "account", c.AccountID, "batch", batch.ID, "folder", c.Folder, "limit", c.Limit, "messages", len(refs))
	m.wg.Go(func() {
		defer finish()
		s.cleanup(r.ctx, mb, batch.ID, refs)
	})
	return batch, nil
}

// cleanup sorts refs, oldest first. Between two emails it lets go of the account, so new
// mail, an undo or a correction is never kept waiting for the whole run.
func (s *Supervisor) cleanup(ctx context.Context, mb mail.Mailbox, batchID int64, refs []mail.MsgRef) {
	p := s.Pipeline
	p.Mailbox, p.Account, p.Batch = mb, s.Account, batchID
	p.Rediscover = func(ctx context.Context) error { return s.discover(ctx, mb) }
	var handled, tokens int
	var cost float64
	began := time.Now()
	var total struct { // what the whole run did; the counters above are cleared at each report
		handled, tokens int
		cost            float64
	}
	p.Spent = func(t int, c float64) { tokens, cost = tokens+t, cost+c }

	// What the run did must be on record even while the daemon stops.
	recCtx := context.WithoutCancel(ctx)
	report := func(status string) {
		total.handled, total.tokens, total.cost = total.handled+handled, total.tokens+tokens, total.cost+cost
		err := s.Store.AddBatchProgress(recCtx, batchID, handled, tokens, cost)
		handled, tokens, cost = 0, 0, 0
		if status != "" {
			err = errors.Join(err, s.Store.SetBatchStatus(recCtx, batchID, status))
		}
		b, berr := s.Store.Batch(recCtx, batchID)
		if err = errors.Join(err, berr); err != nil {
			slog.ErrorContext(ctx, "could not record cleanup progress", "account", s.Account.ID, "batch", batchID, "error", err.Error())
			return
		}
		s.Hub.Publish(events.BatchProgress, b)
		if status != "" {
			slog.InfoContext(ctx, "cleanup finished", "account", s.Account.ID, "batch", batchID, "status", status, "handled", total.handled,
				"of", len(refs), "tokens", total.tokens, "cost_usd", total.cost, "duration_ms", time.Since(began).Milliseconds())
		}
	}
	// About a hundred progress events per run, however long it is: the event stream keeps
	// only the last 200 events for everyone.
	every := max(1, len(refs)/100)
	for i, ref := range refs {
		// A connection that was lost or replaced would fail every remaining email.
		if ctx.Err() != nil || s.Mailbox() != mb {
			report(store.BatchFailed)
			return
		}
		s.work.Lock()
		err := p.Sort(ctx, ref)
		s.work.Unlock()
		if err != nil { // the outcome could not be recorded at all, or the daemon is stopping
			slog.WarnContext(ctx, "cleanup stopped", "account", s.Account.ID, "batch", batchID, "error", err.Error())
			report(store.BatchFailed)
			return
		}
		handled++
		if (i+1)%every == 0 && i+1 < len(refs) {
			report("")
		}
	}
	report(store.BatchDone)
}
