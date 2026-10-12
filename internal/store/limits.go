package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// NoLimit is a limit that does not limit (CreateAccountWithin, and the limits a module sets).
const NoLimit = -1

// ErrMailboxLimit is what CreateAccountWithin returns, as a *MailboxLimitError, when the
// tenant already has as many mailboxes as it may.
var ErrMailboxLimit = errors.New("the mailbox limit is reached")

// MailboxLimitError is a mailbox refused because its tenant has Connected mailboxes and may
// have Limit. Its text is the sentence the screen shows.
type MailboxLimitError struct {
	Limit, Connected int
}

func (e *MailboxLimitError) Error() string {
	switch e.Limit {
	case 0:
		return "Your plan does not include a mailbox, so this one was not connected."
	case 1:
		return "Your plan allows 1 mailbox, and it is connected already. Remove it to connect another."
	}
	return fmt.Sprintf("Your plan allows %d mailboxes, and %d are connected already. Remove one to connect another.", e.Limit, e.Connected)
}

// Is makes errors.Is(err, ErrMailboxLimit) true.
func (e *MailboxLimitError) Is(target error) bool { return target == ErrMailboxLimit }

// CreateAccountWithin is CreateAccount for a tenant that may have at most max mailboxes
// (NoLimit: any number). The count and the insert are one write transaction, which holds the
// database's write lock from its start, so two mailboxes added at once cannot both take the
// last place. A tenant at or over max gets a *MailboxLimitError and nothing is stored.
func (s *Store) CreateAccountWithin(ctx context.Context, master []byte, a Account, secret string, max int) (Account, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Account{}, fmt.Errorf("create account: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	if max >= 0 {
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM accounts WHERE tenant_id = (SELECT tenant_id FROM users WHERE id = ?)`, a.UserID).Scan(&n); err != nil {
			return Account{}, fmt.Errorf("create account: count mailboxes: %w", err)
		}
		if n >= max {
			return Account{}, &MailboxLimitError{Limit: max, Connected: n}
		}
	}
	if a, err = createAccount(ctx, tx, master, a, secret); err != nil {
		return Account{}, err
	}
	if err := tx.Commit(); err != nil {
		return Account{}, fmt.Errorf("create account: %w", err)
	}
	return a, nil
}

// Month is the calendar month, in UTC as the usage ledger's days are, that an email decided
// at t counts in: YYYY-MM.
func Month(t time.Time) string { return t.UTC().Format("2006-01") }

// AIEmails returns how many emails a model decided for the tenant on keys the operator holds
// in month (see Month).
func (s *Store) AIEmails(ctx context.Context, tenantID int64, month string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT emails FROM ai_emails WHERE tenant_id = ? AND month = ?`, tenantID, month).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("count AI-sorted emails: %w", err)
	}
	return n, nil
}

// SpendAIEmails counts n emails a model decided for the tenant on keys the operator holds at
// at, which no decision row records: those of a cleanup check. Live sorting counts with the
// decision (Decision.Metered).
func (s *Store) SpendAIEmails(ctx context.Context, tenantID int64, at time.Time, n int) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO ai_emails (tenant_id, month, emails) VALUES (?, ?, ?)
		 ON CONFLICT (tenant_id, month) DO UPDATE SET emails = emails + excluded.emails`, tenantID, Month(at), n); err != nil {
		return fmt.Errorf("count AI-sorted emails: %w", err)
	}
	return nil
}

// meter counts, in tx, the message's email against its tenant's allowance for the month of
// at, unless it was counted before.
func meter(ctx context.Context, tx *sql.Tx, messageID, at int64) error {
	res, err := tx.ExecContext(ctx, `UPDATE messages SET metered = 1 WHERE id = ? AND metered = 0`, messageID)
	if err != nil {
		return fmt.Errorf("count AI-sorted email: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO ai_emails (tenant_id, month, emails)
		 SELECT a.tenant_id, ?, 1 FROM messages m JOIN accounts a ON a.id = m.account_id WHERE m.id = ?
		 ON CONFLICT (tenant_id, month) DO UPDATE SET emails = emails + 1`, Month(time.Unix(at, 0)), messageID); err != nil {
		return fmt.Errorf("count AI-sorted email: %w", err)
	}
	return nil
}
