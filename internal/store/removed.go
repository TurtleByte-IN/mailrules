package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/TurtleByte-IN/mailrules/internal/crypto"
)

// RemovedMailboxDays is how long a removed mailbox waits for the module that connected it to
// take it (ext.Module.MailboxRemoved); then it is deleted with its copy of the secret.
const RemovedMailboxDays = ForgetDays

// RemovedMailbox is a mailbox a module connected whose secret the daemon let go of, waiting
// to be handed to that module. Its secret is read with RemovedMailboxSecret.
type RemovedMailbox struct {
	ID        int64 // the row's own: the same on every hand-over
	TenantID  int64 // the tenant the row was read in; 0 on SQLite
	AccountID int64
	Preset    string
	OAuth     bool // connected with one-click sign-in; else through the module's sign-in form
	Username  string
	CreatedAt int64
	Attempts  int
}

// queueRemovedMailboxes writes, in tx, a removed_mailboxes row for each of ids that a module
// connected and that still has its secret, before the caller deletes the mailbox or its
// secret in the same transaction. A mailbox whose secret is already gone was queued when it
// went, so it is not queued twice.
//
// The copy is resealed under the key-encryption key itself (crypto.SealKEK, envelope 3),
// not the tenant's key: the row must open until the module takes it or it expires, and the
// tenant, with its key, may be deleted in the same transaction (RemoveForgotten,
// RemoveEndedTenant). The copy lives exactly as long as its row, which is deleted when
// taken and after RemovedMailboxDays at the latest, so the tenant's key can be destroyed at
// once without keeping anything else of the tenant.
func (s *Store) queueRemovedMailboxes(ctx context.Context, tx *txn, ids []int64, now int64) error {
	if len(ids) == 0 {
		return nil
	}
	enc, err := json.Marshal(ids)
	if err != nil {
		return fmt.Errorf("queue removed mailboxes: %w", err)
	}
	type queued struct {
		id, tenantID      int64
		preset, username  string
		oauth             bool
		secretEnc, dekEnc []byte
	}
	rs, err := tx.QueryContext(ctx, `SELECT id, tenant_id, preset, oauth, username, secret_enc, dek_enc FROM accounts
		WHERE id IN (`+tx.d.jsonValues("?", "BIGINT")+`) AND (module = TRUE OR oauth = TRUE) AND length(secret_enc) > 0 ORDER BY id`, string(enc))
	if err != nil {
		return fmt.Errorf("queue removed mailboxes: %w", err)
	}
	var qs []queued
	for rs.Next() {
		var q queued
		if err := rs.Scan(&q.id, &q.tenantID, &q.preset, &q.oauth, &q.username, &q.secretEnc, &q.dekEnc); err != nil {
			_ = rs.Close()
			return fmt.Errorf("queue removed mailboxes: %w", err)
		}
		qs = append(qs, q)
	}
	if err := errors.Join(rs.Err(), rs.Close()); err != nil {
		return fmt.Errorf("queue removed mailboxes: %w", err)
	}
	for _, q := range qs {
		key, err := s.tenantKey(ctx, tx, q.tenantID)
		if err != nil {
			return fmt.Errorf("queue removed mailboxes: %w", err)
		}
		ref := strconv.FormatInt(q.id, 10)
		plain, err := key.Open(secretAccount, ref, q.id, q.secretEnc, q.dekEnc)
		if err != nil {
			return fmt.Errorf("queue removed mailbox %d: %w", q.id, err)
		}
		secretEnc, dekEnc, err := crypto.SealKEK(ctx, s.keys.p, removedBinding(q.id), plain)
		if err != nil {
			return fmt.Errorf("queue removed mailbox %d: %w", q.id, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO removed_mailboxes (account_id, preset, oauth, username, secret_enc, dek_enc, created_at, next_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, q.id, q.preset, q.oauth, q.username, secretEnc, dekEnc, now, now); err != nil {
			return fmt.Errorf("queue removed mailboxes: %w", err)
		}
	}
	return nil
}

// removedBinding is what a removed mailbox's copy of its secret is sealed to: no tenant
// (the copy outlives it), the kind and the mailbox's id, which envelope 1 bound it to too.
func removedBinding(accountID int64) crypto.Binding {
	return crypto.Binding{Kind: secretRemoved, Ref: strconv.FormatInt(accountID, 10), LegacyRow: accountID}
}

// removedCols are the columns of a RemovedMailbox, as scanRemoved reads them.
const removedCols = `id, account_id, preset, oauth, username, created_at, attempts`

// scanRemoved reads every row of rs, read in tenantID, removedCols each and then into more
// whatever the query selects after them, passing each to add; it closes rs.
func scanRemoved(rs *rows, tenantID int64, add func(RemovedMailbox), more ...any) error {
	for rs.Next() {
		r := RemovedMailbox{TenantID: tenantID}
		if err := rs.Scan(append([]any{&r.ID, &r.AccountID, &r.Preset, &r.OAuth, &r.Username, &r.CreatedAt, &r.Attempts}, more...)...); err != nil {
			_ = rs.Close()
			return err
		}
		add(r)
	}
	return errors.Join(rs.Err(), rs.Close())
}

// WaitingRemovedMailboxes lists at most limit removed mailboxes due to be handed over by
// now, the longest waiting first.
func (s *Store) WaitingRemovedMailboxes(ctx context.Context, now int64, limit int) ([]RemovedMailbox, error) {
	var waiting []outboxRow[RemovedMailbox]
	err := s.eachTenant(ctx, tenantsRemoved, func(ctx context.Context, tenantID int64) error {
		rows, err := s.db.QueryContext(ctx, `SELECT `+removedCols+`, next_at FROM removed_mailboxes WHERE next_at <= ? ORDER BY next_at, id LIMIT ?`, now, limit)
		if err != nil {
			return err
		}
		var next int64
		return scanRemoved(rows, tenantID, func(r RemovedMailbox) {
			waiting = append(waiting, outboxRow[RemovedMailbox]{row: r, next: next, id: r.ID})
		}, &next)
	})
	if err != nil {
		return nil, fmt.Errorf("list removed mailboxes: %w", err)
	}
	return firstDue(waiting, limit), nil
}

// RemovedMailboxSecret opens the secret a removed mailbox had. It returns crypto.ErrDecrypt
// when the key-encryption key does not open it, and ErrNotFound when the row is gone.
func (s *Store) RemovedMailboxSecret(ctx context.Context, id int64) (string, error) {
	if s.keys == nil {
		return "", ErrNoKeys
	}
	ctx = s.tenantFor(ctx, ofRemoved, id)
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
	secret, err := crypto.OpenKEK(ctx, s.keys.p, s.legacy(), removedBinding(accountID), secretEnc, dekEnc)
	if err != nil {
		return "", fmt.Errorf("decrypt removed mailbox secret: %w", err)
	}
	return string(secret), nil
}

// RemovedMailboxTaken deletes a removed mailbox the module took, with its secret.
func (s *Store) RemovedMailboxTaken(ctx context.Context, id int64) error {
	ctx = s.tenantFor(ctx, ofRemoved, id)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM removed_mailboxes WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete removed mailbox: %w", err)
	}
	return nil
}

// RemovedMailboxRetry records that a removed mailbox was not taken; it is handed over again
// at next.
func (s *Store) RemovedMailboxRetry(ctx context.Context, id, next int64) error {
	ctx = s.tenantFor(ctx, ofRemoved, id)
	if _, err := s.db.ExecContext(ctx, `UPDATE removed_mailboxes SET attempts = attempts + 1, next_at = ? WHERE id = ?`, next, id); err != nil {
		return fmt.Errorf("retry removed mailbox: %w", err)
	}
	return nil
}

// ExpireRemovedMailboxes deletes, with their secrets, the removed mailboxes that waited
// RemovedMailboxDays by now without being taken, and returns them.
func (s *Store) ExpireRemovedMailboxes(ctx context.Context, now int64) ([]RemovedMailbox, error) {
	var out []RemovedMailbox
	err := s.eachTenant(ctx, tenantsRemoved, func(ctx context.Context, tenantID int64) error {
		rows, err := s.db.QueryContext(ctx, `DELETE FROM removed_mailboxes WHERE created_at <= ? RETURNING `+removedCols,
			now-RemovedMailboxDays*day)
		if err != nil {
			return err
		}
		return scanRemoved(rows, tenantID, func(r RemovedMailbox) { out = append(out, r) })
	})
	if err != nil {
		return nil, fmt.Errorf("expire removed mailboxes: %w", err)
	}
	return out, nil
}
