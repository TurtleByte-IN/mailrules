package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/crypto"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

const (
	// removedEvery is how often the worker looks for removed mailboxes to hand over: a
	// module hears of one within about this long of its removal.
	removedEvery = 2 * time.Second
	// removedBatch is how many removed mailboxes one look hands over at most.
	removedBatch = 50
	// removedTimeout bounds one MailboxRemoved call.
	removedTimeout = 30 * time.Second
)

// mailboxOwner is the module of modules that connects mailboxes of preset, with one-click
// sign-in when oauth, else through its sign-in form; nil when the build has none.
func mailboxOwner(modules []ext.Module, preset string, oauth bool) *ext.Module {
	for i := range modules {
		m := &modules[i]
		if oauth && m.Mailboxes != nil && slices.Contains(m.Mailboxes.Providers, preset) {
			return m
		}
		if !oauth && slices.ContainsFunc(m.MailboxForms, func(f ext.MailboxForm) bool { return f.Provider == preset }) {
			return m
		}
	}
	return nil
}

// removedWorker hands each mailbox a module connected, whose secret the daemon let go of,
// to that module (ext.Module.MailboxRemoved), at least once: it stays queued, across
// restarts, until the module takes it or RemovedMailboxDays pass.
type removedWorker struct {
	store   *store.Store
	modules []ext.Module
	every   time.Duration    // 0 = removedEvery
	now     func() time.Time // nil = time.Now
}

func (w *removedWorker) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

// Run hands over what is waiting now, and then every few seconds, until ctx is done.
func (w *removedWorker) Run(ctx context.Context) {
	every := w.every
	if every == 0 {
		every = removedEvery
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if err := w.once(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "telling modules of removed mailboxes failed", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// once drops what waited too long, then hands over what is due, longest waiting first. One
// that cannot be handed over does not hold up the rest; the errors are returned joined.
func (w *removedWorker) once(ctx context.Context) error {
	now := w.clock().Unix()
	expired, err := w.store.ExpireRemovedMailboxes(ctx, now)
	if err != nil {
		return err
	}
	for _, r := range expired {
		slog.WarnContext(ctx, "dropped a removed mailbox its module never took", "removed_id", r.ID,
			"account_id", r.AccountID, "provider", r.Preset, "attempts", r.Attempts, "days", store.RemovedMailboxDays)
	}
	waiting, err := w.store.WaitingRemovedMailboxes(ctx, now, removedBatch)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range waiting {
		if ctx.Err() != nil {
			break
		}
		if err := w.hand(ctx, r); err != nil {
			errs = append(errs, fmt.Errorf("removed mailbox %d: %w", r.ID, err))
		}
	}
	return errors.Join(errs...)
}

// hand gives one removed mailbox to the module that connected it. One whose module takes
// none is dropped; one whose module is not in this build waits, as for a refusal.
func (w *removedWorker) hand(ctx context.Context, r store.RemovedMailbox) error {
	owner := mailboxOwner(w.modules, r.Preset, r.OAuth)
	if owner != nil && owner.MailboxRemoved == nil {
		return w.store.RemovedMailboxTaken(ctx, r.ID)
	}
	if owner == nil {
		slog.WarnContext(ctx, "no module of this build connects a removed mailbox's provider; it waits for one",
			"removed_id", r.ID, "account_id", r.AccountID, "provider", r.Preset, "one_click", r.OAuth)
		return w.store.RemovedMailboxRetry(ctx, r.ID, w.clock().Add(noticeRetryMax).Unix())
	}
	secret, err := w.store.RemovedMailboxSecret(ctx, r.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil // taken meanwhile
	case errors.Is(err, crypto.ErrDecrypt):
		slog.WarnContext(ctx, "a removed mailbox's secret cannot be opened with this master key; the module gets it without",
			"removed_id", r.ID, "account_id", r.AccountID)
	case err != nil:
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, removedTimeout)
	err = owner.MailboxRemoved(callCtx, ext.RemovedMailbox{ID: r.ID, AccountID: r.AccountID, Provider: r.Preset, Address: r.Username, Secret: secret})
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return nil // stopping: it is handed over again after the restart
		}
		// The module's error holds no secret (ext.Module.MailboxRemoved).
		slog.WarnContext(ctx, "a module did not take a removed mailbox; it is handed over again later",
			"module", owner.Name, "removed_id", r.ID, "account_id", r.AccountID, "attempts", r.Attempts+1, "error", err.Error())
		return w.store.RemovedMailboxRetry(ctx, r.ID, w.clock().Add(retryAfter(r.Attempts)).Unix())
	}
	return w.store.RemovedMailboxTaken(context.WithoutCancel(ctx), r.ID)
}
