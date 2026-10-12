package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// limitsOf is the build's ext.Module.Limits, or nil when no module limits tenants.
func limitsOf(modules []ext.Module) func(ctx context.Context, tenantID int64) (ext.Limits, error) {
	for _, m := range modules {
		if m.Limits != nil {
			return m.Limits
		}
	}
	return nil
}

// noLimits is what a tenant may use in a build whose modules limit nothing: self-host.
var noLimits = ext.Limits{Mailboxes: ext.NoLimit, AIEmails: ext.NoLimit}

// limits are what the tenant may use, as the build's module says.
func (s *server) limits(ctx context.Context, tenantID int64) (ext.Limits, error) {
	limits := limitsOf(s.Modules)
	if limits == nil {
		return noLimits, nil
	}
	l, err := limits(ctx, tenantID)
	if err != nil {
		return ext.Limits{}, fmt.Errorf("the tenant's limits: %w", err)
	}
	return l, nil
}

// Allowance is pipeline.Pipeline.Allowance for a build's modules: whether the tenant has used
// up the month's allowance of emails a model decides on keys the operator holds, at now. It
// is nil when no module limits tenants.
func Allowance(st *store.Store, modules []ext.Module) func(ctx context.Context, tenantID int64, now time.Time) (bool, error) {
	limits := limitsOf(modules)
	if limits == nil {
		return nil
	}
	return func(ctx context.Context, tenantID int64, now time.Time) (bool, error) {
		l, err := limits(ctx, tenantID)
		if err != nil {
			return false, fmt.Errorf("the tenant's limits: %w", err)
		}
		if l.AIEmails < 0 {
			return false, nil
		}
		used, err := st.AIEmails(ctx, tenantID, store.Month(now))
		return used >= l.AIEmails, err
	}
}

// cleanupMeter is Decider.Allowance and Decider.Spend for a cleanup check of the tenant's
// mail. Each email a model decides on keys the operator holds takes one of the tenant's
// one-time first-cleanup grant (ext.Limits.FirstCleanupEmails) while any is left, then counts
// against the month's allowance; the check stops asking such a model only once both are used
// up. Without a module that limits tenants there is no grant and no allowance, and such
// emails count against the month as live sorting's do.
func (s *server) cleanupMeter(tenantID int64) (allowance func(context.Context) (bool, error), spend func(context.Context) error) {
	spend = func(ctx context.Context) error {
		l, err := s.limits(ctx, tenantID)
		if err != nil {
			return err
		}
		_, err = s.store.SpendCleanupEmail(ctx, tenantID, s.now(), l.FirstCleanupEmails)
		return err
	}
	monthly := Allowance(s.store, s.Modules)
	if monthly == nil {
		return nil, spend
	}
	allowance = func(ctx context.Context) (bool, error) {
		l, err := s.limits(ctx, tenantID)
		if err != nil {
			return false, err
		}
		if l.FirstCleanupEmails > 0 {
			used, err := s.store.FirstCleanupUsed(ctx, tenantID)
			if err != nil || used < l.FirstCleanupEmails {
				return false, err
			}
		}
		return monthly(ctx, tenantID, s.now())
	}
	return allowance, spend
}

// mailboxRoom answers 409 mailbox_limit, and returns -1 with ok false, when the tenant has
// all the mailboxes it may; otherwise it returns how many it may have (store.NoLimit for any
// number), for saveAccount to check again as it stores the new one.
func (s *server) mailboxRoom(w http.ResponseWriter, r *http.Request, tenantID int64) (max int, ok bool) {
	max, err := s.mailboxRoomFor(r.Context(), tenantID)
	var full *store.MailboxLimitError
	switch {
	case errors.As(err, &full):
		writeError(w, http.StatusConflict, "mailbox_limit", full.Error(), "")
		return -1, false
	case err != nil:
		internalError(w, r, err)
		return -1, false
	}
	return max, true
}

// mailboxRoomFor is mailboxRoom's check: the tenant's mailbox limit, or a
// *store.MailboxLimitError when it has no room for another.
func (s *server) mailboxRoomFor(ctx context.Context, tenantID int64) (int, error) {
	l, err := s.limits(ctx, tenantID)
	if err != nil || l.Mailboxes < 0 {
		return l.Mailboxes, err
	}
	have, err := s.store.TenantAccounts(ctx, tenantID)
	if err != nil {
		return 0, err
	}
	if len(have) >= l.Mailboxes {
		return 0, &store.MailboxLimitError{Limit: l.Mailboxes, Connected: len(have)}
	}
	return l.Mailboxes, nil
}

type mailboxUseJSON struct {
	Used  int `json:"used"`
	Limit int `json:"limit"`
	// Refusal is the sentence a new mailbox is refused with while the tenant has no room for
	// one; nil while it has.
	Refusal *string `json:"refusal"`
}

// useJSON is how much of a one-time grant is used.
type useJSON struct {
	Used  int `json:"used"`
	Limit int `json:"limit"`
}

type aiEmailsJSON struct {
	Used     int    `json:"used"`
	Limit    int    `json:"limit"`
	Month    string `json:"month"`
	ResetsAt int64  `json:"resets_at"`
	Counted  bool   `json:"counted"`
	Paused   bool   `json:"paused"`
}

// handleLimits is the tenant's use against its limits, for Usage, Needs review and
// Mailboxes. A limit the tenant does not have is null: on self-host every one is.
func (s *server) handleLimits(w http.ResponseWriter, r *http.Request) {
	ctx, v := r.Context(), viewer(r)
	l, err := s.limits(ctx, v.TenantID)
	if err != nil {
		internalError(w, r, err)
		return
	}
	out := struct {
		Mailboxes    *mailboxUseJSON `json:"mailboxes"`
		AIEmails     *aiEmailsJSON   `json:"ai_emails"`
		FirstCleanup *useJSON        `json:"first_cleanup"`
		UpgradeURL   *string         `json:"upgrade_url"`
	}{}
	if l.UpgradeURL != "" {
		out.UpgradeURL = &l.UpgradeURL
	}
	if l.Mailboxes >= 0 {
		have, err := s.store.TenantAccounts(ctx, v.TenantID)
		if err != nil {
			internalError(w, r, err)
			return
		}
		out.Mailboxes = &mailboxUseJSON{Used: len(have), Limit: l.Mailboxes}
		if len(have) >= l.Mailboxes {
			why := (&store.MailboxLimitError{Limit: l.Mailboxes, Connected: len(have)}).Error()
			out.Mailboxes.Refusal = &why
		}
	}
	if l.AIEmails >= 0 {
		now := s.now().UTC()
		month := store.Month(now)
		used, err := s.store.AIEmails(ctx, v.TenantID, month)
		if err != nil {
			internalError(w, r, err)
			return
		}
		router, _ := s.modelSource().Live(ctx, v.TenantID)
		counted := router.OnOperatorKeys()
		out.AIEmails = &aiEmailsJSON{Used: used, Limit: l.AIEmails, Month: month,
			ResetsAt: time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC).Unix(),
			Counted:  counted, Paused: counted && used >= l.AIEmails && router.OwnKeysOnly() == nil}
	}
	if l.FirstCleanupEmails > 0 {
		used, err := s.store.FirstCleanupUsed(ctx, v.TenantID)
		if err != nil {
			internalError(w, r, err)
			return
		}
		out.FirstCleanup = &useJSON{Used: min(used, l.FirstCleanupEmails), Limit: l.FirstCleanupEmails}
	}
	writeJSON(w, http.StatusOK, out)
}
