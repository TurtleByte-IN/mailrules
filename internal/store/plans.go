package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
)

// Plans, as a billing module reports them (SetPlan) and as TenantPlan sees them.
const (
	PlanTrial    = "trial" // no plan bought yet, within TrialDays of the tenant's creation
	PlanPersonal = "personal"
	PlanPro      = "pro"
	PlanEnded    = "ended" // the trial or the paid plan is over: nothing is sorted
)

// Billing intervals (Plan.Interval).
const (
	IntervalMonth = "month"
	IntervalYear  = "year"
)

// Plan statuses (Plan.Status).
const (
	StatusActive = "active"
	// StatusPastDue: a renewal is unpaid and the billing service retries it; the plan runs.
	StatusPastDue = "past_due"
	// StatusCancelsAtPeriodEnd: cancelled, running to PeriodEnd, when it ends.
	StatusCancelsAtPeriodEnd = "cancels_at_period_end"
)

const (
	// TrialDays is how long a tenant's trial runs from its creation.
	TrialDays = 14
	// EndedSecretsDays is how many days after a plan ends the tenant's mailbox passwords and
	// tokens and its model keys are deleted.
	EndedSecretsDays = 30
	// EndedRemovalDays is how many days after a plan ends everything else of the tenant is
	// removed.
	EndedRemovalDays = 90
)

// ErrBadPlan is what SetPlan returns for a plan, interval or status it does not know.
var ErrBadPlan = errors.New("unknown plan, interval or status")

// Plan is a tenant's plan as a billing module reports it.
type Plan struct {
	Plan      string // PlanPersonal, PlanPro or PlanEnded
	Interval  string // IntervalMonth, IntervalYear or empty
	Status    string // StatusActive, StatusPastDue, StatusCancelsAtPeriodEnd or empty
	PeriodEnd int64  // the end of the paid period, unix seconds; 0 = not known
	Record    string // the module's own record of the tenant (its billing ids); never shown
}

// PlanState is a tenant's plan as it stands at a moment (TenantPlan).
type PlanState struct {
	Plan
	// TrialEnd is when the tenant's trial ends (or ended): its creation plus TrialDays.
	TrialEnd int64
	// EndedAt is when the plan ended; 0 while one runs. A trial ends at TrialEnd, a plan
	// cancelled at its period's end at PeriodEnd.
	EndedAt int64
	// Warned is how many of the end-of-plan warning emails were sent; SecretsDeleted whether
	// the end-of-plan job deleted the tenant's passwords, tokens and model keys.
	Warned         int
	SecretsDeleted bool
}

// Ended reports whether the tenant's plan is over: nothing is sorted and the app is read-only.
func (p PlanState) Ended() bool { return p.EndedAt != 0 }

// TenantPlan returns the tenant's plan at now: the one the billing module last reported, or
// the trial while no plan was bought and TrialDays have not passed since the tenant was
// made, else ended. ErrNotFound for a tenant that does not exist.
func (s *Store) TenantPlan(ctx context.Context, tenantID, now int64) (PlanState, error) {
	return tenantPlan(ctx, s.db, tenantID, now)
}

// tenantPlan is TenantPlan on q, the database or a transaction.
func tenantPlan(ctx context.Context, q querier, tenantID, now int64) (PlanState, error) {
	var (
		created   int64
		plan      sql.NullString
		periodEnd sql.NullInt64
		endedAt   sql.NullInt64
		st        PlanState
	)
	err := q.QueryRowContext(ctx,
		`SELECT t.created_at, p.plan, COALESCE(p.interval, ''), COALESCE(p.status, ''), p.period_end, COALESCE(p.record, ''),
		        p.ended_at, COALESCE(p.warned, 0), COALESCE(p.secrets_deleted, 0)
		 FROM tenants t LEFT JOIN plans p ON p.tenant_id = t.id WHERE t.id = ?`, tenantID).
		Scan(&created, &plan, &st.Interval, &st.Status, &periodEnd, &st.Record, &endedAt, &st.Warned, &st.SecretsDeleted)
	if errors.Is(err, sql.ErrNoRows) {
		return PlanState{}, ErrNotFound
	}
	if err != nil {
		return PlanState{}, fmt.Errorf("read the tenant's plan: %w", err)
	}
	st.TrialEnd = created + TrialDays*day
	st.PeriodEnd = periodEnd.Int64
	switch {
	case !plan.Valid: // never bought: the trial, then ended
		st.Plan.Plan = PlanTrial
		if now >= st.TrialEnd {
			st.Plan.Plan, st.EndedAt = PlanEnded, st.TrialEnd
		}
	case plan.String == PlanEnded:
		st.Plan.Plan, st.EndedAt = PlanEnded, endedAt.Int64
		if st.EndedAt == 0 {
			st.EndedAt = now
		}
	case st.Status == StatusCancelsAtPeriodEnd && st.PeriodEnd != 0 && now >= st.PeriodEnd:
		st.Plan.Plan, st.EndedAt = PlanEnded, st.PeriodEnd
	default:
		st.Plan.Plan = plan.String
	}
	return st, nil
}

// SetPlan stores the tenant's plan as the billing module reports it, at now, and returns the
// state before and after. A plan that runs (personal or pro) clears what the end of an
// earlier one left: when it ended, the warnings sent, the secrets deleted. One that ends, or
// a report that leaves it ended (a cancelled plan whose period is over, reported again),
// keeps the time it really ended (the trial's end, or the paid period's) when that is
// earlier than now, and the end-of-plan job's progress. ErrBadPlan for a plan, interval or
// status it does not know, ErrNotFound for a tenant that does not exist.
func (s *Store) SetPlan(ctx context.Context, tenantID int64, p Plan, now int64) (before, after PlanState, err error) {
	if !slices.Contains([]string{PlanPersonal, PlanPro, PlanEnded}, p.Plan) ||
		!slices.Contains([]string{"", IntervalMonth, IntervalYear}, p.Interval) ||
		!slices.Contains([]string{"", StatusActive, StatusPastDue, StatusCancelsAtPeriodEnd}, p.Status) {
		return PlanState{}, PlanState{}, fmt.Errorf("set plan %q, interval %q, status %q: %w", p.Plan, p.Interval, p.Status, ErrBadPlan)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PlanState{}, PlanState{}, fmt.Errorf("set plan: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	if before, err = tenantPlan(ctx, tx, tenantID, now); err != nil {
		return PlanState{}, PlanState{}, err
	}
	stillEnded := p.Plan == PlanEnded || p.Status == StatusCancelsAtPeriodEnd && p.PeriodEnd != 0 && now >= p.PeriodEnd
	keep := stillEnded && before.Ended() // the end-of-plan job's progress stays
	var endedAt any                      // NULL: the plan runs, or ends at its period's end
	if p.Plan == PlanEnded {
		endedAt = now
		if before.Ended() {
			endedAt = before.EndedAt
		}
	}
	warned, deleted := 0, false
	if keep {
		warned, deleted = before.Warned, before.SecretsDeleted
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO plans (tenant_id, plan, interval, status, period_end, record, ended_at, warned, secrets_deleted, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (tenant_id) DO UPDATE SET plan = excluded.plan, interval = excluded.interval, status = excluded.status,
		   period_end = excluded.period_end, record = excluded.record, ended_at = excluded.ended_at, warned = excluded.warned,
		   warned_at = CASE WHEN ? THEN plans.warned_at ELSE NULL END,
		   secrets_deleted = excluded.secrets_deleted, updated_at = excluded.updated_at`,
		tenantID, p.Plan, p.Interval, p.Status, null(p.PeriodEnd), p.Record, endedAt, warned, deleted, now, keep); err != nil {
		return PlanState{}, PlanState{}, fmt.Errorf("set plan: %w", err)
	}
	if after, err = tenantPlan(ctx, tx, tenantID, now); err != nil {
		return PlanState{}, PlanState{}, err
	}
	if err := tx.Commit(); err != nil {
		return PlanState{}, PlanState{}, fmt.Errorf("set plan: %w", err)
	}
	return before, after, nil
}
