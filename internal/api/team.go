package api

import (
	"context"
	"encoding/csv"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// Who may do what (MAI-234). Every route needs at least a member; the route table names
// the routes that need more (route.need), and a few handlers check more finely: who may
// change an owner, make a sender rule from a correction, or leave a team.
//
//	owner   the plan and billing, and everything an admin may
//	admin   the shared rules and sender rules, settings, notification destinations, members
//	        and invitations, the audit log
//	member  their own mailboxes and the mail they see; the shared rules sort it
//
// Self-host's admin, and everyone who existed before roles, is an owner.

// What a refusal says each role is needed for.
const (
	toEditRules     = "change the shared rules"
	toEditSenders   = "change the sender rules"
	toEditSettings  = "change settings"
	toManageMembers = "manage members and invitations"
	toSeeAudit      = "download the audit log"
	toManagePlan    = "manage the plan and billing"
	toUseModule     = "use this"
)

// allowed reports whether the signed-in person's role is at least need.
func allowed(r *http.Request, need string) bool {
	return store.RoleRank(user(r).Role) >= store.RoleRank(need)
}

// roleRequired answers 403 role_required: only need, or a role above it, may do what.
func roleRequired(w http.ResponseWriter, need, what string) {
	who := "an Owner or Admin"
	if need == store.RoleOwner {
		who = "an Owner"
	}
	writeError(w, http.StatusForbidden, "role_required", "Only "+who+" can "+what+".", "")
}

// requireRole serves h only to a signed-in person whose role is at least need.
func (s *server) requireRole(need, what string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !allowed(r, need) {
			roleRequired(w, need, what)
			return
		}
		h(w, r)
	}
}

// team is the build's Module.Team, or nil.
func (s *server) team() *ext.Team {
	for _, m := range s.Modules {
		if m.Team != nil {
			return m.Team
		}
	}
	return nil
}

// teamOnly refuses a request with 404 not_available in a build with no module that invites
// people, then with 403 plan_required while the tenant's plan does not include the team.
func (s *server) teamOnly(h http.HandlerFunc) http.HandlerFunc {
	gated := s.planFeatureOnly("team", h)
	return func(w http.ResponseWriter, r *http.Request) {
		if s.team() == nil {
			writeError(w, http.StatusNotFound, "not_available", "Members and invitations are not part of this build of MailRules.", "")
			return
		}
		gated(w, r)
	}
}

// seatLimit is how many people a tenant on a plan may have, for the store's checks; nil in
// a build that sells no plans.
func (s *server) seatLimit() store.SeatLimit {
	b := s.billing()
	if b == nil {
		return nil
	}
	return func(p store.PlanState) int { return limitsOf(b, p).Seats }
}

type memberJSON struct {
	ID       int64  `json:"id"`
	Email    string `json:"email"`
	Role     string `json:"role"`
	JoinedAt int64  `json:"joined_at"`
	You      bool   `json:"you"`
}

type seatsJSON struct {
	Used  int     `json:"used"`
	Limit *int    `json:"limit"` // null: any number
	Price *string `json:"price"` // what one more costs, when an invitation past limit buys it
}

type invitationJSON struct {
	ID         int64   `json:"id"`
	Email      string  `json:"email"`
	Role       string  `json:"role"`
	InvitedBy  *string `json:"invited_by"`
	CreatedAt  int64   `json:"created_at"`
	ExpiresAt  int64   `json:"expires_at"`
	AcceptedAt *int64  `json:"accepted_at"`
}

func toInvitationJSON(v store.Invitation) invitationJSON {
	return invitationJSON{ID: v.ID, Email: v.Email, Role: v.Role, InvitedBy: orNil(v.InvitedByEmail), CreatedAt: v.CreatedAt,
		ExpiresAt: v.ExpiresAt, AcceptedAt: ts(v.AcceptedAt)}
}

// handleMembers lists the team, with how many of its plan's seats are taken (store.SeatsUsed,
// the one count of them) and what one more costs when an invitation buys it.
func (s *server) handleMembers(w http.ResponseWriter, r *http.Request) {
	ctx, me, now := r.Context(), user(r), s.now().Unix()
	members, err := s.store.Members(ctx, me.TenantID)
	if err != nil {
		internalError(w, r, err)
		return
	}
	used, err := s.store.SeatsUsed(ctx, me.TenantID, now)
	if err != nil {
		internalError(w, r, err)
		return
	}
	l, err := s.limits(ctx, me.TenantID)
	if err != nil {
		internalError(w, r, err)
		return
	}
	seats := seatsJSON{Used: used, Limit: limitOrNil(l.Seats)}
	if b := s.billing(); b != nil {
		p, err := s.store.TenantPlan(ctx, me.TenantID, now)
		if err != nil {
			internalError(w, r, err)
			return
		}
		if t, ok := perSeat(b, p); ok {
			seats.Price = orNil(t.Prices[p.Interval])
		}
	}
	out := struct {
		Members []memberJSON `json:"members"`
		Seats   seatsJSON    `json:"seats"`
	}{Members: []memberJSON{}, Seats: seats}
	for _, m := range members {
		out.Members = append(out.Members, memberJSON{ID: m.UserID, Email: m.Email, Role: m.Role, JoinedAt: m.JoinedAt, You: m.UserID == me.ID})
	}
	writeJSON(w, http.StatusOK, out)
}

// member loads the member the {id} path names, answering 404 for anyone else.
func (s *server) member(w http.ResponseWriter, r *http.Request) (store.Member, bool) {
	id, ok := pathID(w, r, "id", "member")
	if !ok {
		return store.Member{}, false
	}
	members, err := s.store.Members(r.Context(), viewer(r).TenantID)
	if err != nil {
		internalError(w, r, err)
		return store.Member{}, false
	}
	i := slices.IndexFunc(members, func(m store.Member) bool { return m.UserID == id })
	if i < 0 {
		notFound(w, "member")
		return store.Member{}, false
	}
	return members[i], true
}

// handleMemberPatch changes a member's role. Only an owner makes someone an owner or changes
// an owner's role, and the team keeps at least one owner.
func (s *server) handleMemberPatch(w http.ResponseWriter, r *http.Request) {
	m, ok := s.member(w, r)
	if !ok {
		return
	}
	var in struct {
		Role string `json:"role"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if store.RoleRank(in.Role) == 0 {
		invalid(w, "role", "Choose Owner, Admin or Member.")
		return
	}
	if (in.Role == store.RoleOwner || m.Role == store.RoleOwner) && !allowed(r, store.RoleOwner) {
		roleRequired(w, store.RoleOwner, "make someone an Owner or change an Owner's role")
		return
	}
	err := s.store.SetRole(r.Context(), viewer(r).TenantID, m.UserID, in.Role, s.now().Unix())
	switch {
	case errors.Is(err, store.ErrLastOwner):
		lastOwner(w)
		return
	case errors.Is(err, store.ErrNotFound):
		notFound(w, "member")
		return
	case err != nil:
		internalError(w, r, err)
		return
	}
	m.Role = in.Role
	writeJSON(w, http.StatusOK, map[string]any{"member": memberJSON{ID: m.UserID, Email: m.Email, Role: m.Role, JoinedAt: m.JoinedAt, You: m.UserID == user(r).ID}})
}

// lastOwner answers 409 last_owner.
func lastOwner(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, "last_owner", "Your team needs at least one Owner. Make someone else an Owner first.", "")
}

// handleMemberDelete removes a member from the team: the sign-in service takes them out of
// the team's organisation first (ext.Team.RemoveMember), then they are forgotten as a
// person the service deleted is, their mailboxes going with them, except that they never
// get their data back (store.RemoveMember).
func (s *server) handleMemberDelete(w http.ResponseWriter, r *http.Request) {
	m, ok := s.member(w, r)
	if !ok {
		return
	}
	me := user(r)
	switch {
	case m.UserID == me.ID:
		writeError(w, http.StatusConflict, "remove_self", "You cannot remove yourself. To leave the team, delete your account in Settings.", "")
		return
	case m.Role == store.RoleOwner && !allowed(r, store.RoleOwner):
		roleRequired(w, store.RoleOwner, "remove an Owner")
		return
	}
	id, err := s.store.UserIdentity(r.Context(), m.UserID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusConflict, "not_removable", "This member was not made by the sign-in service, so they cannot be removed here.", "")
		return
	}
	if err != nil {
		internalError(w, r, err)
		return
	}
	_, org, err := s.store.TenantOrganisation(r.Context(), me.TenantID)
	if err != nil {
		internalError(w, r, err)
		return
	}
	if err := s.team().RemoveMember(r.Context(), me.TenantID, ext.Identity{Provider: id.Provider, Subject: id.Subject, Email: m.Email, Tenant: org}); err != nil {
		slog.ErrorContext(r.Context(), "the sign-in service did not remove a member; nothing was removed", "user_id", m.UserID, "error", err.Error())
		writeError(w, http.StatusBadGateway, "sign_in_error", "The sign-in service could not remove this member, so nothing was removed. Try again in a few minutes.", "")
		return
	}
	// Finish here even if the browser goes away: the service has removed them already.
	if err := s.forgetIdentity(context.WithoutCancel(r.Context()), id.Provider, id.Subject, s.store.RemoveMember); err != nil {
		internalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleInvitations lists the invitations waiting.
func (s *server) handleInvitations(w http.ResponseWriter, r *http.Request) {
	invited, err := s.store.Invitations(r.Context(), viewer(r).TenantID, s.now().Unix())
	if err != nil {
		internalError(w, r, err)
		return
	}
	out := make([]invitationJSON, len(invited))
	for i, v := range invited {
		out[i] = toInvitationJSON(v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"invitations": out})
}

// handleInvitationCreate invites a person by email: the daemon keeps the invitation, which
// takes a seat, and the module has the sign-in service send it. Only an owner invites an
// owner. Past the paid seats of a plan sold by the seat, the billing module is asked for the
// seats the team then needs first, and the invitation is kept only once it agrees; a seat
// bought for an invitation that is then not kept is given back (store.QueueSeatCheck).
func (s *server) handleInvitationCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	in.Email = strings.TrimSpace(in.Email)
	switch {
	case !strings.Contains(in.Email, "@") || len(in.Email) > 254 || strings.ContainsAny(in.Email, " \t\r\n,;<>"):
		invalid(w, "email", "Enter one email address.")
		return
	case store.RoleRank(in.Role) == 0:
		invalid(w, "role", "Choose Owner, Admin or Member.")
		return
	case in.Role == store.RoleOwner && !allowed(r, store.RoleOwner):
		roleRequired(w, store.RoleOwner, "invite an Owner")
		return
	}
	ctx, me, now := r.Context(), user(r), s.now().Unix()
	_, org, err := s.store.TenantOrganisation(ctx, me.TenantID)
	if err != nil {
		internalError(w, r, err)
		return
	}
	inv, bought, err := s.invite(ctx, me.TenantID, store.Invitation{Email: in.Email, Role: in.Role, InvitedBy: me.ID}, now)
	if bought > 0 && err != nil {
		if qerr := s.store.QueueSeatCheck(context.WithoutCancel(ctx), me.TenantID, now); qerr != nil {
			slog.ErrorContext(ctx, "could not queue giving back a seat bought for an invitation not kept", "error", qerr.Error())
		}
	}
	var full *store.SeatLimitError
	var refused seatRefused
	switch {
	case errors.As(err, &refused):
		slog.ErrorContext(ctx, "the billing service did not add a seat for an invitation; nobody was invited", "seats", refused.seats, "error", refused.err.Error())
		writeError(w, http.StatusBadGateway, "billing_error", "The billing service could not add a seat for this invitation, so nobody was invited. Try again in a few minutes.", "")
		return
	case errors.As(err, &full):
		writeError(w, http.StatusConflict, "seat_limit", full.Error(), "")
		return
	case errors.Is(err, store.ErrAlreadyMember):
		writeError(w, http.StatusConflict, "already_member", in.Email+" is in your team already.", "email")
		return
	case errors.Is(err, store.ErrHasAccount):
		writeError(w, http.StatusConflict, "has_account", in.Email+" already has a MailRules account. They must delete their own MailRules account (Settings, Delete my account) before you can invite them.", "email")
		return
	case errors.Is(err, store.ErrAlreadyInvited):
		writeError(w, http.StatusConflict, "already_invited", in.Email+" has an invitation waiting. Revoke it to send a new one.", "email")
		return
	case err != nil:
		internalError(w, r, err)
		return
	}
	inv.InvitedByEmail = me.Email
	sent := ext.Invitation{ID: inv.ID, TenantID: me.TenantID, Organisation: org, Email: inv.Email, Role: inv.Role,
		InvitedBy: me.Email, ExpiresAt: time.Unix(inv.ExpiresAt, 0)}
	rec, err := s.team().Invite(ctx, sent)
	if err != nil {
		slog.ErrorContext(ctx, "the sign-in service did not send an invitation; it was withdrawn", "invitation_id", inv.ID, "error", err.Error())
		if _, werr := s.store.RevokeInvitation(context.WithoutCancel(ctx), me.TenantID, inv.ID, "it could not be sent", now); werr != nil {
			internalError(w, r, werr)
			return
		}
		writeError(w, http.StatusBadGateway, "invite_failed", "The invitation could not be sent, so nobody was invited. Try again in a few minutes.", "")
		return
	}
	if rec != "" {
		if err := s.store.SetInvitationRecord(ctx, me.TenantID, inv.ID, rec); err != nil {
			internalError(w, r, err)
			return
		}
		inv.Record = rec
	}
	writeJSON(w, http.StatusCreated, map[string]any{"invitation": toInvitationJSON(inv)})
}

// seatRefused is the billing module refusing the seats an invitation needed.
type seatRefused struct {
	seats int
	err   error
}

func (e seatRefused) Error() string { return "add seats: " + e.err.Error() }

// invite stores the invitation (store.CreateInvitation). Past the paid seats of a plan sold
// by the seat it asks the billing module for the seats the team then needs and stores it
// once the module agrees, up to three times should others take those seats meanwhile;
// bought is the seats last asked for, 0 for none. A refusal is a seatRefused, storing
// nothing.
func (s *server) invite(ctx context.Context, tenantID int64, inv store.Invitation, now int64) (stored store.Invitation, bought int, err error) {
	for raised := 0; ; raised++ {
		stored, err = s.store.CreateInvitation(ctx, tenantID, inv, s.seatsAtLeast(bought), now)
		var full *store.SeatLimitError
		if !errors.As(err, &full) || raised == 3 {
			return stored, bought, err
		}
		p, perr := s.store.TenantPlan(ctx, tenantID, now)
		if perr != nil {
			return stored, bought, perr
		}
		need := seatsNeeded(s.billing(), p, full)
		if need == 0 {
			return stored, bought, err
		}
		// Not cut short by the browser leaving: a seat bought must be known to be bought.
		if cerr := s.billing().ChangePlan(context.WithoutCancel(ctx), ext.PlanChange{TenantID: tenantID, Record: p.Record, Plan: p.Plan.Plan,
			Interval: p.Interval, Seats: need, CompanySignIn: p.CompanySignInAddOn}); cerr != nil {
			return stored, bought, seatRefused{seats: need, err: cerr}
		}
		bought = need
	}
}

// handleInvitationDelete revokes an invitation: it is deleted, freeing its seat, then the
// module takes it back at the sign-in service.
func (s *server) handleInvitationDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id", "invitation")
	if !ok {
		return
	}
	ctx, tenantID := r.Context(), viewer(r).TenantID
	inv, err := s.store.RevokeInvitation(ctx, tenantID, id, "", s.now().Unix())
	if err != nil {
		fail(w, r, err, "invitation")
		return
	}
	_, org, err := s.store.TenantOrganisation(ctx, tenantID)
	if err == nil {
		err = s.team().Revoke(ctx, ext.Invitation{ID: inv.ID, TenantID: tenantID, Organisation: org, Email: inv.Email, Role: inv.Role,
			InvitedBy: inv.InvitedByEmail, ExpiresAt: time.Unix(inv.ExpiresAt, 0), Record: inv.Record})
	}
	if err != nil {
		slog.WarnContext(ctx, "the sign-in service did not take back a revoked invitation", "invitation_id", inv.ID, "error", err.Error())
	}
	w.WriteHeader(http.StatusNoContent)
}

// auditJSON is one row of the audit log as the API writes it.
type auditJSON struct {
	ID     int64  `json:"id"`
	At     int64  `json:"at"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Target string `json:"target"`
	Detail string `json:"detail"`
}

// handleAudit is the tenant's audit log from since to until (unix seconds; the last
// AuditDays and now when left out), as a CSV file (format=csv, the default) or JSON.
func (s *server) handleAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	now := s.now()
	since, until := now.Unix()-store.AuditDays*24*3600, now.Unix()
	for _, p := range []struct {
		name string
		dst  *int64
	}{{"since", &since}, {"until", &until}} {
		if v := q.Get(p.name); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				invalid(w, p.name, "Give a time in unix seconds.")
				return
			}
			*p.dst = n
		}
	}
	format := q.Get("format")
	if format != "" && format != "csv" && format != "json" {
		invalid(w, "format", "Choose csv or json.")
		return
	}
	entries, err := s.store.AuditLog(r.Context(), viewer(r).TenantID, since, until)
	if err != nil {
		internalError(w, r, err)
		return
	}
	name := "mailrules-audit-" + now.UTC().Format("2006-01-02")
	if format == "json" {
		out := make([]auditJSON, len(entries))
		for i, e := range entries {
			out[i] = auditJSON{ID: e.ID, At: e.At, Actor: e.Actor, Action: e.Action, Target: e.Target, Detail: e.Detail}
		}
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.json"`)
		writeJSON(w, http.StatusOK, map[string]any{"entries": out})
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"time", "actor", "action", "target", "detail"})
	for _, e := range entries {
		_ = cw.Write([]string{time.Unix(e.At, 0).UTC().Format(time.RFC3339), cell(e.Actor), e.Action, cell(e.Target), cell(e.Detail)})
	}
	cw.Flush()
}

// cell keeps a CSV cell from being read as a formula by a spreadsheet: one that starts with
// =, +, - or @ gets a leading apostrophe.
func cell(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

// AcceptInvitation records that the person invited at email accepted (ext.Host).
func (h host) AcceptInvitation(ctx context.Context, tenantID int64, email string) error {
	if _, ok := store.ActorOf(ctx); !ok {
		ctx = store.WithActor(ctx, store.Actor{Name: "Sign-in service"})
	}
	err := h.s.store.AcceptInvitation(ctx, tenantID, strings.TrimSpace(email), h.s.now().Unix())
	if errors.Is(err, store.ErrNotFound) {
		return ext.ErrNoInvitation
	}
	return err
}
