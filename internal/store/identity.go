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
// whose email becomes id.Email, and a known one that was forgotten (ForgetIdentity) has its
// removal called off. One whose person deleted their own account is no longer known
// (DeleteOwnAccount), so it gets a new user. It fails, changing nothing, with
// ErrTenantMismatch when a known identity's user is in another tenant, and with
// ErrEmailInUse when another user has the email. A new tenant gets its data key at once,
// wrapped by the store's key provider (when it has one): a provider that cannot answer
// fails the sign-in with crypto.ErrKeyUnavailable, and no tenant is made.
//
// It runs before anyone is signed in, so on Postgres it finds the tenants through the
// definer functions (scope.go) and scopes its transaction once it knows the tenant.
func (s *Store) SignInIdentity(ctx context.Context, id Identity, now int64) (User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, fmt.Errorf("sign in identity: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	tenantID, err := s.findTenant(ctx, tx, ofExternal, id.Provider, id.Tenant)
	if err != nil {
		return User{}, fmt.Errorf("sign in identity: find tenant: %w", err)
	}
	userTenant, err := s.findTenant(ctx, tx, ofIdentity, id.Provider, id.Subject)
	if err != nil {
		return User{}, fmt.Errorf("sign in identity: find user: %w", err)
	}
	known := userTenant != 0
	if known && userTenant != tenantID {
		return User{}, ErrTenantMismatch
	}
	var u User
	if known {
		if err := tx.scope(ctx, tenantID, true); err != nil {
			return User{}, fmt.Errorf("sign in identity: %w", err)
		}
		if u, err = scanUser(tx.QueryRowContext(ctx, `SELECT u.id, u.email, u.password_hash, u.created_at, u.tenant_id
			FROM identities i JOIN users u ON u.id = i.user_id
			WHERE i.provider = ? AND i.subject = ?`, id.Provider, id.Subject)); err != nil {
			return User{}, fmt.Errorf("sign in identity: find user: %w", err)
		}
	}

	inUse, err := s.emailInUse(ctx, tx, id.Email, u.ID)
	if err != nil {
		return User{}, fmt.Errorf("sign in identity: check email: %w", err)
	}
	if inUse {
		return User{}, ErrEmailInUse
	}

	if known {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET email = ? WHERE id = ?`, id.Email, u.ID); err != nil {
			return User{}, fmt.Errorf("sign in identity: update email: %w", err)
		}
		u.Email = id.Email
		// A forgotten identity signing in again keeps its data: its removal is called off.
		if _, err := tx.ExecContext(ctx, `DELETE FROM forgotten WHERE user_id = ?`, u.ID); err != nil {
			return User{}, fmt.Errorf("sign in identity: cancel removal: %w", err)
		}
	} else {
		newTenant := tenantID == 0
		if newTenant {
			if tenantID, err = s.createTenant(ctx, tx, id.Provider, id.Tenant, now); err != nil {
				return User{}, fmt.Errorf("sign in identity: create tenant: %w", err)
			}
		}
		if err := tx.scope(ctx, tenantID, true); err != nil {
			return User{}, fmt.Errorf("sign in identity: %w", err)
		}
		if newTenant && s.keys != nil {
			if _, _, err := s.makeTenantKey(ctx, tx, tenantID); err != nil {
				return User{}, fmt.Errorf("sign in identity: %w", err)
			}
		}
		var uid int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO users (email, password_hash, created_at, tenant_id) VALUES (?, '', ?, ?) RETURNING id`,
			id.Email, now, tenantID).Scan(&uid); err != nil {
			return User{}, fmt.Errorf("sign in identity: create user: %w", err)
		}
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

// sqliteTenantOf is, for SQLite, what mailrules_tenant_of answers for the kinds a sign-in
// asks before it knows the tenant.
var sqliteTenantOf = map[string]string{
	ofExternal: `SELECT id FROM tenants WHERE provider = ? AND external_id = ?`,
	ofIdentity: `SELECT u.tenant_id FROM identities i JOIN users u ON u.id = i.user_id WHERE i.provider = ? AND i.subject = ?`,
}

// findTenant is, read in q, the tenant of the row of kind keyed by key and key2, 0 when
// there is none.
func (s *Store) findTenant(ctx context.Context, q querier, kind, key, key2 string) (int64, error) {
	query := `SELECT mailrules_tenant_of(CAST(? AS text), CAST(? AS text), CAST(? AS text))`
	args := []any{kind, key, key2}
	if s.db.d != dialectPostgres {
		query, args = sqliteTenantOf[kind], []any{key, key2}
	}
	var t sql.NullInt64
	if err := q.QueryRowContext(ctx, query, args...).Scan(&t); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	return t.Int64, nil
}

// emailInUse reports, read in q, whether a user other than except has email, compared
// ignoring case, in any tenant: the one deliberate disclosure across tenants (a sign-in
// answers ErrEmailInUse).
func (s *Store) emailInUse(ctx context.Context, q querier, email string, except int64) (bool, error) {
	query := `SELECT mailrules_email_in_use(CAST(? AS text), CAST(? AS BIGINT))`
	if s.db.d != dialectPostgres {
		query = `SELECT EXISTS (SELECT 1 FROM users WHERE lower(email) = lower(?) AND id != ?)`
	}
	var inUse bool
	err := q.QueryRowContext(ctx, query, email, except).Scan(&inUse)
	return inUse, err
}

// createTenant adds a tenant in q and returns its id; on Postgres through the definer
// function, since no tenant's policy admits a row that does not exist yet.
func (s *Store) createTenant(ctx context.Context, q querier, provider, externalID string, now int64) (int64, error) {
	query := `SELECT mailrules_create_tenant(CAST(? AS text), CAST(? AS text), CAST(? AS BIGINT))`
	if s.db.d != dialectPostgres {
		query = `INSERT INTO tenants (provider, external_id, created_at) VALUES (?, ?, ?) RETURNING id`
	}
	var id int64
	err := q.QueryRowContext(ctx, query, null(provider), null(externalID), now).Scan(&id)
	return id, err
}

// EndIdentitySessions deletes every session of the identity's user. An identity nobody
// has is not an error.
func (s *Store) EndIdentitySessions(ctx context.Context, provider, subject string) error {
	ctx = s.tenantFor(ctx, ofIdentity, provider, subject)
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE user_id IN (SELECT user_id FROM identities WHERE provider = ? AND subject = ?)`,
		provider, subject); err != nil {
		return fmt.Errorf("end identity sessions: %w", err)
	}
	return nil
}

// UserIdentity is the identity a user was made from (SignInIdentity): its Provider and
// Subject, the rest left empty. A user with none, such as the self-host admin, is
// ErrNotFound.
func (s *Store) UserIdentity(ctx context.Context, userID int64) (Identity, error) {
	ctx = s.tenantFor(ctx, ofUser, userID)
	var id Identity
	err := s.db.QueryRowContext(ctx, `SELECT provider, subject FROM identities WHERE user_id = ? ORDER BY `+s.db.d.insertOrder()+` LIMIT 1`, userID).
		Scan(&id.Provider, &id.Subject)
	if errors.Is(err, sql.ErrNoRows) {
		return Identity{}, ErrNotFound
	}
	if err != nil {
		return Identity{}, fmt.Errorf("user identity: %w", err)
	}
	return id, nil
}
