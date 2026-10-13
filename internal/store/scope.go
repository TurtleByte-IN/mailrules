package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// Tenant scope (MAI-150). On Postgres every row is its tenant's: the policies show a
// statement only the rows of the tenant its transaction is scoped to, and none when it is
// scoped to none. A store method finds that tenant in one of three ways:
//
//   - ctx carries it (WithTenant): a request scopes its context to the signed-in person's
//     tenant, a mailbox's worker to the mailbox's. It always wins, so code serving one
//     tenant cannot read another's even through a method that takes a bare id.
//   - The method is given it: a tenantID or Viewer argument scopes ctx (WithTenant). One
//     that disagrees with the tenant ctx already carries fails with ErrTenantScope.
//   - The method addresses one row by its id alone (Account, Message, Batch...): tenantFor
//     looks the row's tenant up, through the definer function mailrules_tenant_of, which
//     tells only that number.
//
// Work across tenants (a sweep, an outbox, the startup checks) runs once per tenant
// (eachTenant), and a lookup before sign-in (a session, an email, an identity) asks
// mailrules_tenant_of first. Those definer functions are the only way across tenants.
// SQLite keeps no tenant apart: there scoping is checked the same way and otherwise does
// nothing, and eachTenant runs once, unscoped.

// ErrTenantScope means a method was asked for one tenant inside the scope of another.
var ErrTenantScope = errors.New("store: a tenant outside the current scope")

type scopeKey struct{}

type tenantScope struct {
	id  int64
	err error // the scope is unusable: statements fail with it
}

// WithTenant scopes ctx to the tenant: everything the store runs with it reads and writes
// that tenant's rows only. Inside a scope for another tenant, statements run with the
// result fail with ErrTenantScope. Tenant 0 adds nothing.
func WithTenant(ctx context.Context, tenantID int64) context.Context {
	cur, _ := ctx.Value(scopeKey{}).(tenantScope)
	switch {
	case tenantID == 0 || cur.err != nil || cur.id == tenantID:
		return ctx
	case cur.id != 0:
		return context.WithValue(ctx, scopeKey{}, tenantScope{id: cur.id,
			err: fmt.Errorf("%w: tenant %d asked for in the scope of tenant %d", ErrTenantScope, tenantID, cur.id)})
	}
	return context.WithValue(ctx, scopeKey{}, tenantScope{id: tenantID})
}

// TenantOf is the tenant ctx is scoped to, 0 for none.
func TenantOf(ctx context.Context) int64 {
	s, _ := ctx.Value(scopeKey{}).(tenantScope)
	return s.id
}

// scopeOf is the tenant the statements of ctx run in (0: none), or why they cannot run.
func scopeOf(ctx context.Context) (int64, error) {
	s, _ := ctx.Value(scopeKey{}).(tenantScope)
	return s.id, s.err
}

// failScope makes every statement run with ctx fail with err: how a lookup that could not be
// made reaches the caller without each method checking for it.
func failScope(ctx context.Context, err error) context.Context {
	return context.WithValue(ctx, scopeKey{}, tenantScope{err: err})
}

// Kinds of row mailrules_tenant_of finds the tenant of. The Postgres baseline (0023) lists
// the same.
const (
	ofAccount     = "account"
	ofMessage     = "message"
	ofAction      = "action"
	ofBatch       = "batch"
	ofUser        = "user"
	ofDestination = "destination"
	ofNotice      = "notice"
	ofMove        = "move"
	ofQuestion    = "question"
	ofRemoved     = "removed"
	ofSummary     = "summary"
	ofSenderRule  = "sender_rule"
	ofRule        = "rule"
	ofSession     = "session"
	ofEmail       = "email"
	ofIdentity    = "identity"
	ofExternal    = "external"
	ofFirstUser   = "first_user"
)

// tenantFor scopes ctx to the tenant of the row of kind keyed by key (and key2), unless ctx
// is scoped already. A row that does not exist leaves ctx unscoped, so the method finds
// nothing, as it would have; a lookup that fails makes the method's statements fail. On
// SQLite it returns ctx as it is.
func (s *Store) tenantFor(ctx context.Context, kind string, key any, key2 ...any) context.Context {
	if s.db.d != dialectPostgres || TenantOf(ctx) != 0 {
		return ctx
	}
	if _, err := scopeOf(ctx); err != nil {
		return ctx
	}
	t, err := s.lookupTenant(ctx, kind, key, key2...)
	if err != nil {
		return failScope(ctx, err)
	}
	return WithTenant(ctx, t)
}

// lookupTenant is the tenant of the row of kind keyed by key (and key2), 0 when there is
// none: Postgres only.
func (s *Store) lookupTenant(ctx context.Context, kind string, key any, key2 ...any) (int64, error) {
	var k2 any
	if len(key2) > 0 {
		k2 = fmt.Sprint(key2[0])
	}
	var k1 any
	if key != nil {
		k1 = fmt.Sprint(key)
	}
	var t *int64
	// Run unscoped: the function sees across tenants, and says only the number.
	if err := s.db.QueryRowContext(context.WithValue(ctx, scopeKey{}, tenantScope{}),
		`SELECT mailrules_tenant_of(CAST(? AS text), CAST(? AS text), CAST(? AS text))`, kind, k1, k2).Scan(&t); err != nil {
		return 0, fmt.Errorf("find the tenant of %s %v: %w", kind, key, err)
	}
	if t == nil {
		return 0, nil
	}
	return *t, nil
}

// Kinds of work mailrules_tenants lists the tenants of.
const (
	tenantsAll       = "all"
	tenantsNotices   = "notices"
	tenantsMoves     = "moves"
	tenantsRemoved   = "removed"
	tenantsForgotten = "forgotten"
	tenantsDrafts    = "drafts"
)

// eachTenant calls fn once for every tenant with work of kind, with ctx scoped to it,
// lowest id first; within a scope, once for that tenant alone. On SQLite, which keeps no
// tenant apart, it calls fn once with ctx as it is and tenant 0.
func (s *Store) eachTenant(ctx context.Context, kind string, fn func(ctx context.Context, tenantID int64) error) error {
	if t := TenantOf(ctx); t != 0 || s.db.d != dialectPostgres {
		return fn(ctx, t)
	}
	if _, err := scopeOf(ctx); err != nil {
		return err
	}
	rs, err := s.db.QueryContext(ctx, `SELECT t FROM mailrules_tenants(?) AS t`, kind)
	if err != nil {
		return fmt.Errorf("list tenants with %s: %w", kind, err)
	}
	var ids []int64
	for rs.Next() {
		var id int64
		if err := rs.Scan(&id); err != nil {
			_ = rs.Close()
			return fmt.Errorf("list tenants with %s: %w", kind, err)
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rs.Err(), rs.Close()); err != nil {
		return fmt.Errorf("list tenants with %s: %w", kind, err)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if err := fn(WithTenant(ctx, id), id); err != nil {
			return err
		}
	}
	return nil
}
