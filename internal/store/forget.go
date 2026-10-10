package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/TurtleByte-IN/mailrules/internal/rules"
)

// ForgetDays is how long what MailRules kept of a forgotten person waits before it is
// removed. Signing in again with the same identity before then cancels the removal.
const ForgetDays = 30

// forgottenEmail is the address a forgotten user holds while their removal waits: unique,
// in the reserved .invalid domain, so no sign-in service can vouch for it and the person's
// own email is free at once. Signing in again sets the real one back.
func forgottenEmail(userID int64) string {
	return "forgotten-" + strconv.FormatInt(userID, 10) + "@forgotten.invalid"
}

// ForgetIdentity is a person the sign-in service deleted. In one transaction it ends every
// session of the identity's user, frees their email (a new identity may sign up with it at
// once) and schedules the removal of their data for ForgetDays from now. Their data is
// left as it is until then. An identity nobody has is not an error, and forgetting one
// again changes nothing, the removal time included. Only users made from an identity are
// reached, so the self-host admin never is.
func (s *Store) ForgetIdentity(ctx context.Context, provider, subject string, now int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("forget identity: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	// The first statement writes, so the transaction holds the write lock from the start: one
	// that read first could not write after another connection had (SQLITE_BUSY_SNAPSHOT).
	const user = `SELECT u.id FROM identities i JOIN users u ON u.id = i.user_id
		WHERE i.provider = ? AND i.subject = ? AND u.tenant_id != ?`
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id IN (`+user+`)`, provider, subject, SelfHostTenant); err != nil {
		return fmt.Errorf("forget identity: end sessions: %w", err)
	}
	var userID int64
	err = tx.QueryRowContext(ctx, user, provider, subject, SelfHostTenant).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("forget identity: find user: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET email = ? WHERE id = ?`, forgottenEmail(userID), userID); err != nil {
		return fmt.Errorf("forget identity: free email: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO forgotten (user_id, forgotten_at, remove_at) VALUES (?, ?, ?) ON CONFLICT (user_id) DO NOTHING`,
		userID, now, now+ForgetDays*day); err != nil {
		return fmt.Errorf("forget identity: schedule removal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("forget identity: %w", err)
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
	rows, err := s.db.QueryContext(ctx, `SELECT f.user_id, u.tenant_id, f.remove_at
		FROM forgotten f JOIN users u ON u.id = f.user_id WHERE f.remove_at <= ? ORDER BY f.remove_at, f.user_id`, now)
	if err != nil {
		return nil, fmt.Errorf("list removals due: %w", err)
	}
	defer rows.Close()
	var out []Forgotten
	for rows.Next() {
		var f Forgotten
		if err := rows.Scan(&f.UserID, &f.TenantID, &f.RemoveAt); err != nil {
			return nil, fmt.Errorf("list removals due: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list removals due: %w", err)
	}
	return out, nil
}

// RemovalAccounts lists the mailboxes removing the user would delete as things stand: every
// mailbox of their tenant when they are its only member, else only the ones they own and
// have not shared. Their supervisors are stopped before RemoveForgotten runs.
func (s *Store) RemovalAccounts(ctx context.Context, userID int64) ([]int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
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

// leaverMailboxes is which of a leaving team member's mailboxes are removed with them: the
// ones they own and did not share. A mailbox they shared is the team's and stays, handed
// over with their other team data (handOver).
const leaverMailboxes = `user_id = ? AND shared = 0`

// removalAccounts reports, in tx, whether the user is their tenant's only member, and the
// mailboxes their removal deletes.
func removalAccounts(ctx context.Context, tx *sql.Tx, userID, tenantID int64) (alone bool, ids []int64, err error) {
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
// keys included) and usage ledger, the user with their sessions, identity and summaries,
// and the tenant itself. A member of a team takes only what is theirs alone: the mailboxes
// they own and did not share (the team's rules naming one are rewritten as DeleteAccount
// does), their sessions, identity, summaries and user row; what the team keeps of theirs
// is handed over (handOver). It answers ErrNotFound, changing nothing, for a user who is
// not forgotten or not yet due: one who signed in again, or was removed already.
func (s *Store) RemoveForgotten(ctx context.Context, userID, now int64) (Removal, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Removal{}, fmt.Errorf("remove forgotten user: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	// Deleting the forgotten row first takes the write lock at once (see ForgetIdentity) and
	// says whether the removal is still due; deleting the user would cascade to it anyway.
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

// removeTenant deletes, in tx, a tenant and everything in it. Deleting a mailbox cascades
// to its folders, contacts, messages, decisions, actions and corrections (and to its rules,
// which go anyway); deleting a user cascades to their sessions, identity, summaries and
// forgotten row.
func removeTenant(ctx context.Context, tx *sql.Tx, tenantID int64) error {
	for _, q := range []struct{ what, query string }{
		{"mailboxes", `DELETE FROM accounts WHERE tenant_id = ?`},
		{"sender rules", `DELETE FROM sender_rules WHERE tenant_id = ?`},
		{"rules", `DELETE FROM rules WHERE tenant_id = ?`},
		{"batches", `DELETE FROM batches WHERE tenant_id = ?`},
		{"settings", `DELETE FROM settings WHERE tenant_id = ?`},
		{"usage", `DELETE FROM usage_daily WHERE tenant_id = ?`},
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
// (theirs alone), after rewriting the team's rules that name one, then what the team keeps
// of theirs is handed over and their user row deleted. It returns the rules it rewrote.
func removeMember(ctx context.Context, tx *sql.Tx, userID, tenantID int64, accounts []int64, now int64) ([]rules.Rule, error) {
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

// handOver gives, in tx, what a leaving member's team keeps of theirs to the team's
// longest-standing other member (one not waiting for removal themselves, when there is
// one): the mailboxes they own and shared, which the heir may then change or remove as
// the owner, and the authorship of rules and sender rules, which nothing shows.
func handOver(ctx context.Context, tx *sql.Tx, userID, tenantID int64) error {
	var heir int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE tenant_id = ? AND id != ?
		ORDER BY id IN (SELECT user_id FROM forgotten), created_at, id LIMIT 1`, tenantID, userID).Scan(&heir); err != nil {
		return fmt.Errorf("find who takes over: %w", err)
	}
	for _, q := range []struct{ what, query string }{
		{"mailboxes", `UPDATE accounts SET user_id = ? WHERE user_id = ?`},
		{"rules", `UPDATE rules SET user_id = ? WHERE user_id = ?`},
		{"sender rules", `UPDATE sender_rules SET user_id = ? WHERE user_id = ?`},
	} {
		if _, err := tx.ExecContext(ctx, q.query, heir, userID); err != nil {
			return fmt.Errorf("hand over %s: %w", q.what, err)
		}
	}
	return nil
}
