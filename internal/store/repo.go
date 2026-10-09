package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrNotFound means the row does not exist (or, for a session, has expired).
var ErrNotFound = errors.New("not found")

// ErrAlreadySetUp means the first-run admin account already exists.
var ErrAlreadySetUp = errors.New("already set up")

// Store is the repository over the database. All times are unix seconds.
type Store struct{ db *sql.DB }

// New wraps an open, migrated database.
func New(db *sql.DB) *Store { return &Store{db: db} }

// User is a person who signs in. Self-host has one, the admin, in SelfHostTenant.
type User struct {
	ID           int64
	Email        string
	PasswordHash string
	CreatedAt    int64
	TenantID     int64
}

const userCols = `id, email, password_hash, created_at, tenant_id`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt, &u.TenantID)
	return u, err
}

// CreateFirstUser inserts the admin account, in the self-host tenant, only if no user
// exists yet.
func (s *Store) CreateFirstUser(ctx context.Context, email, passwordHash string, now int64) (User, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (email, password_hash, created_at, tenant_id)
		 SELECT ?, ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM users)`, email, passwordHash, now, SelfHostTenant)
	if err != nil {
		return User{}, fmt.Errorf("create user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return User{}, ErrAlreadySetUp
	}
	id, _ := res.LastInsertId()
	return User{ID: id, Email: email, PasswordHash: passwordHash, CreatedAt: now, TenantID: SelfHostTenant}, nil
}

// HasUsers reports whether first-run setup has been done.
func (s *Store) HasUsers(ctx context.Context) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users)`).Scan(&n); err != nil {
		return false, fmt.Errorf("count users: %w", err)
	}
	return n == 1, nil
}

// UserByEmail looks an account up by its (case-insensitive) email.
func (s *Store) UserByEmail(ctx context.Context, email string) (User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE email = ? COLLATE NOCASE`, email))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}

// CreateSession stores a login session by the hash of its token, and sweeps expired ones.
func (s *Store) CreateSession(ctx context.Context, tokenHash string, userID, now, expiresAt int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, now); err != nil {
		return fmt.Errorf("sweep sessions: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		tokenHash, userID, now, expiresAt); err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// SessionUser returns the user behind a live session and when it expires.
func (s *Store) SessionUser(ctx context.Context, tokenHash string, now int64) (User, int64, error) {
	var u User
	var expiresAt int64
	err := s.db.QueryRowContext(ctx,
		`SELECT u.id, u.email, u.password_hash, u.created_at, u.tenant_id, s.expires_at
		 FROM sessions s JOIN users u ON u.id = s.user_id
		 WHERE s.token_hash = ? AND s.expires_at > ?`, tokenHash, now).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt, &u.TenantID, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, 0, ErrNotFound
	}
	if err != nil {
		return User{}, 0, fmt.Errorf("get session: %w", err)
	}
	return u, expiresAt, nil
}

// ExtendSession slides a session's expiry forward.
func (s *Store) ExtendSession(ctx context.Context, tokenHash string, expiresAt int64) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET expires_at = ? WHERE token_hash = ?`, expiresAt, tokenHash); err != nil {
		return fmt.Errorf("extend session: %w", err)
	}
	return nil
}

// DeleteSession ends a session; deleting a missing one is not an error.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// SetPassword replaces a user's password hash and, in the same transaction, ends every
// session the user has: a session someone else holds does not outlive the password that
// opened it. A missing user is ErrNotFound.
func (s *Store) SetPassword(ctx context.Context, userID int64, passwordHash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, passwordHash, userID)
	if err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("end sessions: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	return nil
}

// Setting returns one of a tenant's stored JSON values, or ErrNotFound.
func (s *Store) Setting(ctx context.Context, tenantID int64, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE tenant_id = ? AND key = ?`, tenantID, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get setting: %w", err)
	}
	return v, nil
}

// SetSetting stores a tenant's JSON value under key, replacing any previous one.
func (s *Store) SetSetting(ctx context.Context, tenantID int64, key, value string) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (tenant_id, key, value) VALUES (?, ?, ?) ON CONFLICT (tenant_id, key) DO UPDATE SET value = excluded.value`,
		tenantID, key, value); err != nil {
		return fmt.Errorf("set setting: %w", err)
	}
	return nil
}

// AddUsage adds model calls to the tenant's cost-ledger row for that day, provider, model
// and purpose (decide | escalate | compose | test | cleanup), creating it if needed.
// operator marks calls made with a key the operator holds (MAILRULES_MODE=cloud): they are
// booked on rows of their own, so billing can charge for them.
func (s *Store) AddUsage(ctx context.Context, tenantID int64, operator bool, day, provider, model, purpose string, calls, tokensIn, tokensOut int, costUSD float64) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO usage_daily (tenant_id, operator, day, provider, model, purpose, calls, tokens_in, tokens_out, cost_usd)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (tenant_id, operator, day, provider, model, purpose) DO UPDATE SET
		   calls = calls + excluded.calls,
		   tokens_in = tokens_in + excluded.tokens_in,
		   tokens_out = tokens_out + excluded.tokens_out,
		   cost_usd = cost_usd + excluded.cost_usd`,
		tenantID, operator, day, provider, model, purpose, calls, tokensIn, tokensOut, costUSD); err != nil {
		return fmt.Errorf("add usage: %w", err)
	}
	return nil
}

// Ledger books model usage for one tenant (models.UsageStore). Operator says, by the
// provider a call went to (models.Usage.Provider), whether it used a key the operator
// holds; nil marks none.
type Ledger struct {
	Store    *Store
	TenantID int64
	Operator func(provider string) bool
}

// AddUsage books calls for the ledger's tenant.
func (l Ledger) AddUsage(ctx context.Context, day, provider, model, purpose string, calls, tokensIn, tokensOut int, costUSD float64) error {
	op := l.Operator != nil && l.Operator(provider)
	return l.Store.AddUsage(ctx, l.TenantID, op, day, provider, model, purpose, calls, tokensIn, tokensOut, costUSD)
}

// Ping reports whether the database answers.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	return nil
}

// Settings returns every setting a tenant has stored, key to JSON value.
func (s *Store) Settings(ctx context.Context, tenantID int64) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings WHERE tenant_id = ?`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list settings: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("list settings: %w", err)
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list settings: %w", err)
	}
	return out, nil
}

// SettingsLike returns, across every tenant, the stored settings whose key starts with
// prefix, as tenant id and key to JSON value. It is for checks over the whole database (the
// master key check counts every stored provider key), never for showing.
func (s *Store) SettingsLike(ctx context.Context, prefix string) (map[int64]map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tenant_id, key, value FROM settings WHERE substr(key, 1, length(?1)) = ?1`, prefix)
	if err != nil {
		return nil, fmt.Errorf("list settings: %w", err)
	}
	defer rows.Close()
	out := map[int64]map[string]string{}
	for rows.Next() {
		var t int64
		var k, v string
		if err := rows.Scan(&t, &k, &v); err != nil {
			return nil, fmt.Errorf("list settings: %w", err)
		}
		if out[t] == nil {
			out[t] = map[string]string{}
		}
		out[t][k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list settings: %w", err)
	}
	return out, nil
}

// SetSettings stores several of a tenant's settings in one transaction, so a change is
// never half applied. A nil value removes the key, which puts its default back in force.
func (s *Store) SetSettings(ctx context.Context, tenantID int64, values map[string]*string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set settings: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	for k, v := range values {
		if v == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM settings WHERE tenant_id = ? AND key = ?`, tenantID, k)
		} else {
			_, err = tx.ExecContext(ctx,
				`INSERT INTO settings (tenant_id, key, value) VALUES (?, ?, ?) ON CONFLICT (tenant_id, key) DO UPDATE SET value = excluded.value`,
				tenantID, k, *v)
		}
		if err != nil {
			return fmt.Errorf("set settings: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set settings: %w", err)
	}
	return nil
}
