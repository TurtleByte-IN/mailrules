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

// User is the admin account.
type User struct {
	ID           int64
	Email        string
	PasswordHash string
	CreatedAt    int64
}

// CreateFirstUser inserts the admin account only if no user exists yet.
func (s *Store) CreateFirstUser(ctx context.Context, email, passwordHash string, now int64) (User, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (email, password_hash, created_at)
		 SELECT ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM users)`, email, passwordHash, now)
	if err != nil {
		return User{}, fmt.Errorf("create user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return User{}, ErrAlreadySetUp
	}
	id, _ := res.LastInsertId()
	return User{ID: id, Email: email, PasswordHash: passwordHash, CreatedAt: now}, nil
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
	var u User
	err := s.db.QueryRowContext(ctx,
		`SELECT id, email, password_hash, created_at FROM users WHERE email = ? COLLATE NOCASE`, email).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt)
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
		`SELECT u.id, u.email, u.password_hash, u.created_at, s.expires_at
		 FROM sessions s JOIN users u ON u.id = s.user_id
		 WHERE s.token_hash = ? AND s.expires_at > ?`, tokenHash, now).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt, &expiresAt)
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

// Setting returns a stored JSON value, or ErrNotFound.
func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get setting: %w", err)
	}
	return v, nil
}

// SetSetting stores a JSON value under key, replacing any previous one.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		key, value); err != nil {
		return fmt.Errorf("set setting: %w", err)
	}
	return nil
}
