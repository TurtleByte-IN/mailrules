package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// Removal is the job that removes what MailRules kept of the people a sign-in module
// forgot (store.ForgetIdentity) once store.ForgetDays have passed. What is waiting lives in
// the database, so a restart only delays a removal to the job's next run.
type Removal struct {
	Store *store.Store
	// Stop stops an account's supervisor and waits until it has; Start starts one again,
	// for a mailbox whose removal did not happen after all (the person signed in again
	// between the two).
	Stop  func(accountID int64)
	Start func(store.Account)
	Hub   *events.Hub      // tells the team's open screens when its rules were rewritten; may be nil
	Now   func() time.Time // nil = time.Now
	Every time.Duration    // 0 = hourly
}

// Once removes every forgotten user whose time has come. Each one's mailboxes are stopped
// before their rows go. A user that fails does not keep the others from their turn; the
// failures are returned joined.
func (r Removal) Once(ctx context.Context) (removed int, err error) {
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	due, err := r.Store.DueForRemoval(ctx, now().Unix())
	if err != nil {
		return 0, err
	}
	var errs []error
	for _, f := range due {
		ok, err := r.remove(ctx, f, now().Unix())
		if err != nil {
			errs = append(errs, fmt.Errorf("user %d: %w", f.UserID, err))
		}
		if ok {
			removed++
		}
	}
	return removed, errors.Join(errs...)
}

// remove removes one forgotten user, and reports whether it did.
func (r Removal) remove(ctx context.Context, f store.Forgotten, now int64) (bool, error) {
	stopped, err := r.Store.RemovalAccounts(ctx, f.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, id := range stopped {
		r.Stop(id)
	}
	gone, err := r.Store.RemoveForgotten(ctx, f.UserID, now)
	// Whatever was stopped and still exists runs again: the person came back, the removal
	// failed, or someone joined the tenant meanwhile and it kept more than was stopped.
	defer r.restart(ctx, stopped)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	slog.InfoContext(ctx, "removed a forgotten user's data", "user_id", f.UserID, "tenant_id", gone.TenantID,
		"whole_tenant", gone.Tenant, "mailboxes", len(gone.Accounts), "rules_changed", len(gone.Rules))
	if len(gone.Rules) > 0 && r.Hub != nil {
		r.Hub.Publish(gone.TenantID, 0, events.RulesChanged, nil)
	}
	return true, nil
}

// restart starts again each of the accounts that still exists, is not paused and still has
// its secret (a forgotten person's mailboxes lost theirs when they were forgotten).
func (r Removal) restart(ctx context.Context, ids []int64) {
	for _, id := range ids {
		a, err := r.Store.Account(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			slog.ErrorContext(ctx, "could not restart a mailbox stopped for a removal", "account", id, "error", err.Error())
			continue
		}
		if a.Status != StatusPaused && !a.SecretGone {
			r.Start(a)
		}
	}
}

// Run runs the job now and then every Every, until ctx is done.
func (r Removal) Run(ctx context.Context) {
	tick := time.NewTicker(or(r.Every, time.Hour))
	defer tick.Stop()
	for {
		switch removed, err := r.Once(ctx); {
		case err != nil && ctx.Err() == nil:
			slog.ErrorContext(ctx, "the removal job failed", "error", err.Error())
		case removed > 0:
			slog.InfoContext(ctx, "removal", "users_removed", removed)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
