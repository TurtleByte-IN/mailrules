package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/TurtleByte-IN/mailrules/internal/crypto"
)

// RemovedMailboxDays is how long a removed mailbox waits for the module that connected it to
// take it (ext.Module.MailboxRemoved); then it is deleted with its copy of the secret.
const RemovedMailboxDays = ForgetDays

// RemovedMailbox is a mailbox a module connected whose secret the daemon let go of, waiting
// to be handed to that module. Its secret is read with RemovedMailboxSecret.
type RemovedMailbox struct {
	ID        int64 // the row's own: the same on every hand-over
	AccountID int64
	Preset    string
	OAuth     bool // connected with one-click sign-in; else through the module's sign-in form
	Username  string
	CreatedAt int64
	Attempts  int
}

// queueRemovedMailboxes writes, in tx, a removed_mailboxes row for each of ids that a module
// connected and that still has its secret, copied as sealed, before the caller deletes the
// mailbox or its secret in the same transaction. A mailbox whose secret is already gone was
// queued when it went, so it is not queued twice.
func queueRemovedMailboxes(ctx context.Context, tx *sql.Tx, ids []int64, now int64) error {
	if len(ids) == 0 {
		return nil
	}
	enc, err := json.Marshal(ids)
	if err != nil {
		return fmt.Errorf("queue removed mailboxes: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO removed_mailboxes (account_id, preset, oauth, username, secret_enc, dek_enc, created_at, next_at)
		SELECT id, preset, oauth, username, secret_enc, dek_enc, ?1, ?1 FROM accounts
		WHERE id IN (SELECT value FROM json_each(?2)) AND (module = 1 OR oauth = 1) AND length(secret_enc) > 0 ORDER BY id`,
		now, string(enc)); err != nil {
		return fmt.Errorf("queue removed mailboxes: %w", err)
	}
	return nil
}

// removedCols are the columns of a RemovedMailbox, as scanRemoved reads them.
const removedCols = `id, account_id, preset, oauth, username, created_at, attempts`

// scanRemoved reads every row of rows, removedCols each, and closes them.
func scanRemoved(rows *sql.Rows) ([]RemovedMailbox, error) {
	defer rows.Close()
	var out []RemovedMailbox
	for rows.Next() {
		var r RemovedMailbox
		if err := rows.Scan(&r.ID, &r.AccountID, &r.Preset, &r.OAuth, &r.Username, &r.CreatedAt, &r.Attempts); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// WaitingRemovedMailboxes lists at most limit removed mailboxes due to be handed over by
// now, the longest waiting first.
func (s *Store) WaitingRemovedMailboxes(ctx context.Context, now int64, limit int) ([]RemovedMailbox, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+removedCols+` FROM removed_mailboxes WHERE next_at <= ? ORDER BY next_at, id LIMIT ?`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list removed mailboxes: %w", err)
	}
	out, err := scanRemoved(rows)
	if err != nil {
		return nil, fmt.Errorf("list removed mailboxes: %w", err)
	}
	return out, nil
}

// RemovedMailboxSecret opens the secret a removed mailbox had. It returns crypto.ErrDecrypt
// when the master key is not the one it was sealed under, and ErrNotFound when the row is
// gone.
func (s *Store) RemovedMailboxSecret(ctx context.Context, master []byte, id int64) (string, error) {
	var accountID int64
	var secretEnc, dekEnc []byte
	err := s.db.QueryRowContext(ctx, `SELECT account_id, secret_enc, dek_enc FROM removed_mailboxes WHERE id = ?`, id).
		Scan(&accountID, &secretEnc, &dekEnc)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get removed mailbox secret: %w", err)
	}
	secret, err := crypto.Open(master, accountID, secretEnc, dekEnc)
	if err != nil {
		return "", fmt.Errorf("decrypt removed mailbox secret: %w", err)
	}
	return string(secret), nil
}

// RemovedMailboxTaken deletes a removed mailbox the module took, with its secret.
func (s *Store) RemovedMailboxTaken(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM removed_mailboxes WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete removed mailbox: %w", err)
	}
	return nil
}

// RemovedMailboxRetry records that a removed mailbox was not taken; it is handed over again
// at next.
func (s *Store) RemovedMailboxRetry(ctx context.Context, id, next int64) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE removed_mailboxes SET attempts = attempts + 1, next_at = ? WHERE id = ?`, next, id); err != nil {
		return fmt.Errorf("retry removed mailbox: %w", err)
	}
	return nil
}

// ExpireRemovedMailboxes deletes, with their secrets, the removed mailboxes that waited
// RemovedMailboxDays by now without being taken, and returns them.
func (s *Store) ExpireRemovedMailboxes(ctx context.Context, now int64) ([]RemovedMailbox, error) {
	rows, err := s.db.QueryContext(ctx, `DELETE FROM removed_mailboxes WHERE created_at <= ? RETURNING `+removedCols,
		now-RemovedMailboxDays*day)
	if err != nil {
		return nil, fmt.Errorf("expire removed mailboxes: %w", err)
	}
	out, err := scanRemoved(rows)
	if err != nil {
		return nil, fmt.Errorf("expire removed mailboxes: %w", err)
	}
	return out, nil
}
