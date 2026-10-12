package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
)

// What a mailbox shows once the end-of-plan job deleted its secret (DeleteTenantSecrets). A
// mailbox a module connected (oauth or module) shows planEndedOAuthMessage and
// reconnect_needed instead of auth_failed.
const (
	planEndedMessage      = "Your MailRules plan ended, so this mailbox's password was deleted. Subscribe on Plan and billing, then enter a new app password to sort again."
	planEndedOAuthMessage = "Your MailRules plan ended, so this mailbox's sign-in was deleted. Subscribe on Plan and billing, then reconnect to sort again."
)

// ErrPlanNotEnded is what the end-of-plan steps return, changing nothing, for a tenant whose
// plan no longer ended when the caller saw it end: one bought again meanwhile, or one that
// ended again at another time.
var ErrPlanNotEnded = errors.New("the tenant's plan has not ended")

// EndedPlan is a tenant's plan as the end-of-plan job sees it: TenantPlan's state, and
// when the latest warning email was sent.
type EndedPlan struct {
	PlanState
	// WarnedAt is when warning number Warned was sent (PlanWarned); 0 while none was.
	WarnedAt int64
}

// PlanEndTenants lists, by id, the tenants other than SelfHostTenant whose plan may have
// ended by now: those that never bought one and whose trial is over, those the billing
// module or the job recorded as ended, and those cancelled at a period's end that has
// passed. It is where the end-of-plan job looks; whether each one ended is TenantPlan's to
// say (EndedPlan).
func (s *Store) PlanEndTenants(ctx context.Context, now int64) ([]int64, error) {
	var out []int64
	err := s.eachTenant(ctx, tenantsAll, func(ctx context.Context, _ int64) error {
		rows, err := s.db.QueryContext(ctx, `SELECT t.id FROM tenants t LEFT JOIN plans p ON p.tenant_id = t.id
			WHERE t.id != ? AND ((p.tenant_id IS NULL AND t.created_at + ? <= ?) OR p.plan = ?
			  OR (p.status = ? AND p.period_end IS NOT NULL AND p.period_end <= ?))
			ORDER BY t.id`, SelfHostTenant, TrialDays*day, now, PlanEnded, StatusCancelsAtPeriodEnd, now)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out = append(out, id)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list ended plans: %w", err)
	}
	slices.Sort(out)
	return out, nil
}

// EndedPlan returns the tenant's plan at now (TenantPlan) with when its latest warning
// was sent. ErrNotFound for a tenant that does not exist.
func (s *Store) EndedPlan(ctx context.Context, tenantID, now int64) (EndedPlan, error) {
	ctx = WithTenant(ctx, tenantID)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return EndedPlan{}, fmt.Errorf("read the tenant's plan: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	st, err := tenantPlan(ctx, tx, tenantID, now)
	if err != nil {
		return EndedPlan{}, err
	}
	out := EndedPlan{PlanState: st}
	if st.Warned > 0 {
		var at sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT warned_at FROM plans WHERE tenant_id = ?`, tenantID).Scan(&at)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return EndedPlan{}, fmt.Errorf("read the tenant's plan: %w", err)
		}
		out.WarnedAt = at.Int64
	}
	return out, nil
}

// EndTrial records, at now, the end of a trial over with no plan bought: a plans row with
// plan ended at the trial's end and no warning sent yet, so the end-of-plan job has where
// to keep its steps. It reports whether it wrote one; a tenant with a row already, whose
// trial runs, or the self-host tenant (which has no plan) is left as it is.
func (s *Store) EndTrial(ctx context.Context, tenantID, now int64) (bool, error) {
	if tenantID == SelfHostTenant {
		return false, nil
	}
	ctx = WithTenant(ctx, tenantID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("end trial: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	st, err := tenantPlan(ctx, tx, tenantID, now)
	if err != nil {
		return false, err
	}
	if !st.Ended() || st.EndedAt != st.TrialEnd {
		return false, nil
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO plans (tenant_id, plan, ended_at, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (tenant_id) DO NOTHING`, tenantID, PlanEnded, st.EndedAt, now)
	if err != nil {
		return false, fmt.Errorf("end trial: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("end trial: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// PlanWarned records that warning n (1 to 3) about the plan that ended at endedAt was sent
// at now. A lower or equal number than the one recorded changes nothing. ErrPlanNotEnded,
// changing nothing, when the plan no longer ended at endedAt. Never the self-host tenant.
func (s *Store) PlanWarned(ctx context.Context, tenantID, endedAt int64, n int, now int64) error {
	if n < 1 || n > 3 {
		return fmt.Errorf("record plan warning %d: there are 3", n)
	}
	return s.endedStep(WithTenant(ctx, tenantID), tenantID, endedAt, now, 0, "record plan warning", func(tx *txn) error {
		// No row yet is a trial that ended: the row EndTrial would write, with the warning.
		_, err := tx.ExecContext(ctx, `INSERT INTO plans (tenant_id, plan, ended_at, warned, warned_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (tenant_id) DO UPDATE SET warned = excluded.warned, warned_at = excluded.warned_at,
			  updated_at = excluded.updated_at
			WHERE plans.warned < excluded.warned`, tenantID, PlanEnded, endedAt, n, now, now)
		return err
	})
}

// DeleteTenantSecrets deletes, in one transaction, the secrets of a tenant whose plan ended
// at endedAt, once EndedSecretsDays have passed since by now: every mailbox's password or
// token (as ForgetIdentity deletes an only member's: each one a module connected is queued
// for it first, and they show auth_failed, or reconnect_needed, saying the plan ended), the
// tenant's stored model API keys, and its notification destinations' addresses and signing
// secrets; the plan records it (PlanState.SecretsDeleted). It returns the tenant's
// mailboxes, whose supervisors the caller stops before and after. Everything else stays,
// readable. ErrPlanNotEnded, changing nothing, when the plan no longer ended at endedAt or
// not long enough ago. Never the self-host tenant.
func (s *Store) DeleteTenantSecrets(ctx context.Context, tenantID, endedAt, now int64) ([]int64, error) {
	var ids []int64
	ctx = WithTenant(ctx, tenantID)
	err := s.endedStep(ctx, tenantID, endedAt, now, EndedSecretsDays, "delete the tenant's secrets", func(tx *txn) error {
		var err error
		if ids, err = tenantAccountIDs(ctx, tx, tenantID); err != nil {
			return err
		}
		if err := deleteSecrets(ctx, tx, tenantID, ids, true, planEndedMessage, planEndedOAuthMessage, now); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO plans (tenant_id, plan, ended_at, secrets_deleted, updated_at)
			VALUES (?, ?, ?, TRUE, ?)
			ON CONFLICT (tenant_id) DO UPDATE SET secrets_deleted = TRUE, updated_at = excluded.updated_at`,
			tenantID, PlanEnded, endedAt, now)
		return err
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// RemoveEndedTenant removes, in one transaction, a tenant whose plan ended at endedAt, once
// EndedRemovalDays have passed since by now: everything RemoveForgotten removes with a
// tenant's only member, for every member, the plan too. A mailbox a module connected that
// still has its secret is queued for that module first. It returns what went; the caller
// stops the mailboxes before. ErrPlanNotEnded, changing nothing, when the plan no longer
// ended at endedAt or not long enough ago. Never the self-host tenant.
func (s *Store) RemoveEndedTenant(ctx context.Context, tenantID, endedAt, now int64) (Removal, error) {
	out := Removal{TenantID: tenantID, Tenant: true}
	ctx = WithTenant(ctx, tenantID)
	err := s.endedStep(ctx, tenantID, endedAt, now, EndedRemovalDays, "remove the tenant", func(tx *txn) error {
		var err error
		if out.Accounts, err = tenantAccountIDs(ctx, tx, tenantID); err != nil {
			return err
		}
		// Their secrets went after EndedSecretsDays, so normally none is queued here.
		if err := queueRemovedMailboxes(ctx, tx, out.Accounts, now); err != nil {
			return err
		}
		return removeTenant(ctx, tx, tenantID)
	})
	if err != nil {
		return Removal{}, err
	}
	return out, nil
}

// endedStep runs step in a transaction when the tenant's plan, read in it, ended at endedAt
// and at least days ago by now; else it answers ErrPlanNotEnded and changes nothing. A plan
// bought again between the job's look and its step so keeps everything. The self-host
// tenant has no plan, so no step is ever taken for it.
func (s *Store) endedStep(ctx context.Context, tenantID, endedAt, now int64, days int64, what string, step func(*txn) error) error {
	if tenantID == SelfHostTenant {
		return fmt.Errorf("%s %d: the self-host tenant has no plan", what, tenantID)
	}
	ctx = WithTenant(ctx, tenantID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	st, err := tenantPlan(ctx, tx, tenantID, now)
	if err != nil {
		return err
	}
	if !st.Ended() || st.EndedAt != endedAt || now < endedAt+days*day {
		return fmt.Errorf("%s %d: %w", what, tenantID, ErrPlanNotEnded)
	}
	if err := step(tx); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// tenantAccountIDs lists, in tx, the tenant's mailboxes.
func tenantAccountIDs(ctx context.Context, tx *txn, tenantID int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM accounts WHERE tenant_id = ? ORDER BY id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list mailboxes: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list mailboxes: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// TenantEmails lists the email addresses of a tenant's members, oldest member first, but
// those forgotten (ForgetIdentity) whose removal waits: who an email about the tenant's
// plan goes to.
func (s *Store) TenantEmails(ctx context.Context, tenantID int64) ([]string, error) {
	ctx = WithTenant(ctx, tenantID)
	rows, err := s.db.QueryContext(ctx, `SELECT email FROM users
		WHERE tenant_id = ? AND email != '' AND id NOT IN (SELECT user_id FROM forgotten) ORDER BY id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list tenant emails: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, fmt.Errorf("list tenant emails: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tenant emails: %w", err)
	}
	return out, nil
}
