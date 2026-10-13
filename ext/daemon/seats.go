package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/store"
)

const (
	// seatsEvery is how often the worker looks for seats to give back: the billing module
	// hears of a freed seat within about this long.
	seatsEvery = 5 * time.Second
	// seatsBatch is how many tenants one look handles at most.
	seatsBatch = 50
	// seatsTimeout bounds one call to the billing module.
	seatsTimeout = 30 * time.Second
)

// seatWorker gives back the seats a team freed (store.SeatCheck): for each check queued, it
// has the billing module lower the tenant's paid seats to those it uses, never below the
// plan's least (api.LowerSeats), at least once: a check stays queued, across restarts, until
// the module agrees. In a build that sells no plans (lower is nil) a check is dropped.
type seatWorker struct {
	store *store.Store
	lower func(ctx context.Context, tenantID int64, now time.Time) error
	every time.Duration    // 0 = seatsEvery
	now   func() time.Time // nil = time.Now
}

func (w *seatWorker) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

// Run handles what is waiting now, and then every few seconds, until ctx is done.
func (w *seatWorker) Run(ctx context.Context) {
	tick := time.NewTicker(cmp.Or(w.every, seatsEvery))
	defer tick.Stop()
	for {
		if err := w.once(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "giving back freed seats failed", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// once handles the checks due, the longest due first. One the module refuses does not hold
// up the rest; the errors are returned joined.
func (w *seatWorker) once(ctx context.Context) error {
	waiting, err := w.store.WaitingSeatChecks(ctx, w.clock().Unix(), seatsBatch)
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range waiting {
		if ctx.Err() != nil {
			break
		}
		if err := w.check(ctx, c); err != nil {
			errs = append(errs, fmt.Errorf("seat check of tenant %d: %w", c.TenantID, err))
		}
	}
	return errors.Join(errs...)
}

// check lowers one tenant's seats, then deletes the check; a refusal waits to be tried again.
func (w *seatWorker) check(ctx context.Context, c store.SeatCheck) error {
	if w.lower != nil {
		callCtx, cancel := context.WithTimeout(ctx, seatsTimeout)
		err := w.lower(callCtx, c.TenantID, w.clock())
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil // stopping: it is handled again after the restart
			}
			slog.WarnContext(ctx, "the billing module did not give back a freed seat; it is asked again later",
				"tenant_id", c.TenantID, "attempts", c.Attempts+1, "error", err.Error())
			return w.store.SeatCheckRetry(ctx, c, w.clock().Add(retryAfter(c.Attempts)).Unix())
		}
	}
	return w.store.SeatCheckDone(context.WithoutCancel(ctx), c)
}
