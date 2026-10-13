package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The roles a user has in their tenant (User.Role). Who may do what with each is the API's
// to enforce; the store only keeps them, and keeps a team from losing its last owner.
const (
	// RoleOwner: billing and the plan, members, everything an admin does.
	RoleOwner = "owner"
	// RoleAdmin: mailboxes, the shared rules and sender rules, members, settings, the audit log.
	RoleAdmin = "admin"
	// RoleMember: their own mailboxes, sorted by the shared rules.
	RoleMember = "member"
)

// Roles are the roles, from the most a user may do to the least.
var Roles = []string{RoleOwner, RoleAdmin, RoleMember}

// RoleRank orders the roles: an owner ranks above an admin, an admin above a member. An
// unknown role ranks below them all.
func RoleRank(role string) int {
	switch role {
	case RoleOwner:
		return 3
	case RoleAdmin:
		return 2
	case RoleMember:
		return 1
	}
	return 0
}

// RoleName is a role as people read it: "Owner".
func RoleName(role string) string {
	if role == "" {
		return ""
	}
	return strings.ToUpper(role[:1]) + role[1:]
}

var (
	// ErrLastOwner is a change that would leave a team with no owner.
	ErrLastOwner = errors.New("the team's last owner")
	// ErrBadRole is a role that is not one of Roles.
	ErrBadRole = errors.New("unknown role")
	// ErrAlreadyMember is an invitation for someone who is in the team already.
	ErrAlreadyMember = errors.New("already a member of the team")
	// ErrAlreadyInvited is an invitation for an address that has one waiting.
	ErrAlreadyInvited = errors.New("already invited")
	// ErrHasAccount is an invitation for an address that has a MailRules account of its own,
	// in any tenant: one person is one user in one tenant.
	ErrHasAccount = errors.New("the address has a MailRules account")
	// ErrSeatLimit is what a *SeatLimitError matches with errors.Is.
	ErrSeatLimit = errors.New("seat limit reached")
)

// InvitationDays is how long an invitation waits to be accepted before it expires; once the
// sign-in service says it was accepted, it waits as long again for the person's first
// sign-in.
const InvitationDays = 7

// SeatLimit is how many people a tenant on plan p may have: members, and invitations
// waiting, together. NoLimit is any number. A nil SeatLimit limits nothing.
type SeatLimit func(p PlanState) int

// SeatLimitError is a member or an invitation refused because every seat of the tenant's
// plan is taken.
type SeatLimitError struct {
	Limit int // the seats the plan has
	Used  int // members and invitations waiting
}

func (e *SeatLimitError) Error() string {
	if e.Limit == 1 {
		return "Your plan has one seat, and it is yours. Choose the Team plan on Plan and billing to add people."
	}
	return fmt.Sprintf("Your plan has %d seats, and all of them are taken by members and invitations waiting. Add seats on Plan and billing, or revoke an invitation or remove a member first.", e.Limit)
}

// Is makes errors.Is(err, ErrSeatLimit) hold.
func (e *SeatLimitError) Is(target error) bool { return target == ErrSeatLimit }

// Member is a person of a tenant, as the Members screen lists them. Those forgotten
// (ForgetIdentity), whose removal waits, are not members.
type Member struct {
	UserID   int64
	Email    string
	Role     string
	JoinedAt int64
}

// activeUsers is the condition that a users row is a member: not forgotten.
const activeUsers = `id NOT IN (SELECT user_id FROM forgotten)`

// Members lists the tenant's members, the longest-standing first.
func (s *Store) Members(ctx context.Context, tenantID int64) ([]Member, error) {
	ctx = WithTenant(ctx, tenantID)
	rows, err := s.db.QueryContext(ctx, `SELECT id, email, role, created_at FROM users
		WHERE tenant_id = ? AND `+activeUsers+` ORDER BY created_at, id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.Email, &m.Role, &m.JoinedAt); err != nil {
			return nil, fmt.Errorf("list members: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	return out, nil
}

// SetRole gives one of the tenant's members role. It fails, changing nothing, with
// ErrNotFound for a user who is not a member, ErrBadRole, and ErrLastOwner when it would
// leave the tenant with no owner.
func (s *Store) SetRole(ctx context.Context, tenantID, userID int64, role string, now int64) error {
	if RoleRank(role) == 0 {
		return ErrBadRole
	}
	return s.change(ctx, tenantID, "set role", func(ctx context.Context, tx *txn) error {
		var email, was string
		err := tx.QueryRowContext(ctx, `SELECT email, role FROM users WHERE id = ? AND tenant_id = ? AND `+activeUsers,
			userID, tenantID).Scan(&email, &was)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("set role: %w", err)
		}
		if was == role {
			return nil
		}
		if was == RoleOwner {
			if err := keepAnOwner(ctx, tx, tenantID, userID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET role = ? WHERE id = ?`, role, userID); err != nil {
			return fmt.Errorf("set role: %w", err)
		}
		return record(ctx, tx, tenantID, now, AuditMemberRoleChanged, email, RoleName(was)+" to "+RoleName(role))
	})
}

// keepAnOwner answers ErrLastOwner, read in tx, when the tenant has no owner but userID.
func keepAnOwner(ctx context.Context, tx *txn, tenantID, userID int64) error {
	var others int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE tenant_id = ? AND role = ? AND id != ? AND `+activeUsers,
		tenantID, RoleOwner, userID).Scan(&others); err != nil {
		return fmt.Errorf("count owners: %w", err)
	}
	if others == 0 {
		return ErrLastOwner
	}
	return nil
}

// LastOwner reports whether the user is the only owner of a tenant that has other members:
// a team that would be left with nobody to manage its plan and members.
func (s *Store) LastOwner(ctx context.Context, u User) (bool, error) {
	if u.Role != RoleOwner {
		return false, nil
	}
	ctx = WithTenant(ctx, u.TenantID)
	var owners, others int
	if err := s.db.QueryRowContext(ctx, `SELECT
		  (SELECT COUNT(*) FROM users WHERE tenant_id = ?1 AND role = ?2 AND id != ?3 AND `+activeUsers+`),
		  (SELECT COUNT(*) FROM users WHERE tenant_id = ?1 AND id != ?3 AND `+activeUsers+`)`,
		u.TenantID, RoleOwner, u.ID).Scan(&owners, &others); err != nil {
		return false, fmt.Errorf("count owners: %w", err)
	}
	return owners == 0 && others > 0, nil
}

// promoteHeir makes, in tx, the longest-standing remaining member an owner when userID, who
// is leaving, is the tenant's last owner and anyone else remains: an admin if there is one,
// else a member. The change is recorded under MailRules.
func promoteHeir(ctx context.Context, tx *txn, tenantID, userID, now int64) error {
	var role string
	if err := tx.QueryRowContext(ctx, `SELECT role FROM users WHERE id = ?`, userID).Scan(&role); err != nil {
		return fmt.Errorf("read role: %w", err)
	}
	if role != RoleOwner || !errors.Is(keepAnOwner(ctx, tx, tenantID, userID), ErrLastOwner) {
		return nil
	}
	var heir int64
	var email, was string
	err := tx.QueryRowContext(ctx, `SELECT id, email, role FROM users WHERE tenant_id = ? AND id != ? AND `+activeUsers+`
		ORDER BY CASE role WHEN 'admin' THEN 0 ELSE 1 END, created_at, id LIMIT 1`, tenantID, userID).Scan(&heir, &email, &was)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // nobody stays
	}
	if err != nil {
		return fmt.Errorf("find the next owner: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET role = ? WHERE id = ?`, RoleOwner, heir); err != nil {
		return fmt.Errorf("make the next owner: %w", err)
	}
	return recordAs(ctx, tx, tenantID, now, Actor{Name: "MailRules"}, AuditMemberRoleChanged, email,
		RoleName(was)+" to Owner: the team's last owner left")
}

// SeatsUsed counts the seats the tenant has taken at now: its members (a removed or
// forgotten person, whose removal waits, is not one) and the invitations waiting. It is the
// one count of seats: the Members screen, Plan and billing, the downgrade check and the
// checks that refuse a person over the seats (takeSeat) all use it.
func (s *Store) SeatsUsed(ctx context.Context, tenantID, now int64) (int, error) {
	return seatsUsed(WithTenant(ctx, tenantID), s.db, tenantID, now)
}

// seatsUsed is SeatsUsed, read with q.
func seatsUsed(ctx context.Context, q querier, tenantID, now int64) (int, error) {
	var members, invited int
	if err := q.QueryRowContext(ctx, `SELECT
		  (SELECT COUNT(*) FROM users WHERE tenant_id = ?1 AND `+activeUsers+`),
		  (SELECT COUNT(*) FROM invitations WHERE tenant_id = ?1 AND expires_at > ?2)`,
		tenantID, now).Scan(&members, &invited); err != nil {
		return 0, fmt.Errorf("count seats: %w", err)
	}
	return members + invited, nil
}

// takeSeat answers a *SeatLimitError, read in tx, when the tenant has no seat left for one
// more person.
func takeSeat(ctx context.Context, tx *txn, tenantID, now int64, seats SeatLimit) error {
	if seats == nil {
		return nil
	}
	p, err := tenantPlan(ctx, tx, tenantID, now)
	if err != nil {
		return err
	}
	limit := seats(p)
	if limit < 0 {
		return nil
	}
	used, err := seatsUsed(ctx, tx, tenantID, now)
	if err != nil {
		return err
	}
	if used >= limit {
		return &SeatLimitError{Limit: limit, Used: used}
	}
	return nil
}

// Invitation is an invitation to join a tenant, waiting.
type Invitation struct {
	ID, TenantID int64
	Email        string
	Role         string
	// InvitedBy is the user who sent it, and InvitedByEmail their address; 0 and empty once
	// they left.
	InvitedBy      int64
	InvitedByEmail string
	// Record is the module's own reference to it at the service that sent it.
	Record     string
	CreatedAt  int64
	ExpiresAt  int64
	AcceptedAt int64 // 0 until the sign-in service says it was accepted
}

const invitationCols = `i.id, i.tenant_id, i.email, i.role, COALESCE(i.invited_by, 0), COALESCE(u.email, ''), i.record,
	i.created_at, i.expires_at, COALESCE(i.accepted_at, 0)`

func scanInvitation(row interface{ Scan(...any) error }) (Invitation, error) {
	var v Invitation
	err := row.Scan(&v.ID, &v.TenantID, &v.Email, &v.Role, &v.InvitedBy, &v.InvitedByEmail, &v.Record, &v.CreatedAt, &v.ExpiresAt, &v.AcceptedAt)
	return v, err
}

// CreateInvitation stores an invitation of the tenant for inv.Email with inv.Role, sent by
// inv.InvitedBy, expiring InvitationDays from now, and returns it. One that expired for the
// same address is replaced. It fails, storing nothing, with ErrBadRole, ErrAlreadyMember
// when a member has the address, ErrHasAccount when a user of any other tenant has it (the
// person must delete that account first), ErrAlreadyInvited when an invitation for it waits,
// and a *SeatLimitError when the tenant's plan has no seat left (the count and the insert are
// one write transaction, so two invitations sent at once cannot both take the last seat).
func (s *Store) CreateInvitation(ctx context.Context, tenantID int64, inv Invitation, seats SeatLimit, now int64) (Invitation, error) {
	if RoleRank(inv.Role) == 0 {
		return Invitation{}, ErrBadRole
	}
	err := s.change(ctx, tenantID, "invite", func(ctx context.Context, tx *txn) error {
		var member bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE tenant_id = ? AND lower(email) = lower(?) AND `+activeUsers+`)`,
			tenantID, inv.Email).Scan(&member); err != nil {
			return fmt.Errorf("invite: %w", err)
		}
		if member {
			return ErrAlreadyMember
		}
		// Across tenants, on Postgres through the definer function: a forgotten person's
		// address is free already (forgottenEmail).
		inUse, err := s.emailInUse(ctx, tx, inv.Email, 0)
		if err != nil {
			return fmt.Errorf("invite: %w", err)
		}
		if inUse {
			return ErrHasAccount
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM invitations WHERE tenant_id = ? AND lower(email) = lower(?) AND expires_at <= ?`,
			tenantID, inv.Email, now); err != nil {
			return fmt.Errorf("invite: %w", err)
		}
		var waiting bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM invitations WHERE tenant_id = ? AND lower(email) = lower(?))`,
			tenantID, inv.Email).Scan(&waiting); err != nil {
			return fmt.Errorf("invite: %w", err)
		}
		if waiting {
			return ErrAlreadyInvited
		}
		if err := takeSeat(ctx, tx, tenantID, now, seats); err != nil {
			return err
		}
		inv.TenantID, inv.CreatedAt, inv.ExpiresAt = tenantID, now, now+InvitationDays*day
		if err := tx.QueryRowContext(ctx, `INSERT INTO invitations (tenant_id, email, role, invited_by, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?) RETURNING id`, tenantID, inv.Email, inv.Role, null(inv.InvitedBy), inv.CreatedAt, inv.ExpiresAt).Scan(&inv.ID); err != nil {
			return fmt.Errorf("invite: %w", err)
		}
		return record(ctx, tx, tenantID, now, AuditInvitationCreated, inv.Email, "as "+RoleName(inv.Role))
	})
	if err != nil {
		return Invitation{}, err
	}
	return inv, nil
}

// SetInvitationRecord keeps the module's reference to one of the tenant's invitations.
// ErrNotFound for one it does not have.
func (s *Store) SetInvitationRecord(ctx context.Context, tenantID, id int64, rec string) error {
	ctx = WithTenant(ctx, tenantID)
	res, err := s.db.ExecContext(ctx, `UPDATE invitations SET record = ? WHERE id = ? AND tenant_id = ?`, rec, id, tenantID)
	if err != nil {
		return fmt.Errorf("keep the invitation's record: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Invitations lists the tenant's invitations that have not expired at now, oldest first.
func (s *Store) Invitations(ctx context.Context, tenantID, now int64) ([]Invitation, error) {
	ctx = WithTenant(ctx, tenantID)
	rows, err := s.db.QueryContext(ctx, `SELECT `+invitationCols+` FROM invitations i LEFT JOIN users u ON u.id = i.invited_by
		WHERE i.tenant_id = ? AND i.expires_at > ? ORDER BY i.created_at, i.id`, tenantID, now)
	if err != nil {
		return nil, fmt.Errorf("list invitations: %w", err)
	}
	defer rows.Close()
	var out []Invitation
	for rows.Next() {
		v, err := scanInvitation(rows)
		if err != nil {
			return nil, fmt.Errorf("list invitations: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list invitations: %w", err)
	}
	return out, nil
}

// RevokeInvitation deletes one of the tenant's invitations and returns it, recorded as
// revoked, or as withdrawn with why when the daemon takes it back itself (it could not be
// sent). The seat it held is checked for (queueSeatCheck). ErrNotFound for one it does not
// have.
func (s *Store) RevokeInvitation(ctx context.Context, tenantID, id int64, why string, now int64) (Invitation, error) {
	var v Invitation
	err := s.change(ctx, tenantID, "revoke invitation", func(ctx context.Context, tx *txn) error {
		var err error
		v, err = scanInvitation(tx.QueryRowContext(ctx, `SELECT `+invitationCols+` FROM invitations i LEFT JOIN users u ON u.id = i.invited_by
			WHERE i.id = ? AND i.tenant_id = ?`, id, tenantID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("revoke invitation: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM invitations WHERE id = ?`, id); err != nil {
			return fmt.Errorf("revoke invitation: %w", err)
		}
		if err := queueSeatCheck(ctx, tx, tenantID, now); err != nil {
			return err
		}
		if why != "" {
			return record(ctx, tx, tenantID, now, AuditInvitationWithdrawn, v.Email, why)
		}
		return record(ctx, tx, tenantID, now, AuditInvitationRevoked, v.Email, "")
	})
	return v, err
}

// AcceptInvitation records that the person invited to the tenant at email accepted, as the
// sign-in service reports it. When they already joined (they signed in under the team's
// organisation before the report came), they get the invitation's role now and it is used
// up; otherwise it waits InvitationDays more for their first sign-in, which joins them with
// its role. ErrNotFound when the tenant has no invitation for the address that has not
// expired.
func (s *Store) AcceptInvitation(ctx context.Context, tenantID int64, email string, now int64) error {
	return s.change(ctx, tenantID, "accept invitation", func(ctx context.Context, tx *txn) error {
		var id int64
		var role string
		err := tx.QueryRowContext(ctx, `SELECT id, role FROM invitations WHERE tenant_id = ? AND lower(email) = lower(?) AND expires_at > ?`,
			tenantID, email, now).Scan(&id, &role)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("accept invitation: %w", err)
		}
		var userID int64
		var was string
		err = tx.QueryRowContext(ctx, `SELECT id, role FROM users WHERE tenant_id = ? AND lower(email) = lower(?) AND `+activeUsers,
			tenantID, email).Scan(&userID, &was)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx, `UPDATE invitations SET accepted_at = ?, expires_at = ? WHERE id = ?`,
				now, now+InvitationDays*day, id); err != nil {
				return fmt.Errorf("accept invitation: %w", err)
			}
			return record(ctx, tx, tenantID, now, AuditInvitationAccepted, email, "joins as "+RoleName(role)+" at their first sign-in")
		case err != nil:
			return fmt.Errorf("accept invitation: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM invitations WHERE id = ?`, id); err != nil {
			return fmt.Errorf("accept invitation: %w", err)
		}
		if was != role {
			if was == RoleOwner {
				if err := keepAnOwner(ctx, tx, tenantID, userID); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE users SET role = ? WHERE id = ?`, role, userID); err != nil {
				return fmt.Errorf("accept invitation: %w", err)
			}
		}
		return record(ctx, tx, tenantID, now, AuditInvitationAccepted, email, "joined as "+RoleName(role))
	})
}

// joinRole is, in tx, the role a new user of an existing tenant gets at now: the role of
// the invitation waiting for their email, which is then used up, else member, for which a
// seat must be free. how is the audit log's detail of the join: the inviter is named by
// their user id, never their address (no audit detail holds one).
func joinRole(ctx context.Context, tx *txn, tenantID int64, email string, now int64, seats SeatLimit) (role, how string, err error) {
	var id int64
	var invitedBy sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT id, role, invited_by FROM invitations
		WHERE tenant_id = ? AND lower(email) = lower(?) AND expires_at > ?`, tenantID, email, now).Scan(&id, &role, &invitedBy)
	if errors.Is(err, sql.ErrNoRows) {
		if err := takeSeat(ctx, tx, tenantID, now, seats); err != nil {
			return "", "", err
		}
		return RoleMember, "as " + RoleName(RoleMember) + ", without an invitation", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("find invitation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM invitations WHERE id = ?`, id); err != nil {
		return "", "", fmt.Errorf("use invitation: %w", err)
	}
	how = "as " + RoleName(role) + ", invited"
	if invitedBy.Valid {
		how += " by member #" + strconv.FormatInt(invitedBy.Int64, 10)
	}
	return role, how, nil
}

// retainInvitations deletes the tenant's invitations that expired; the seats they held are
// checked for (queueSeatCheck).
func (s *Store) retainInvitations(ctx context.Context, tenantID, now int64) error {
	return s.change(ctx, tenantID, "delete expired invitations", func(ctx context.Context, tx *txn) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM invitations WHERE tenant_id = ? AND expires_at <= ?`, tenantID, now)
		if err != nil {
			return fmt.Errorf("delete expired invitations: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		return queueSeatCheck(ctx, tx, tenantID, now)
	})
}
