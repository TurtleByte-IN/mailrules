package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// Company sign-in (MAI-246): a team's people sign in through their company's identity
// provider (SAML or OIDC), which the team's IT admin sets up at the sign-in service through
// its admin portal. The sign-in module offers it (ext.Team.AdminPortal); in a build that
// sells plans only a Team tenant may use it, from the seats its terms include it from or
// with the add-on an Owner buys on Plan and billing (ext.Billing.CompanySignIn).
//
// When a tenant may use it no more (fewer seats and no add-on, another plan, the plan
// ended), it loses the setup link and the Company sign-in section, and that is all: the
// daemon changes nothing at the sign-in service, where a connection made stays, and the
// sign-in module decides what becomes of it (ext.PlanState.CompanySignIn), by the date the
// daemon saves at the loss (checkCompanySignIn, ext.PlanState.CompanySignInEnds). Plan and billing
// warns the Owner before a change of theirs that ends it.

// toSetUpSignIn is what a refusal says the Company sign-in section is for.
const toSetUpSignIn = "set up company sign-in"

// companySignInFeature is how GET /api/settings names company sign-in in `features`.
const companySignInFeature = "company_sign_in"

// portalTeam is the build's Module.Team when it offers company sign-in, else nil.
func (s *server) portalTeam() *ext.Team {
	if t := s.team(); t != nil && t.AdminPortal != nil {
		return t
	}
	return nil
}

// companySignInAllowed reports whether a tenant in p may use company sign-in in a build that
// offers it: every tenant in a build that sells no plans; else one whose plan runs and sells
// it (the Team plan), with the seats it is included from or the add-on.
func companySignInAllowed(b *ext.Billing, p store.PlanState) bool {
	if b == nil {
		return true
	}
	if p.Ended() {
		return false
	}
	c := b.Plans[p.Plan.Plan].CompanySignIn
	return c != nil && (p.CompanySignInAddOn || c.IncludedSeats > 0 && p.Seats >= c.IncludedSeats)
}

// checkCompanySignIn records, in a build that offers company sign-in and sells plans,
// whether the tenant in p may use it now, and returns p with when it stops for a tenant
// that lost it (store.CheckCompanySignIn). before is the plan as it stood before a report
// that changed it into p, or p itself: a loss stops sign-in at the end of the paid period in
// force before it, or the day the plan ended. A loss no report brought (a release that
// changed the terms) gets its date the first time this sees it.
func (s *server) checkCompanySignIn(ctx context.Context, tenantID int64, before, p store.PlanState) (store.PlanState, error) {
	if s.portalTeam() == nil || s.billing() == nil {
		return p, nil
	}
	lossEnd := s.now().Unix()
	switch {
	case p.Ended():
		lossEnd = p.EndedAt
	case !before.Ended() && before.PeriodEnd != 0:
		lossEnd = before.PeriodEnd
	}
	end, err := s.store.CheckCompanySignIn(ctx, tenantID, companySignInAllowed(s.billing(), p), lossEnd)
	if err != nil {
		return p, err
	}
	p.CompanySignInEndsAt = end
	return p, nil
}

// companySignInEnds is when company sign-in stops for the tenant in p that lost it, unix
// seconds; 0 while it may use it or never could. A plan that ended before the date saved at
// the loss stops it on the day it ended.
func companySignInEnds(p store.PlanState) int64 {
	if e := p.CompanySignInEndsAt; e != 0 && p.Ended() && p.EndedAt < e {
		return p.EndedAt
	}
	return p.CompanySignInEndsAt
}

// companySignInSold are the terms the build sells company sign-in on: the Team plan's, in a
// build that offers it (portal) and sells plans; nil otherwise.
func companySignInSold(b *ext.Billing, portal bool) *ext.CompanySignInTerms {
	if b == nil || !portal {
		return nil
	}
	return b.Plans[store.PlanTeam].CompanySignIn
}

// companySignIn reports whether the tenant may use company sign-in now: false in a build
// that does not offer it.
func (s *server) companySignIn(ctx context.Context, tenantID int64) (bool, error) {
	if s.portalTeam() == nil {
		return false, nil
	}
	if s.billing() == nil {
		return true, nil
	}
	p, err := s.store.TenantPlan(ctx, tenantID, s.now().Unix())
	if err != nil {
		return false, fmt.Errorf("the tenant's plan: %w", err)
	}
	return companySignInAllowed(s.billing(), p), nil
}

// companySignInOnly refuses a request with 404 not_available in a build that does not offer
// company sign-in, then with 403 plan_required while the tenant may not use it.
func (s *server) companySignInOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.portalTeam() == nil {
			writeError(w, http.StatusNotFound, "not_available", "Company sign-in is not part of this build of MailRules.", "")
			return
		}
		ok, err := s.companySignIn(r.Context(), viewer(r).TenantID)
		switch {
		case err != nil:
			internalError(w, r, err)
		case !ok:
			writeError(w, http.StatusForbidden, "plan_required", s.companySignInOffer(), "")
		default:
			h(w, r)
		}
	}
}

// companySignInOffer is what a tenant that may not use company sign-in is told: how to get it.
func (s *server) companySignInOffer() string {
	c := companySignInSold(s.billing(), true)
	switch {
	case c == nil:
		return "Company sign-in is not part of your plan."
	case c.IncludedSeats > 0:
		return fmt.Sprintf("Company sign-in comes with the Team plan from %d seats, or with its company sign-in add-on below that. An Owner adds it on Plan and billing.", c.IncludedSeats)
	default:
		return "Company sign-in comes with the Team plan's company sign-in add-on. An Owner adds it on Plan and billing."
	}
}

// signInFailed answers 502 sign_in_error for a failure of the sign-in module or its service.
func signInFailed(w http.ResponseWriter, r *http.Request, what string, err error) {
	slog.ErrorContext(r.Context(), "the sign-in service failed", "what", what, "error", err.Error())
	writeError(w, http.StatusBadGateway, "sign_in_error", "The sign-in service could not be reached. Try again in a minute.", "")
}

// connectionStates are the states of company sign-in a module may report (ext.Team.Connection).
var connectionStates = []string{ext.ConnectionNone, ext.ConnectionDomainPending, ext.ConnectionActive}

// handleCompanySignIn reports the state of the tenant's company sign-in at the sign-in
// service, as the module tells it.
func (s *server) handleCompanySignIn(w http.ResponseWriter, r *http.Request) {
	ctx, tenantID := r.Context(), viewer(r).TenantID
	_, org, err := s.store.TenantOrganisation(ctx, tenantID)
	if err != nil {
		internalError(w, r, err)
		return
	}
	state, err := s.portalTeam().Connection(ctx, tenantID, org)
	if err == nil && !slices.Contains(connectionStates, state) {
		err = fmt.Errorf("unknown connection state %q", state)
	}
	if err != nil {
		signInFailed(w, r, "company sign-in state", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": state})
}

// portalIntents are what an admin portal link may be asked for, as the audit log says it.
var portalIntents = map[string]string{ext.PortalSSO: "set up the connection", ext.PortalDomain: "verify the domain"}

// handleCompanySignInPortal asks the sign-in module for a one-time link to its admin portal,
// where the team's IT admin sets company sign-in up, and records who asked. The link is
// handed to the browser and kept nowhere, the log and the audit log included.
func (s *server) handleCompanySignInPortal(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Intent string `json:"intent"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	what, ok := portalIntents[in.Intent]
	if !ok {
		invalid(w, "intent", "Choose sso or domain_verification.")
		return
	}
	ctx, tenantID := r.Context(), viewer(r).TenantID
	_, org, err := s.store.TenantOrganisation(ctx, tenantID)
	if err != nil {
		internalError(w, r, err)
		return
	}
	url, err := s.portalTeam().AdminPortal(ctx, ext.PortalRequest{TenantID: tenantID, Organisation: org, Intent: in.Intent})
	if err == nil && url == "" {
		err = fmt.Errorf("no link")
	}
	if err != nil {
		signInFailed(w, r, "admin portal link", err)
		return
	}
	if err := s.store.Audit(ctx, tenantID, store.AuditCompanySignInPortal, "Company sign-in", "opened a setup link to "+what); err != nil {
		internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": url})
}

// handlePlanCompanySignIn has the billing module add the company sign-in add-on to the
// tenant's Team subscription or remove it (ext.Billing.CompanySignIn), and records that the
// Owner asked. The add-on changes when the module reports it (Host.SetPlan).
func (s *server) handlePlanCompanySignIn(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AddOn *bool `json:"add_on"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.AddOn == nil {
		invalid(w, "add_on", "Say whether to add the add-on (true) or remove it (false).")
		return
	}
	c := companySignInSold(s.billing(), s.portalTeam() != nil)
	if c == nil {
		writeError(w, http.StatusNotFound, "not_available", "Company sign-in is not sold in this build of MailRules.", "")
		return
	}
	p, ok := s.currentPlan(w, r)
	if !ok {
		return
	}
	add := *in.AddOn
	switch {
	case p.Record == "" || p.Ended():
		writeError(w, http.StatusConflict, "not_subscribed", "Choose the Team plan first.", "")
		return
	case p.Plan.Plan != store.PlanTeam:
		writeError(w, http.StatusConflict, "team_only", "The company sign-in add-on is part of the Team plan. Switch to Team first.", "")
		return
	case add == p.CompanySignInAddOn:
		msg := "You have the company sign-in add-on already."
		if !add {
			msg = "You do not have the company sign-in add-on."
		}
		writeError(w, http.StatusConflict, "same_plan", msg, "")
		return
	case add && c.IncludedSeats > 0 && p.Seats >= c.IncludedSeats:
		writeError(w, http.StatusConflict, "included", fmt.Sprintf("Company sign-in comes with your %d seats already, so the add-on is not needed.", p.Seats), "")
		return
	}
	tenantID := viewer(r).TenantID
	// Not cut short by the browser leaving: once asked, the request is recorded.
	ctx := context.WithoutCancel(r.Context())
	if err := s.billing().CompanySignIn(ctx, ext.CompanySignInChange{TenantID: tenantID, Record: p.Record, Add: add, Interval: p.Interval}); err != nil {
		billingFailed(w, r, "company sign-in add-on", err)
		return
	}
	detail := "asked to remove it"
	if add {
		detail = "asked to add it, billed " + map[string]string{store.IntervalMonth: "monthly", store.IntervalYear: "yearly"}[p.Interval]
	}
	if err := s.store.Audit(ctx, tenantID, store.AuditCompanySignInAddOn, "Company sign-in add-on", detail); err != nil {
		internalError(w, r, err)
		return
	}
	s.writePlan(w, r, http.StatusAccepted, p, true)
}

// companySignInJSON is where the tenant stands with company sign-in, in GET /api/plan: null
// in a build that does not sell it.
type companySignInJSON struct {
	Allowed       bool `json:"allowed"`        // the tenant may use it now (features.company_sign_in)
	AddOn         bool `json:"add_on"`         // the add-on is on its subscription
	IncludedSeats *int `json:"included_seats"` // the paid Team seats it comes with; null: only with the add-on
	// EndsAt is when company sign-in stops for a team that lost it (companySignInEnds); null
	// while allowed or never allowed.
	EndsAt *int64 `json:"ends_at"`
}

// companySignInTermsJSON is how the Team plan sells company sign-in, for an Owner.
type companySignInTermsJSON struct {
	IncludedSeats *int              `json:"included_seats"`
	Prices        map[string]string `json:"prices"`
}

// includedSeats is c.IncludedSeats as the API writes it: null for never included.
func includedSeats(c *ext.CompanySignInTerms) *int {
	if c.IncludedSeats <= 0 {
		return nil
	}
	n := c.IncludedSeats
	return &n
}

// checkCompanySignIn refuses company sign-in terms the daemon cannot serve: on a plan other
// than Team, with a price missing or a negative seat count, without Billing.CompanySignIn or
// a module that offers company sign-in (offered), and Billing.CompanySignIn with no terms.
func checkCompanySignIn(m ext.Module, offered bool) error {
	b := m.Billing
	for id, t := range b.Plans {
		if slices.Contains(t.Features, companySignInFeature) {
			return fmt.Errorf("module %s: plan %s lists %q in Features; the Team plan sells it through PlanTerms.CompanySignIn", m.Name, id, companySignInFeature)
		}
		c := t.CompanySignIn
		if c == nil {
			continue
		}
		switch {
		case id != store.PlanTeam:
			return fmt.Errorf("module %s: plan %s has CompanySignIn terms; only the team plan sells company sign-in", m.Name, id)
		case c.IncludedSeats < 0 || c.Prices[store.IntervalMonth] == "" || c.Prices[store.IntervalYear] == "":
			return fmt.Errorf("module %s: CompanySignIn terms need a monthly and a yearly price and IncludedSeats of 0 or more", m.Name)
		case b.CompanySignIn == nil:
			return fmt.Errorf("module %s: the team plan sells company sign-in, and Billing has no CompanySignIn", m.Name)
		case !offered:
			return fmt.Errorf("module %s: the team plan sells company sign-in, and no module of the build offers it (Team.AdminPortal)", m.Name)
		}
	}
	if b.CompanySignIn != nil && b.Plans[store.PlanTeam].CompanySignIn == nil {
		return fmt.Errorf("module %s: Billing.CompanySignIn is set, and the team plan has no CompanySignIn terms", m.Name)
	}
	return nil
}
