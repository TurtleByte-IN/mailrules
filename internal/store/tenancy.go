package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
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
// inVisible, which is built from it). The viewer is bound by name, @viewer_tenant and
// @viewer_user, so the condition fits a query wherever it numbers its own parameters: pass
// v.tenantArg() and v.userArg() with the query's arguments.
func visibleAccounts(alias string) string {
	return "(" + alias + ".tenant_id = @viewer_tenant AND (" + alias + ".user_id = @viewer_user OR " + alias + ".shared = TRUE))"
}

// inVisible is the condition that the account id in column is one the viewer sees; it binds
// the viewer as visibleAccounts does.
func inVisible(column string) string {
	return column + " IN (SELECT va.id FROM accounts va WHERE " + visibleAccounts("va") + ")"
}

// tenantArg binds the viewer's tenant as @viewer_tenant.
func (v Viewer) tenantArg() sql.NamedArg { return sql.Named("viewer_tenant", v.TenantID) }

// userArg binds the viewer as @viewer_user.
func (v Viewer) userArg() sql.NamedArg { return sql.Named("viewer_user", v.UserID) }

// CreateTenant adds a tenant for a sign-in service's organisation and returns its id.
func (s *Store) CreateTenant(ctx context.Context, provider, externalID string, now int64) (int64, error) {
	id, err := s.createTenant(ctx, s.db, provider, externalID, now)
	if err != nil {
		return 0, fmt.Errorf("create tenant: %w", err)
	}
	return id, nil
}

// CreateUser adds a user to a tenant: its owner when it has nobody yet, else a member. An
// empty passwordHash makes a user who cannot sign in with a password (one made from a
// sign-in service's identity).
func (s *Store) CreateUser(ctx context.Context, tenantID int64, email, passwordHash string, now int64) (User, error) {
	ctx = WithTenant(ctx, tenantID)
	u := User{Email: email, PasswordHash: passwordHash, CreatedAt: now, TenantID: tenantID}
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO users (email, password_hash, created_at, tenant_id, role)
		 SELECT CAST(?1 AS TEXT), CAST(?2 AS TEXT), CAST(?3 AS BIGINT), id,
		        CASE WHEN EXISTS (SELECT 1 FROM users WHERE tenant_id = ?4) THEN 'member' ELSE 'owner' END
		 FROM tenants WHERE id = ?4 RETURNING id, role`,
		email, passwordHash, now, tenantID).Scan(&u.ID, &u.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, fmt.Errorf("create user: tenant %d: %w", tenantID, ErrNotFound)
	}
	if err != nil {
		return User{}, fmt.Errorf("create user: %w", err)
	}
	return u, nil
}

// TenantMembers counts the users in a tenant.
func (s *Store) TenantMembers(ctx context.Context, tenantID int64) (int, error) {
	ctx = WithTenant(ctx, tenantID)
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE tenant_id = ?`, tenantID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count tenant members: %w", err)
	}
	return n, nil
}

// Tenants lists every tenant's id, oldest first: what per-tenant jobs (retention) walk.
// Within a tenant's scope it is that tenant alone.
func (s *Store) Tenants(ctx context.Context) ([]int64, error) {
	query := `SELECT id FROM tenants ORDER BY id`
	if s.db.d == dialectPostgres && TenantOf(ctx) == 0 {
		query = `SELECT t FROM mailrules_tenants('all') AS t ORDER BY t`
	}
	rows, err := s.db.QueryContext(ctx, query)
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
	var out []User
	err := s.eachTenant(ctx, tenantsAll, func(ctx context.Context, _ int64) error {
		rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+` FROM users WHERE id NOT IN (SELECT user_id FROM forgotten) ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			u, err := scanUser(rows)
			if err != nil {
				return err
			}
			out = append(out, u)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	slices.SortStableFunc(out, func(a, b User) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

// User returns one user, or ErrNotFound.
func (s *Store) User(ctx context.Context, id int64) (User, error) {
	ctx = s.tenantFor(ctx, ofUser, id)
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
	ctx = s.tenantFor(ctx, ofAccount, accountID)
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
	ctx = WithTenant(ctx, v.TenantID)
	var ok bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM accounts a WHERE a.id = ? AND `+visibleAccounts("a")+`)`, accountID, v.tenantArg(), v.userArg()).Scan(&ok); err != nil {
		return false, fmt.Errorf("check account visibility: %w", err)
	}
	return ok, nil
}

// TenantOrganisation is the sign-in service that made the tenant and its id of the
// tenant's organisation there; both empty for the self-host tenant. ErrNotFound for a
// tenant that does not exist.
func (s *Store) TenantOrganisation(ctx context.Context, tenantID int64) (provider, organisation string, err error) {
	ctx = WithTenant(ctx, tenantID)
	var p, o sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT provider, external_id FROM tenants WHERE id = ?`, tenantID).Scan(&p, &o)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("read the tenant's organisation: %w", err)
	}
	return p.String, o.String, nil
}
