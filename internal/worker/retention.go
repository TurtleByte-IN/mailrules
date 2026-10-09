package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// Retention is the job that forgets: for each tenant, it blanks the snippets of messages
// older than that tenant's retention setting, and deletes the rows of messages older than
// store.KeepMessagesDays that have nothing left to undo.
type Retention struct {
	Store *store.Store
	Days  func(ctx context.Context, tenantID int64) int // the tenant's retention_days setting in force
	Now   func() time.Time                              // nil = time.Now
	Every time.Duration                                 // 0 = hourly
}

// Once runs the job one time over every tenant. A tenant that fails does not keep the
// others from their turn; the failures are returned joined.
func (r Retention) Once(ctx context.Context) (blanked, deleted int64, err error) {
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	tenants, err := r.Store.Tenants(ctx)
	if err != nil {
		return 0, 0, err
	}
	var errs []error
	for _, t := range tenants {
		b, d, err := r.Store.Retain(ctx, t, now().Unix(), r.Days(ctx, t))
		blanked, deleted = blanked+b, deleted+d
		if err != nil {
			errs = append(errs, fmt.Errorf("tenant %d: %w", t, err))
		}
	}
	return blanked, deleted, errors.Join(errs...)
}

// Run runs the job now and then every Every, until ctx is done.
func (r Retention) Run(ctx context.Context) {
	tick := time.NewTicker(or(r.Every, time.Hour))
	defer tick.Stop()
	for {
		switch blanked, deleted, err := r.Once(ctx); {
		case err != nil && ctx.Err() == nil:
			slog.ErrorContext(ctx, "the retention job failed", "error", err.Error())
		case blanked+deleted > 0:
			slog.InfoContext(ctx, "retention", "snippets_blanked", blanked, "messages_deleted", deleted)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
