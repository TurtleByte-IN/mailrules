package store

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// AuditDays is how long a tenant's audit log keeps a row: a year, then the retention job
// deletes it.
const AuditDays = 365

// What the audit log records (AuditEntry.Action): who changed the tenant's rules, sender
// rules, mailboxes, members, invitations, settings and plan.
const (
	AuditRuleCreated         = "rule.created"
	AuditRuleChanged         = "rule.changed"
	AuditRuleDeleted         = "rule.deleted"
	AuditRulesReordered      = "rules.reordered"
	AuditSenderRuleSet       = "sender_rule.set"
	AuditSenderRuleDeleted   = "sender_rule.deleted"
	AuditMailboxAdded        = "mailbox.added"
	AuditMailboxChanged      = "mailbox.changed"
	AuditMailboxRemoved      = "mailbox.removed"
	AuditSettingsChanged     = "settings.changed"
	AuditDestinationAdded    = "notification.added"
	AuditDestinationRemoved  = "notification.removed"
	AuditPlanChanged         = "plan.changed"
	AuditMemberJoined        = "member.joined"
	AuditMemberRoleChanged   = "member.role_changed"
	AuditMemberRemoved       = "member.removed"
	AuditInvitationCreated   = "invitation.created"
	AuditInvitationRevoked   = "invitation.revoked"
	AuditInvitationAccepted  = "invitation.accepted"
	AuditInvitationWithdrawn = "invitation.withdrawn"
)

// Actor is who makes the changes of a context (WithActor): a person, by their user and
// email, or a service by its name (UserID 0).
type Actor struct {
	UserID int64
	Name   string
}

type actorKey struct{}

// WithActor says who makes the changes the store is asked for with ctx: each change to what
// the audit log covers is then recorded under them, in its own transaction. A request
// carries the signed-in person; a module's call carries the service it speaks for. Changes
// made with no actor (the mailbox workers, the startup checks, the retention job) are not
// recorded.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// WithoutActor is ctx with no actor: what is changed with it is not recorded, such as a
// token a provider rotated in the middle of a request.
func WithoutActor(ctx context.Context) context.Context { return WithActor(ctx, Actor{}) }

// ActorOf is the actor of ctx; ok is false when it has none.
func ActorOf(ctx context.Context) (Actor, bool) {
	a, _ := ctx.Value(actorKey{}).(Actor)
	return a, a.Name != ""
}

// AuditEntry is one row of a tenant's audit log. It never holds email content or a secret.
type AuditEntry struct {
	ID      int64
	At      int64
	ActorID int64 // 0: a service, or a person whose user is gone
	Actor   string
	Action  string // one of the Audit* constants
	Target  string // what was changed, by name: a rule, a mailbox's address, a member's email
	Detail  string // how, in a few words; never an address (withoutAddresses)
}

// record writes, in q (the change's transaction), one audit row of the tenant under ctx's
// actor; it does nothing when ctx has none. at 0 is now.
func record(ctx context.Context, q querier, tenantID, at int64, action, target, detail string) error {
	a, ok := ActorOf(ctx)
	if !ok {
		return nil
	}
	return recordAs(ctx, q, tenantID, at, a, action, target, detail)
}

// addressLike is a word holding an @: an email address, or something that may be one.
var addressLike = regexp.MustCompile(`[^\s,;()<>"']*@[^\s,;()<>"']*`)

// withoutAddresses is detail with every address-like word in it replaced. A detail names
// people by user id and quotes names people chose (a mailbox's label, a rule's name, a
// folder), and such a name may be an address; forgetting a person rewrites actor and target
// only (store.forget), so detail must never hold one.
func withoutAddresses(detail string) string {
	if !strings.Contains(detail, "@") {
		return detail
	}
	return addressLike.ReplaceAllString(detail, "an address")
}

// recordAs is record under a, whatever ctx carries.
func recordAs(ctx context.Context, q querier, tenantID, at int64, a Actor, action, target, detail string) error {
	if at == 0 {
		at = time.Now().Unix()
	}
	if _, err := q.ExecContext(ctx,
		`INSERT INTO audit_log (tenant_id, at, actor_id, actor, action, target, detail) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		tenantID, at, null(a.UserID), a.Name, action, target, withoutAddresses(detail)); err != nil {
		return fmt.Errorf("record %s in the audit log: %w", action, err)
	}
	return nil
}

// change runs fn in one write transaction of the tenant, then commits: the change and the
// audit rows fn records go in together or not at all.
func (s *Store) change(ctx context.Context, tenantID int64, what string, fn func(ctx context.Context, tx *txn) error) error {
	ctx = WithTenant(ctx, tenantID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// AuditLog lists the tenant's audit log from since to until (unix seconds, both included;
// until 0 = now), oldest first.
func (s *Store) AuditLog(ctx context.Context, tenantID, since, until int64) ([]AuditEntry, error) {
	ctx = WithTenant(ctx, tenantID)
	if until == 0 {
		until = time.Now().Unix()
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, at, COALESCE(actor_id, 0), actor, action, target, detail FROM audit_log
		 WHERE tenant_id = ? AND at >= ? AND at <= ? ORDER BY id`, tenantID, since, until)
	if err != nil {
		return nil, fmt.Errorf("read the audit log: %w", err)
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.At, &e.ActorID, &e.Actor, &e.Action, &e.Target, &e.Detail); err != nil {
			return nil, fmt.Errorf("read the audit log: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the audit log: %w", err)
	}
	return out, nil
}

// retainAudit deletes the tenant's audit rows older than AuditDays.
func (s *Store) retainAudit(ctx context.Context, tenantID, now int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM audit_log WHERE tenant_id = ? AND at < ?`, tenantID, now-AuditDays*day); err != nil {
		return fmt.Errorf("delete old audit rows: %w", err)
	}
	return nil
}
