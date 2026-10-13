package store

import (
	"context"
	"fmt"
)

// Seat checks (MAI-247). The paid seats of a plan sold by the seat follow the team: an
// invitation past them buys one more at once (the API asks the billing module before it
// keeps the invitation), and a seat freed (a member who left, an invitation revoked,
// withdrawn or expired) is given back by the seat worker, which asks the billing module for
// the seats the tenant then uses, never fewer than the plan's least. The transaction that
// frees the seat queues the check, so a restart loses none, and the worker keeps it until
// the module agrees: at least once, and asking for a number of seats rather than one fewer,
// so asking twice changes nothing.

// SeatCheck is a tenant whose paid seats may be more than it uses (WaitingSeatChecks).
type SeatCheck struct {
	TenantID  int64
	Version   int64 // goes up each time it is queued again
	CreatedAt int64 // when it was first queued
	Attempts  int   // the billing module's refusals so far
}

// queueSeatCheck queues, with q, a check of the tenant's seats, due at now, when its plan is
// sold by the seat (it has seats). One queued already is due at now again, and counts as new.
func queueSeatCheck(ctx context.Context, q querier, tenantID, now int64) error {
	if _, err := q.ExecContext(ctx, `INSERT INTO seat_checks (tenant_id, created_at, next_at)
		SELECT tenant_id, CAST(? AS BIGINT), CAST(? AS BIGINT) FROM plans WHERE tenant_id = ? AND seats > 0
		ON CONFLICT (tenant_id) DO UPDATE SET version = seat_checks.version + 1, attempts = 0, next_at = excluded.next_at`,
		now, now, tenantID); err != nil {
		return fmt.Errorf("queue a seat check: %w", err)
	}
	return nil
}

// QueueSeatCheck queues a check of the tenant's seats, due at now: for a seat bought for an
// invitation that was then not kept.
func (s *Store) QueueSeatCheck(ctx context.Context, tenantID, now int64) error {
	return queueSeatCheck(WithTenant(ctx, tenantID), s.db, tenantID, now)
}

// WaitingSeatChecks lists at most limit seat checks due by now, the longest due first.
func (s *Store) WaitingSeatChecks(ctx context.Context, now int64, limit int) ([]SeatCheck, error) {
	var waiting []outboxRow[SeatCheck]
	err := s.eachTenant(ctx, tenantsSeats, func(ctx context.Context, _ int64) error {
		rows, err := s.db.QueryContext(ctx, `SELECT tenant_id, version, created_at, attempts, next_at FROM seat_checks
			WHERE next_at <= ? ORDER BY next_at, tenant_id LIMIT ?`, now, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c SeatCheck
			var next int64
			if err := rows.Scan(&c.TenantID, &c.Version, &c.CreatedAt, &c.Attempts, &next); err != nil {
				return err
			}
			waiting = append(waiting, outboxRow[SeatCheck]{row: c, next: next, id: c.TenantID})
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list seat checks: %w", err)
	}
	return firstDue(waiting, limit), nil
}

// SeatCheckDone deletes a seat check the worker handled, unless it was queued again since it
// was listed (c.Version): that one waits to be handled in turn.
func (s *Store) SeatCheckDone(ctx context.Context, c SeatCheck) error {
	ctx = WithTenant(ctx, c.TenantID)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM seat_checks WHERE tenant_id = ? AND version = ?`, c.TenantID, c.Version); err != nil {
		return fmt.Errorf("delete seat check: %w", err)
	}
	return nil
}

// SeatCheckRetry records that the billing module did not agree to a seat check: it is
// handled again at next, unless it was queued again since it was listed (it is due then).
func (s *Store) SeatCheckRetry(ctx context.Context, c SeatCheck, next int64) error {
	ctx = WithTenant(ctx, c.TenantID)
	if _, err := s.db.ExecContext(ctx, `UPDATE seat_checks SET attempts = attempts + 1, next_at = ? WHERE tenant_id = ? AND version = ?`,
		next, c.TenantID, c.Version); err != nil {
		return fmt.Errorf("retry seat check: %w", err)
	}
	return nil
}
