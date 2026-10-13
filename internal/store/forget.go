package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/TurtleByte-IN/mailrules/internal/rules"
)

// ForgetDays is how long what MailRules kept of a forgotten person waits before it is
// removed. Signing in again with the same identity before then cancels the removal, unless
// the person deleted their own account (DeleteOwnAccount).
const ForgetDays = 30

// forgottenEmail is the address a forgotten user holds while their removal waits: unique,
// in the reserved .invalid domain, so no sign-in service can vouch for it and the person's
// own email is free at once. Signing in again sets the real one back.
func forgottenEmail(userID int64) string {
	return "forgotten-" + strconv.FormatInt(userID, 10) + "@forgotten.invalid"
}

// removedMember is what stands for a forgotten or removed person in the audit log, in place
// of their address. The audit log's guard accepts no other rewrite (migration 0029).
func removedMember(userID int64) string {
	return "removed member #" + strconv.FormatInt(userID, 10)
}

// forgottenMessage is the last error a forgotten person's mailboxes show: their secret is
// gone, and they need a new one if the same person signs in again. A mailbox a module
// connected (oauth or module: one-click, or through its sign-in form) shows
// forgottenOAuthMessage and reconnect_needed instead of auth_failed.
const (
	forgottenMessage      = "Your MailRules sign-in was deleted, so this mailbox's password was deleted with it. Enter a new app password to start sorting again."
	forgottenOAuthMessage = "Your MailRules sign-in was deleted, so this mailbox's sign-in was deleted with it. Reconnect to sign in to the provider again."
)

// providerKeys matches the settings rows that hold a tenant's stored model API keys
// (settings.keyPrefix).
const providerKeys = `substr(key, 1, 4) = 'key.'`

// Forget is what ForgetIdentity did at once.
type Forget struct {
	UserID, TenantID int64 // 0 when no user has the identity
	// Accounts are the person's mailboxes whose secrets it deleted: every mailbox of the
	// tenant when they are its only member, else the ones they own, shared or not.
	Accounts []int64
	// Keys is whether it deleted the tenant's stored model API keys, and the addresses and
	// signing secrets of its notification destinations: the person is the tenant's only
	// member.
	Keys bool
}

// IdentityMailboxes lists the mailboxes ForgetIdentity would take the secrets of, as things
// stand. Their supervisors are stopped before ForgetIdentity runs. An identity nobody has
// has none.
func (s *Store) IdentityMailboxes(ctx context.Context, provider, subject string) ([]int64, error) {
	ctx = s.tenantFor(ctx, ofIdentity, provider, subject)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("list identity mailboxes: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	userID, tenantID, err := identityUser(ctx, tx, provider, subject)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list identity mailboxes: %w", err)
	}
	_, ids, err := removalAccounts(ctx, tx, userID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list identity mailboxes: %w", err)
	}
	return ids, nil
}

// identityUser finds, in tx, the user made from an identity, never one in the self-host
// tenant; ErrNotFound when there is none.
func identityUser(ctx context.Context, tx *txn, provider, subject string) (userID, tenantID int64, err error) {
	err = tx.QueryRowContext(ctx, `SELECT u.id, u.tenant_id FROM identities i JOIN users u ON u.id = i.user_id
		WHERE i.provider = ? AND i.subject = ? AND u.tenant_id != ?`, provider, subject, SelfHostTenant).Scan(&userID, &tenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, ErrNotFound
	}
	return userID, tenantID, err
}

// ForgetIdentity is a person the sign-in service deleted. In one transaction it ends every
// session of the identity's user, frees their email (a new identity may sign up with it at
// once), deletes their secrets and schedules the removal of the rest of their data for
// ForgetDays from now. The secrets are the passwords and tokens of their mailboxes (every
// mailbox of the tenant when they are its only member, else the ones they own, shared or
// not), which then show auth_failed asking for a new password, and, when they are the
// tenant's only member, the tenant's stored model API keys. A mailbox a module connected is
// first queued for that module with its secret (queueRemovedMailboxes). The caller stops those
// mailboxes' supervisors before (IdentityMailboxes) and after (Forget.Accounts) it.
// Everything else is left as it is until the removal. An identity nobody has is not an
// error, and forgetting one again changes nothing, the removal time included. Only users
// made from an identity are reached, so the self-host admin never is.
func (s *Store) ForgetIdentity(ctx context.Context, provider, subject string, now int64) (Forget, error) {
	return s.forget(ctx, provider, subject, now, false, "their sign-in was deleted")
}

// RemoveMember is a member an owner or admin removed from their team: ForgetIdentity,
// recorded in the audit log as a removal.
func (s *Store) RemoveMember(ctx context.Context, provider, subject string, now int64) (Forget, error) {
	return s.forget(ctx, provider, subject, now, false, "removed from the team")
}

// DeleteOwnAccount is a person who deleted their own account: ForgetIdentity, and in the
// same transaction their user lets go of the identity, so that it never gets their data
// back: the same identity signing in again, before the removal or after, gets a new, empty
// user. When they are their tenant's only member, the tenant lets go of the sign-in
// service's organisation too, so that user gets a new tenant instead of joining the old
// data's. The old data is removed after ForgetDays all the same, and the service's own
// deletion notice for the identity afterwards reaches nobody.
func (s *Store) DeleteOwnAccount(ctx context.Context, provider, subject string, now int64) (Forget, error) {
	return s.forget(ctx, provider, subject, now, true, "deleted their own account")
}

// forget is ForgetIdentity, and DeleteOwnAccount when own; how is the audit log's detail.
func (s *Store) forget(ctx context.Context, provider, subject string, now int64, own bool, how string) (Forget, error) {
	ctx = s.tenantFor(ctx, ofIdentity, provider, subject)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Forget{}, fmt.Errorf("forget identity: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	const user = `SELECT u.id FROM identities i JOIN users u ON u.id = i.user_id
		WHERE i.provider = ? AND i.subject = ? AND u.tenant_id != ?`
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id IN (`+user+`)`, provider, subject, SelfHostTenant); err != nil {
		return Forget{}, fmt.Errorf("forget identity: end sessions: %w", err)
	}
	var out Forget
	out.UserID, out.TenantID, err = identityUser(ctx, tx, provider, subject)
	if errors.Is(err, ErrNotFound) {
		return Forget{}, nil
	}
	if err != nil {
		return Forget{}, fmt.Errorf("forget identity: find user: %w", err)
	}
	var email string
	var again bool // forgotten before: nothing more to record
	if err := tx.QueryRowContext(ctx, `SELECT email, id IN (SELECT user_id FROM forgotten) FROM users WHERE id = ?`, out.UserID).
		Scan(&email, &again); err != nil {
		return Forget{}, fmt.Errorf("forget identity: find user: %w", err)
	}
	// A team losing its last owner gets the next one as they go (promoteHeir).
	if !again {
		if err := promoteHeir(ctx, tx, out.TenantID, out.UserID, now); err != nil {
			return Forget{}, fmt.Errorf("forget identity: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET email = ? WHERE id = ?`, forgottenEmail(out.UserID), out.UserID); err != nil {
		return Forget{}, fmt.Errorf("forget identity: free email: %w", err)
	}
	var alone bool
	if alone, out.Accounts, err = removalAccounts(ctx, tx, out.UserID, out.TenantID); err != nil {
		return Forget{}, fmt.Errorf("forget identity: list mailboxes: %w", err)
	}
	out.Keys = alone
	if err := s.deleteSecrets(ctx, tx, out.TenantID, out.Accounts, out.Keys, forgottenMessage, forgottenOAuthMessage, now); err != nil {
		return Forget{}, fmt.Errorf("forget identity: %w", err)
	}
	if !again {
		// The audit log keeps what they did and what was done to them, but not their address:
		// it becomes removedMember in every row, this removal's own included.
		gone := removedMember(out.UserID)
		if err := record(ctx, tx, out.TenantID, now, AuditMemberRemoved, gone, how); err != nil {
			return Forget{}, fmt.Errorf("forget identity: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE audit_log SET actor = ? WHERE tenant_id = ? AND actor_id = ?`,
			gone, out.TenantID, out.UserID); err != nil {
			return Forget{}, fmt.Errorf("forget identity: audit log actor: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE audit_log SET target = ? WHERE tenant_id = ? AND lower(target) = lower(?)`,
			gone, out.TenantID, email); err != nil {
			return Forget{}, fmt.Errorf("forget identity: audit log target: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO forgotten (user_id, forgotten_at, remove_at) VALUES (?, ?, ?) ON CONFLICT (user_id) DO NOTHING`,
		out.UserID, now, now+ForgetDays*day); err != nil {
		return Forget{}, fmt.Errorf("forget identity: schedule removal: %w", err)
	}
	if own {
		if _, err := tx.ExecContext(ctx, `DELETE FROM identities WHERE user_id = ?`, out.UserID); err != nil {
			return Forget{}, fmt.Errorf("forget identity: let go of the identity: %w", err)
		}
		// NULL never matches the lookup by organisation, and is unique for UNIQUE.
		if alone {
			if _, err := tx.ExecContext(ctx, `UPDATE tenants SET external_id = NULL WHERE id = ?`, out.TenantID); err != nil {
				return Forget{}, fmt.Errorf("forget identity: let go of the organisation: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return Forget{}, fmt.Errorf("forget identity: %w", err)
	}
	return out, nil
}

// deleteSecrets deletes, in tx, the secrets of the mailboxes in accounts, all of the
// tenant's: each one a module connected is first queued for that module with its secret
// (queueRemovedMailboxes), then every one's password or token is blanked and it shows
// auth_failed with msg, or reconnect_needed with oauthMsg for a mailbox a module connected
// (oauth or module). When keys, the tenant's stored model API keys go too, and the
// addresses and signing secrets of its notification destinations. A mailbox whose secret
// is already gone keeps its status, so a second call changes nothing.
func (s *Store) deleteSecrets(ctx context.Context, tx *txn, tenantID int64, accounts []int64, keys bool, msg, oauthMsg string, now int64) error {
	// The mailboxes a module connected are queued for it with their secrets before those go.
	if err := s.queueRemovedMailboxes(ctx, tx, accounts, now); err != nil {
		return err
	}
	// secret_enc and dek_enc are NOT NULL: empty is "deleted" (Account.SecretGone); for a
	// one-click mailbox that is its refresh token.
	for _, id := range accounts {
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET secret_enc = ?, dek_enc = ?,
			status = CASE WHEN oauth = TRUE OR module = TRUE THEN 'reconnect_needed' ELSE 'auth_failed' END,
			last_error = CASE WHEN oauth = TRUE OR module = TRUE THEN CAST(? AS TEXT) ELSE CAST(? AS TEXT) END, last_event_at = ?
			WHERE id = ? AND length(secret_enc) > 0`, []byte{}, []byte{}, oauthMsg, msg, now, id); err != nil {
			return fmt.Errorf("delete mailbox secret: %w", err)
		}
	}
	if !keys {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE tenant_id = ? AND `+providerKeys, tenantID); err != nil {
		return fmt.Errorf("delete model keys: %w", err)
	}
	// The destinations stay, named; nothing can be delivered to them until they are added
	// again.
	if _, err := tx.ExecContext(ctx, `UPDATE notify_destinations SET secret_enc = ?, dek_enc = ? WHERE tenant_id = ?`,
		[]byte{}, []byte{}, tenantID); err != nil {
		return fmt.Errorf("delete notification addresses: %w", err)
	}
	return nil
}

// Forgotten is a forgotten user whose removal is waiting.
type Forgotten struct {
	UserID, TenantID int64
	RemoveAt         int64
}

// DueForRemoval lists the forgotten users whose removal time has come by now, the earliest
// first.
func (s *Store) DueForRemoval(ctx context.Context, now int64) ([]Forgotten, error) {
	var out []Forgotten
	err := s.eachTenant(ctx, tenantsForgotten, func(ctx context.Context, _ int64) error {
		rows, err := s.db.QueryContext(ctx, `SELECT f.user_id, u.tenant_id, f.remove_at
			FROM forgotten f JOIN users u ON u.id = f.user_id WHERE f.remove_at <= ? ORDER BY f.remove_at, f.user_id`, now)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var f Forgotten
			if err := rows.Scan(&f.UserID, &f.TenantID, &f.RemoveAt); err != nil {
				return err
			}
			out = append(out, f)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list removals due: %w", err)
	}
	slices.SortStableFunc(out, func(a, b Forgotten) int {
		return cmp.Or(cmp.Compare(a.RemoveAt, b.RemoveAt), cmp.Compare(a.UserID, b.UserID))
	})
	return out, nil
}

// RemovalAccounts lists the mailboxes removing the user would delete as things stand: every
// mailbox of their tenant when they are its only member, else the ones they own, shared or
// not. Their supervisors are stopped before RemoveForgotten runs.
func (s *Store) RemovalAccounts(ctx context.Context, userID int64) ([]int64, error) {
	ctx = s.tenantFor(ctx, ofUser, userID)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("list removal accounts: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var tenantID int64
	if err := tx.QueryRowContext(ctx, `SELECT tenant_id FROM users WHERE id = ?`, userID).Scan(&tenantID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("list removal accounts: %w", err)
	}
	_, ids, err := removalAccounts(ctx, tx, userID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list removal accounts: %w", err)
	}
	return ids, nil
}

// leaverMailboxes is which of a leaving team member's mailboxes go with them: every one they
// own, shared or not. MailRules is not mailbox software: a shared mailbox is the person's
// own mail, which the team saw while they were in it.
const leaverMailboxes = `user_id = ?`

// removalAccounts reports, in tx, whether the user is their tenant's only member, and the
// mailboxes their removal deletes.
func removalAccounts(ctx context.Context, tx *txn, userID, tenantID int64) (alone bool, ids []int64, err error) {
	var others int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE tenant_id = ? AND id != ?`, tenantID, userID).Scan(&others); err != nil {
		return false, nil, err
	}
	alone = others == 0
	query, arg := `SELECT id FROM accounts WHERE `+leaverMailboxes+` ORDER BY id`, userID
	if alone {
		query, arg = `SELECT id FROM accounts WHERE tenant_id = ? ORDER BY id`, tenantID
	}
	rows, err := tx.QueryContext(ctx, query, arg)
	if err != nil {
		return false, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return false, nil, err
		}
		ids = append(ids, id)
	}
	return alone, ids, rows.Err()
}

// Removal is what RemoveForgotten removed.
type Removal struct {
	TenantID int64
	// Tenant is whether the whole tenant went: the user was its only member.
	Tenant bool
	// Accounts are the mailboxes deleted.
	Accounts []int64
	// Rules are the team's rules rewritten because a mailbox they named was deleted (as
	// DeleteAccount rewrites them), when the tenant stays.
	Rules []rules.Rule
}

// RemoveForgotten removes, in one transaction, what MailRules kept of a forgotten user
// whose removal time has come by now. When they are their tenant's only member the whole
// tenant goes: its mailboxes with their credentials, folders, contacts, messages,
// decisions, actions and corrections, its rules, sender rules, batches, settings (provider
// keys included), usage ledger, and notification destinations with their notices and
// delivery log, the user with their sessions, identity and summaries, and the tenant
// itself. A member of a team takes all their mailboxes, shared or not (the team's rules
// naming one are rewritten as DeleteAccount does), with the notices about their mail,
// their sessions, identity, summaries and user row; the authorship of their rules and
// sender rules passes to the team (handOver). It answers ErrNotFound, changing nothing,
// for a user who is not forgotten or not yet due: one who signed in again, or was removed
// already.
func (s *Store) RemoveForgotten(ctx context.Context, userID, now int64) (Removal, error) {
	ctx = s.tenantFor(ctx, ofUser, userID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Removal{}, fmt.Errorf("remove forgotten user: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	// Deleting the forgotten row first says whether the removal is still due; deleting the
	// user would cascade to it anyway.
	res, err := tx.ExecContext(ctx, `DELETE FROM forgotten WHERE user_id = ? AND remove_at <= ?`, userID, now)
	if err != nil {
		return Removal{}, fmt.Errorf("remove forgotten user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Removal{}, ErrNotFound
	}
	var out Removal
	if err := tx.QueryRowContext(ctx, `SELECT tenant_id FROM users WHERE id = ?`, userID).Scan(&out.TenantID); err != nil {
		return Removal{}, fmt.Errorf("remove forgotten user: %w", err)
	}
	if out.TenantID == SelfHostTenant { // never: an identity's user is not in it
		return Removal{}, fmt.Errorf("remove forgotten user %d: in the self-host tenant", userID)
	}
	out.Tenant, out.Accounts, err = removalAccounts(ctx, tx, userID, out.TenantID)
	if err != nil {
		return Removal{}, fmt.Errorf("remove forgotten user: %w", err)
	}
	// Their secrets went when they were forgotten, so normally none is queued here.
	if err := s.queueRemovedMailboxes(ctx, tx, out.Accounts, now); err != nil {
		return Removal{}, fmt.Errorf("remove forgotten user: %w", err)
	}
	if out.Tenant {
		err = removeTenant(ctx, tx, out.TenantID)
	} else {
		out.Rules, err = removeMember(ctx, tx, userID, out.TenantID, out.Accounts, now)
	}
	if err != nil {
		return Removal{}, fmt.Errorf("remove forgotten user: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Removal{}, fmt.Errorf("remove forgotten user: %w", err)
	}
	return out, nil
}

// removeTenant deletes, in tx, a tenant and everything in it, its plan too. Deleting a
// mailbox cascades to its folders, contacts, messages, decisions, actions, corrections and
// notices (and to its rules, which go anyway); deleting a notification destination to its
// delivery log; deleting a user to their sessions, identity, summaries and forgotten row.
// Deleting the tenant row deletes the tenant's data key (tenants.dek_enc): a copy of one of
// its secrets kept apart from that row no longer opens (crypto-shred). A backup of the
// whole database holds its own copy of the key until the backup itself is gone. A removed
// mailbox queued for its module keeps a copy of its secret that does not depend on the
// tenant's key (queueRemovedMailboxes).
func removeTenant(ctx context.Context, tx *txn, tenantID int64) error {
	for _, q := range []struct{ what, query string }{
		{"mailboxes", `DELETE FROM accounts WHERE tenant_id = ?`},
		{"notices", `DELETE FROM notices WHERE tenant_id = ?`},
		{"draft jobs", `DELETE FROM draft_jobs WHERE tenant_id = ?`},
		{"notification destinations", `DELETE FROM notify_destinations WHERE tenant_id = ?`},
		{"invitations", `DELETE FROM invitations WHERE tenant_id = ?`},
		{"audit log", `DELETE FROM audit_log WHERE tenant_id = ?`},
		{"sender rules", `DELETE FROM sender_rules WHERE tenant_id = ?`},
		{"rules", `DELETE FROM rules WHERE tenant_id = ?`},
		{"batches", `DELETE FROM batches WHERE tenant_id = ?`},
		{"settings", `DELETE FROM settings WHERE tenant_id = ?`},
		{"usage", `DELETE FROM usage_daily WHERE tenant_id = ?`},
		{"AI-sorted email counts", `DELETE FROM ai_emails WHERE tenant_id = ?`},
		{"first-cleanup grant", `DELETE FROM first_cleanup WHERE tenant_id = ?`},
		{"plan", `DELETE FROM plans WHERE tenant_id = ?`}, // no foreign key, and tenant ids can be reused
		{"users", `DELETE FROM users WHERE tenant_id = ?`},
		{"tenant", `DELETE FROM tenants WHERE id = ?`},
	} {
		if _, err := tx.ExecContext(ctx, q.query, tenantID); err != nil {
			return fmt.Errorf("delete %s: %w", q.what, err)
		}
	}
	return nil
}

// removeMember deletes, in tx, one member of a team that stays: the mailboxes in accounts
// (all of theirs), after rewriting the team's rules that name one, then the authorship of
// their rules and sender rules is handed over and their user row deleted. It returns the
// rules it rewrote.
func removeMember(ctx context.Context, tx *txn, userID, tenantID int64, accounts []int64, now int64) ([]rules.Rule, error) {
	gone := make(map[int64]bool, len(accounts))
	for _, id := range accounts {
		gone[id] = true
	}
	changed, err := dropMailboxes(ctx, tx, func(id int64) bool { return gone[id] }, now)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM accounts WHERE `+leaverMailboxes, userID); err != nil {
		return nil, fmt.Errorf("delete mailboxes: %w", err)
	}
	if err := handOver(ctx, tx, userID, tenantID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, userID); err != nil {
		return nil, fmt.Errorf("delete user: %w", err)
	}
	return changed, nil
}

// handOver gives, in tx, the authorship of a leaving member's rules and sender rules, which
// nothing shows but the database requires, to the team's longest-standing other member
// (one not waiting for removal themselves, when there is one). Their mailboxes are gone by
// then.
func handOver(ctx context.Context, tx *txn, userID, tenantID int64) error {
	var heir int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE tenant_id = ? AND id != ?
		ORDER BY id IN (SELECT user_id FROM forgotten), created_at, id LIMIT 1`, tenantID, userID).Scan(&heir); err != nil {
		return fmt.Errorf("find who takes over: %w", err)
	}
	for _, q := range []struct{ what, query string }{
		{"rules", `UPDATE rules SET user_id = ? WHERE user_id = ?`},
		{"sender rules", `UPDATE sender_rules SET user_id = ? WHERE user_id = ?`},
	} {
		if _, err := tx.ExecContext(ctx, q.query, heir, userID); err != nil {
			return fmt.Errorf("hand over %s: %w", q.what, err)
		}
	}
	return nil
}
