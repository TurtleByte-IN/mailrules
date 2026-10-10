package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// SelfHostTenant is the one tenant of a self-hosted install, made by migration 0011.
const SelfHostTenant int64 = 1

// NewAccountShared is whether a mailbox someone adds starts shared with everyone in their
// tenant. It starts private: only its owner sees it, its mail and what MailRules did to it,
// until the owner shares it.
const NewAccountShared = false

// Viewer is the signed-in person a user-facing read or change is made for. They see an
// account when it is in their tenant and they own it or it is shared (visibleAccounts), and
// through it its messages, decisions, actions, corrections, contacts, folders, batches,
// stats, activity and Needs review queue. Rules, sender rules, settings and the usage
// ledger belong to the tenant.
type Viewer struct {
	UserID, TenantID int64
}

// Viewer is the user as the one who sees through it.
func (u User) Viewer() Viewer { return Viewer{UserID: u.ID, TenantID: u.TenantID} }

// Owner is the account's owner as a viewer: who internal work on one account (the
// pipeline, the learner) acts for.
func (a Account) Owner() Viewer { return Viewer{UserID: a.UserID, TenantID: a.TenantID} }

// visibleAccounts is the one rule of who sees which mailbox, as an SQL condition on the
// accounts row aliased alias: in the viewer's tenant, and theirs or shared. Every
// user-facing query that touches accounts or what hangs off them is built from it (or from
// inVisible, which is built from it). The ids are written into the SQL as numbers rather
// than bound, so the condition fits queries that number their own parameters (?1, ?2);
// an int64 printed in base 10 cannot carry anything but digits and a sign.
func visibleAccounts(alias string, v Viewer) string {
	return "(" + alias + ".tenant_id = " + strconv.FormatInt(v.TenantID, 10) +
		" AND (" + alias + ".user_id = " + strconv.FormatInt(v.UserID, 10) + " OR " + alias + ".shared = 1))"
}

// inVisible is the condition that the account id in column is one the viewer sees.
func inVisible(column string, v Viewer) string {
	return column + " IN (SELECT va.id FROM accounts va WHERE " + visibleAccounts("va", v) + ")"
}

// CreateTenant adds a tenant for a sign-in service's organisation and returns its id.
func (s *Store) CreateTenant(ctx context.Context, provider, externalID string, now int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO tenants (provider, external_id, created_at) VALUES (?, ?, ?)`,
		null(provider), null(externalID), now)
	if err != nil {
		return 0, fmt.Errorf("create tenant: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// CreateUser adds a user to a tenant. An empty passwordHash makes a user who cannot sign
// in with a password (one made from a sign-in service's identity).
func (s *Store) CreateUser(ctx context.Context, tenantID int64, email, passwordHash string, now int64) (User, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (email, password_hash, created_at, tenant_id) SELECT ?, ?, ?, id FROM tenants WHERE id = ?`,
		email, passwordHash, now, tenantID)
	if err != nil {
		return User{}, fmt.Errorf("create user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return User{}, fmt.Errorf("create user: tenant %d: %w", tenantID, ErrNotFound)
	}
	id, _ := res.LastInsertId()
	return User{ID: id, Email: email, PasswordHash: passwordHash, CreatedAt: now, TenantID: tenantID}, nil
}

// TenantMembers counts the users in a tenant.
func (s *Store) TenantMembers(ctx context.Context, tenantID int64) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE tenant_id = ?`, tenantID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count tenant members: %w", err)
	}
	return n, nil
}

// Tenants lists every tenant's id, oldest first: what per-tenant jobs (retention) walk.
func (s *Store) Tenants(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM tenants ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list tenants: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	return out, nil
}

// Users lists every user, oldest first, but those forgotten (ForgetIdentity) whose removal
// waits: who the summary email is checked for.
func (s *Store) Users(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+` FROM users WHERE id NOT IN (SELECT user_id FROM forgotten) ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("list users: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return out, nil
}

// User returns one user, or ErrNotFound.
func (s *Store) User(ctx context.Context, id int64) (User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}

// AccountTenant returns the tenant an account belongs to, or ErrNotFound.
func (s *Store) AccountTenant(ctx context.Context, accountID int64) (int64, error) {
	var t int64
	err := s.db.QueryRowContext(ctx, `SELECT tenant_id FROM accounts WHERE id = ?`, accountID).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("get account tenant: %w", err)
	}
	return t, nil
}

// CanSee reports whether the viewer sees the account: the visibility rule for one id.
func (s *Store) CanSee(ctx context.Context, v Viewer, accountID int64) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM accounts a WHERE a.id = ? AND `+visibleAccounts("a", v)+`)`, accountID).Scan(&n); err != nil {
		return false, fmt.Errorf("check account visibility: %w", err)
	}
	return n == 1, nil
}
