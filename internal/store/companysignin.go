package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// CheckCompanySignIn records whether the tenant may use company sign-in now (allowed, as the
// daemon computes it from the build's terms), and returns when it stops for a tenant that
// lost it (PlanState.CompanySignInEndsAt): 0 while allowed. The moment a tenant that held
// it is found without it, lossEnd (the end of the paid period in force then, or the day the
// plan ended) is saved, and it stays as it is however often the plan renews or this is
// called; allowed again clears it. It writes only when that changes. A tenant with no plan
// stored (the trial, or one gone) has none: 0.
func (s *Store) CheckCompanySignIn(ctx context.Context, tenantID int64, allowed bool, lossEnd int64) (int64, error) {
	ctx = WithTenant(ctx, tenantID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("check company sign-in: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	var (
		held bool
		ends sql.NullInt64
	)
	err = tx.QueryRowContext(ctx, `SELECT company_sign_in_held, company_sign_in_ends_at FROM plans WHERE tenant_id = ?`, tenantID).
		Scan(&held, &ends)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("check company sign-in: %w", err)
	}
	var newEnds any // NULL
	switch {
	case allowed && held && !ends.Valid:
		return 0, nil
	case allowed:
	case !held:
		return ends.Int64, nil
	default: // lost just now
		newEnds = lossEnd
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plans SET company_sign_in_held = ?, company_sign_in_ends_at = ? WHERE tenant_id = ?`,
		allowed, newEnds, tenantID); err != nil {
		return 0, fmt.Errorf("check company sign-in: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("check company sign-in: %w", err)
	}
	if allowed {
		return 0, nil
	}
	return lossEnd, nil
}
