package store

import (
	"context"
	"fmt"
	"slices"
)

// TrialReminderDays is how many days before a trial ends its owners are reminded of it.
const TrialReminderDays = 3

// TrialReminderTenants lists, by id, the tenants other than SelfHostTenant that are owed
// the trial-ending reminder by now: those that never bought a plan, whose trial ends within
// TrialReminderDays but has not ended, and that were not reminded yet (TrialReminded).
func (s *Store) TrialReminderTenants(ctx context.Context, now int64) ([]int64, error) {
	var out []int64
	err := s.eachTenant(ctx, tenantsAll, func(ctx context.Context, _ int64) error {
		rows, err := s.db.QueryContext(ctx, `SELECT t.id FROM tenants t LEFT JOIN plans p ON p.tenant_id = t.id
			WHERE t.id != ? AND p.tenant_id IS NULL AND t.trial_reminded_at IS NULL
			  AND t.created_at + ? <= ? AND t.created_at + ? > ?
			ORDER BY t.id`, SelfHostTenant, (TrialDays-TrialReminderDays)*day, now, TrialDays*day, now)
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
		return nil, fmt.Errorf("list trials to remind: %w", err)
	}
	slices.Sort(out)
	return out, nil
}

// TrialReminded records that the tenant's owners were sent the trial-ending reminder at
// now, so it is never sent again.
func (s *Store) TrialReminded(ctx context.Context, tenantID, now int64) error {
	ctx = WithTenant(ctx, tenantID)
	if _, err := s.db.ExecContext(ctx, `UPDATE tenants SET trial_reminded_at = ? WHERE id = ? AND trial_reminded_at IS NULL`,
		now, tenantID); err != nil {
		return fmt.Errorf("record trial reminder: %w", err)
	}
	return nil
}

// TenantOwnerEmails lists the email addresses of a tenant's owners (RoleOwner), oldest
// first, but those forgotten (ForgetIdentity) whose removal waits: who an email about
// choosing a plan goes to.
func (s *Store) TenantOwnerEmails(ctx context.Context, tenantID int64) ([]string, error) {
	ctx = WithTenant(ctx, tenantID)
	rows, err := s.db.QueryContext(ctx, `SELECT email FROM users
		WHERE tenant_id = ? AND role = ? AND email != '' AND id NOT IN (SELECT user_id FROM forgotten) ORDER BY id`,
		tenantID, RoleOwner)
	if err != nil {
		return nil, fmt.Errorf("list tenant owners: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, fmt.Errorf("list tenant owners: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tenant owners: %w", err)
	}
	return out, nil
}
