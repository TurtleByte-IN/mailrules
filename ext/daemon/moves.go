package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

const (
	// moveEvery is how often the worker looks for moves to hand over, when the watcher has
	// not just woken it.
	moveEvery = 2 * time.Second
	// moveBatch is how many moves one look hands over at most.
	moveBatch = 50
	// moveTimeout bounds one Moved call (ext.Moves.Moved).
	moveTimeout = 30 * time.Second
)

// movesModule is the build's module that learns from the person's moves, or nil.
func movesModule(modules []ext.Module) *ext.Moves {
	for _, m := range modules {
		if m.Moves != nil {
			return m.Moves
		}
	}
	return nil
}

// moveWorker hands the moves the watcher found to the module, at least once each: a move
// stays waiting until Moved takes it, across restarts.
type moveWorker struct {
	store     *store.Store
	moves     *ext.Moves
	questions questions
	wake      chan struct{}    // the watcher found a move
	every     time.Duration    // 0 = moveEvery
	now       func() time.Time // nil = time.Now
}

func newMoveWorker(st *store.Store, m *ext.Moves, hub *events.Hub) *moveWorker {
	return &moveWorker{store: st, moves: m, questions: questions{store: st, hub: hub}, wake: make(chan struct{}, 1)}
}

func (w *moveWorker) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

// found wakes the worker: the watcher recorded a move of the person. It never blocks.
func (w *moveWorker) found(store.Move) {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run hands over what is waiting now, then whenever the watcher finds a move and every few
// seconds, until ctx is done.
func (w *moveWorker) Run(ctx context.Context) {
	every := w.every
	if every == 0 {
		every = moveEvery
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if err := w.once(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "handing moves to the module failed", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-w.wake:
		}
	}
}

// once hands over the moves that are due, oldest first. One the module refuses does not
// hold up the rest.
func (w *moveWorker) once(ctx context.Context) error {
	waiting, err := w.store.WaitingMoves(ctx, w.clock().Unix(), moveBatch)
	if err != nil {
		return err
	}
	var errs []error
	for _, m := range waiting {
		if ctx.Err() != nil {
			break
		}
		if err := w.hand(ctx, m); err != nil {
			errs = append(errs, fmt.Errorf("move %d: %w", m.ID, err))
		}
	}
	return errors.Join(errs...)
}

// hand gives one move to the module.
func (w *moveWorker) hand(ctx context.Context, m store.Move) error {
	callCtx, cancel := context.WithTimeout(ctx, moveTimeout)
	err := w.moves.Moved(callCtx, toMove(m), w.questions)
	cancel()
	now := w.clock()
	if err != nil {
		if ctx.Err() != nil {
			return nil // stopping: it is handed over again after the restart
		}
		slog.WarnContext(ctx, "the module did not take a move; it is handed over again later",
			"move_id", m.ID, "attempts", m.Attempts+1, "error", err.Error())
		return w.store.MoveRetry(ctx, m.ID, now.Add(retryAfter(m.Attempts)).Unix())
	}
	return w.store.MoveHanded(context.WithoutCancel(ctx), m.ID, now.Unix())
}

func toMove(m store.Move) ext.Move {
	return ext.Move{ID: m.ID, TenantID: m.TenantID, AccountID: m.AccountID, Mailbox: m.Mailbox, From: m.From,
		Filed: m.Filed, Chosen: m.Chosen, Inbox: m.Inbox, RuleID: m.RuleID, RuleName: m.RuleName,
		FiledAt: time.Unix(m.FiledAt, 0).UTC(), At: time.Unix(m.CreatedAt, 0).UTC(), Count: m.Count}
}

// questions is where the module asks the person about their moves (ext.Questions). A
// question asked or brought up to date tells the open Activity screens of its tenant.
type questions struct {
	store *store.Store
	hub   *events.Hub
	now   func() time.Time // nil = time.Now
}

var _ ext.Questions = questions{}

func (q questions) Ask(ctx context.Context, moveID int64) error {
	now := time.Now
	if q.now != nil {
		now = q.now
	}
	asked, changed, err := q.store.AskMove(ctx, moveID, now().Unix())
	if errors.Is(err, store.ErrNotFound) {
		return ext.ErrNoMove
	}
	if err != nil {
		return err
	}
	if changed {
		q.hub.Publish(asked.TenantID, asked.AccountID, events.QuestionsChanged, nil)
	}
	return nil
}
