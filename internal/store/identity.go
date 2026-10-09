package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var (
	// ErrTenantMismatch is a returning identity that arrived under a different organisation
	// than the one it signed up with.
	ErrTenantMismatch = errors.New("identity belongs to another tenant")
	// ErrEmailInUse is an email another user already has.
	ErrEmailInUse = errors.New("email in use by another user")
)

// Identity is a person as a sign-in service names them: Subject is the service's stable id
// for them and Tenant the id of the organisation they signed in under.
type Identity struct {
	Provider, Subject, Email, Tenant string
}

// SignInIdentity finds the user an identity belongs to, or makes one, in one transaction.
// The tenant is the one of (Provider, Tenant), made when there is none yet. A new identity
// gets a user in that tenant who cannot sign in with a password; a known one keeps its user,
// whose email becomes id.Email. It fails, changing nothing, with ErrTenantMismatch when a
// known identity's user is in another tenant, and with ErrEmailInUse when another user has
// the email.
func (s *Store) SignInIdentity(ctx context.Context, id Identity, now int64) (User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, fmt.Errorf("sign in identity: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var tenantID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM tenants WHERE provider = ? AND external_id = ?`, id.Provider, id.Tenant).Scan(&tenantID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return User{}, fmt.Errorf("sign in identity: find tenant: %w", err)
	}
	u, err := scanUser(tx.QueryRowContext(ctx, `SELECT u.id, u.email, u.password_hash, u.created_at, u.tenant_id
		FROM identities i JOIN users u ON u.id = i.user_id
		WHERE i.provider = ? AND i.subject = ?`, id.Provider, id.Subject))
	known := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return User{}, fmt.Errorf("sign in identity: find user: %w", err)
	}
	if known && u.TenantID != tenantID {
		return User{}, ErrTenantMismatch
	}

	var other int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE email = ? COLLATE NOCASE AND id != ?)`,
		id.Email, u.ID).Scan(&other); err != nil {
		return User{}, fmt.Errorf("sign in identity: check email: %w", err)
	}
	if other == 1 {
		return User{}, ErrEmailInUse
	}

	if known {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET email = ? WHERE id = ?`, id.Email, u.ID); err != nil {
			return User{}, fmt.Errorf("sign in identity: update email: %w", err)
		}
		u.Email = id.Email
	} else {
		if tenantID == 0 {
			res, err := tx.ExecContext(ctx, `INSERT INTO tenants (provider, external_id, created_at) VALUES (?, ?, ?)`, id.Provider, id.Tenant, now)
			if err != nil {
				return User{}, fmt.Errorf("sign in identity: create tenant: %w", err)
			}
			tenantID, _ = res.LastInsertId()
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO users (email, password_hash, created_at, tenant_id) VALUES (?, '', ?, ?)`, id.Email, now, tenantID)
		if err != nil {
			return User{}, fmt.Errorf("sign in identity: create user: %w", err)
		}
		uid, _ := res.LastInsertId()
		if _, err := tx.ExecContext(ctx, `INSERT INTO identities (provider, subject, user_id) VALUES (?, ?, ?)`, id.Provider, id.Subject, uid); err != nil {
			return User{}, fmt.Errorf("sign in identity: create identity: %w", err)
		}
		u = User{ID: uid, Email: id.Email, CreatedAt: now, TenantID: tenantID}
	}
	if err := tx.Commit(); err != nil {
		return User{}, fmt.Errorf("sign in identity: %w", err)
	}
	return u, nil
}

// EndIdentitySessions deletes every session of the identity's user. An identity nobody
// has is not an error.
func (s *Store) EndIdentitySessions(ctx context.Context, provider, subject string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE user_id IN (SELECT user_id FROM identities WHERE provider = ? AND subject = ?)`,
		provider, subject); err != nil {
		return fmt.Errorf("end identity sessions: %w", err)
	}
	return nil
}
