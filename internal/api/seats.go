package api

import (
	"context"
	"errors"
	"time"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// The seats of a plan sold by the seat follow the team (MAI-247). A seat is a member or an
// invitation waiting (store.SeatsUsed). An invitation past the paid seats buys the seats the
// team then needs at once, before it is kept (handleInvitationCreate); a seat freed, by a
// member who left or an invitation revoked, withdrawn or expired, is given back by the seat
// worker (LowerSeats), never below the plan's least. Joining without an invitation buys
// none: it is refused while every seat is taken.

// perSeat returns the terms of the tenant's plan in p when the daemon may change its seats:
// a plan sold by the seat that the tenant subscribed to and that has not ended.
func perSeat(b *ext.Billing, p store.PlanState) (ext.PlanTerms, bool) {
	if b == nil || p.Ended() || p.Record == "" || p.Seats <= 0 {
		return ext.PlanTerms{}, false
	}
	t := b.Plans[p.Plan.Plan]
	return t, t.PerSeat
}

// seatsAtLeast is the build's seat limit (seatLimit), but at least seats: the seats the
// billing module was just asked for, which the plan has before the module reports them.
func (s *server) seatsAtLeast(seats int) store.SeatLimit {
	limit := s.seatLimit()
	if limit == nil || seats == 0 {
		return limit
	}
	return func(p store.PlanState) int {
		if l := limit(p); l < 0 || l > seats {
			return l
		}
		return seats
	}
}

// seatsNeeded is how many seats a tenant in p whose invitation was refused by full asks the
// billing module for, to keep it: one more than it uses, at least the plan's least. 0 when
// its plan cannot gain seats that way: not sold by the seat, not subscribed, or at maxSeats.
func seatsNeeded(b *ext.Billing, p store.PlanState, full *store.SeatLimitError) int {
	t, ok := perSeat(b, p)
	need := max(full.Used+1, minSeats(t))
	if !ok || need > maxSeats {
		return 0
	}
	return need
}

// LowerSeats is, for the seat worker, how a tenant's paid seats come down once a seat is
// freed (store.SeatCheck): when its plan is sold by the seat, it asks the billing module for
// the seats the tenant uses, never fewer than the plan's least. It asks even when that is
// the number the daemon last heard of: seats bought for an invitation moments ago may not
// be reported yet (Host.SetPlan), and asking for the number the subscription has already
// changes nothing (ext.Billing.ChangePlan), so asking twice is safe. Nil in a build that
// sells no plans.
func LowerSeats(st *store.Store, modules []ext.Module) func(ctx context.Context, tenantID int64, now time.Time) error {
	b := billingOf(modules)
	if b == nil {
		return nil
	}
	return func(ctx context.Context, tenantID int64, now time.Time) error {
		p, err := st.TenantPlan(ctx, tenantID, now.Unix())
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		t, ok := perSeat(b, p)
		if !ok {
			return nil
		}
		used, err := st.SeatsUsed(ctx, tenantID, now.Unix())
		if err != nil {
			return err
		}
		target := max(minSeats(t), used)
		return b.ChangePlan(ctx, ext.PlanChange{TenantID: tenantID, Record: p.Record, Plan: p.Plan.Plan, Interval: p.Interval, Seats: target})
	}
}
