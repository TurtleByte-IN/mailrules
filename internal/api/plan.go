package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/worker"
)

// billingOf is the build's ext.Module.Billing, or nil when the build sells no plans.
func billingOf(modules []ext.Module) *ext.Billing {
	for _, m := range modules {
		if m.Billing != nil {
			return m.Billing
		}
	}
	return nil
}

// soldPlans are the plans on sale, cheapest first.
var soldPlans = []string{store.PlanPersonal, store.PlanPro}

// termsOf are the terms a tenant in st is held to: its plan's, and the trial's in the trial
// and once the plan has ended (nothing is sorted then; they only fill the meters).
func termsOf(b *ext.Billing, st store.PlanState) ext.PlanTerms {
	if t, ok := b.Plans[st.Plan.Plan]; ok && !st.Ended() {
		return t
	}
	return b.Plans[store.PlanTrial]
}

// tenantLimits are what the tenant may use at now: its plan's limits in a build that sells
// plans, else none.
func tenantLimits(ctx context.Context, st *store.Store, b *ext.Billing, tenantID int64, now time.Time) (ext.Limits, error) {
	if b == nil {
		return noLimits, nil
	}
	p, err := st.TenantPlan(ctx, tenantID, now.Unix())
	if err != nil {
		return ext.Limits{}, fmt.Errorf("the tenant's plan: %w", err)
	}
	return termsOf(b, p).Limits, nil
}

// gatedFeatures are the features some plan includes: each is a tenant's only while its plan
// lists it.
func gatedFeatures(b *ext.Billing) []string {
	var out []string
	if b == nil {
		return nil
	}
	for _, t := range b.Plans {
		for _, f := range t.Features {
			if !slices.Contains(out, f) {
				out = append(out, f)
			}
		}
	}
	return out
}

// hasFeature reports whether the tenant may use feature: always, unless a plan of the build
// gates it and the tenant's plan, running, does not list it.
func hasFeature(ctx context.Context, st *store.Store, b *ext.Billing, tenantID int64, feature string, now time.Time) (bool, error) {
	if !slices.Contains(gatedFeatures(b), feature) {
		return true, nil
	}
	p, err := st.TenantPlan(ctx, tenantID, now.Unix())
	if err != nil {
		return false, fmt.Errorf("the tenant's plan: %w", err)
	}
	return !p.Ended() && slices.Contains(b.Plans[p.Plan.Plan].Features, feature), nil
}

// PlanFeature is, for actions.Exec.Notices, whether a tenant may use feature: every tenant's
// in a build that sells no plans. A failure to read the plan is logged and refuses.
func PlanFeature(st *store.Store, modules []ext.Module, feature string) func(ctx context.Context, tenantID int64) bool {
	b := billingOf(modules)
	return func(ctx context.Context, tenantID int64) bool {
		ok, err := hasFeature(ctx, st, b, tenantID, feature, time.Now())
		if err != nil {
			slog.ErrorContext(ctx, "could not read the tenant's plan", "tenant", tenantID, "feature", feature, "error", err.Error())
		}
		return ok
	}
}

// featureNames are how plan_required names a feature.
var featureNames = map[string]string{"suggest": "Suggest from my mail", "notifications": "Notifications and webhooks", "draft_replies": "Draft replies"}

// planRequired answers 403 plan_required for a feature the tenant's plan does not include.
func (s *server) planRequired(w http.ResponseWriter, feature string) {
	name, ok := featureNames[feature]
	if !ok {
		name = "This feature"
	}
	var on []string
	for _, id := range soldPlans {
		if slices.Contains(s.billing().Plans[id].Features, feature) {
			on = append(on, s.billing().Plans[id].Name)
		}
	}
	msg := name + " is not part of your plan."
	if len(on) > 0 {
		msg = name + " is part of " + strings.Join(on, " and ") + ". Choose it on Plan and billing to use it."
	}
	writeError(w, http.StatusForbidden, "plan_required", msg, "")
}

// planFeatureOnly refuses a request with 403 plan_required while the tenant's plan does not
// include feature.
func (s *server) planFeatureOnly(feature string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ok, err := hasFeature(r.Context(), s.store, s.billing(), viewer(r).TenantID, feature, s.now())
		switch {
		case err != nil:
			internalError(w, r, err)
		case !ok:
			s.planRequired(w, feature)
		default:
			h(w, r)
		}
	}
}

// tenantFeatures are the features GET /api/settings reports to the tenant: the build's, but
// a feature a plan gates only while the tenant's plan includes it.
func (s *server) tenantFeatures(ctx context.Context, tenantID int64) (map[string]bool, error) {
	out := s.features()
	for _, f := range gatedFeatures(s.billing()) {
		if !out[f] {
			continue
		}
		ok, err := hasFeature(ctx, s.store, s.billing(), tenantID, f, s.now())
		if err != nil {
			return nil, err
		}
		out[f] = ok
	}
	return out, nil
}

// billing is the build's Billing, or nil.
func (s *server) billing() *ext.Billing { return billingOf(s.Modules) }

// readOnly refuses a change, anything but GET and HEAD, with 403 plan_ended while the
// signed-in user's tenant's plan has ended. Buying a plan is not a change it refuses.
func (s *server) readOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || s.billing() == nil {
			next.ServeHTTP(w, r)
			return
		}
		p, err := s.store.TenantPlan(r.Context(), viewer(r).TenantID, s.now().Unix())
		switch {
		case err != nil:
			internalError(w, r, err)
		case p.Ended():
			writeError(w, http.StatusForbidden, "plan_ended", ext.ErrPlanEnded.Error(), "")
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// readOnlyExempt are the signed-in routes a tenant whose plan has ended may still change
// through: buying a plan.
func readOnlyExempt(path string) bool { return strings.HasPrefix(path, "/api/plan") }

// unix is t as unix seconds, or nil for zero.
func unix(t int64) *int64 {
	if t == 0 {
		return nil
	}
	return &t
}

func orNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

type planTermsJSON struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Prices    map[string]string `json:"prices"`
	Includes  []string          `json:"includes"`
	Mailboxes *int              `json:"mailboxes"`
	AIEmails  *int              `json:"ai_emails"`
	Features  []string          `json:"features"`
}

type planJSON struct {
	Plan             string          `json:"plan"`
	Interval         *string         `json:"interval"`
	Status           *string         `json:"status"`
	PeriodEnd        *int64          `json:"period_end"`
	TrialEndsAt      int64           `json:"trial_ends_at"`
	TrialDaysLeft    *int            `json:"trial_days_left"`
	EndedAt          *int64          `json:"ended_at"`
	SecretsDeletedAt *int64          `json:"secrets_deleted_at"`
	RemovedAt        *int64          `json:"removed_at"`
	ReadOnly         bool            `json:"read_only"`
	Subscribed       bool            `json:"subscribed"`
	Note             *string         `json:"note"`
	Plans            []planTermsJSON `json:"plans"`
}

// limitOrNil is a limit as the API writes it: null for no limit.
func limitOrNil(n int) *int {
	if n < 0 {
		return nil
	}
	return &n
}

func (s *server) planJSON(p store.PlanState) planJSON {
	b := s.billing()
	out := planJSON{Plan: p.Plan.Plan, TrialEndsAt: p.TrialEnd, EndedAt: unix(p.EndedAt), ReadOnly: p.Ended(),
		Subscribed: p.Record != "" && !p.Ended(), Note: orNil(b.Note), Plans: []planTermsJSON{}}
	if !p.Ended() && p.Plan.Plan != store.PlanTrial {
		out.Interval, out.Status, out.PeriodEnd = orNil(p.Interval), orNil(p.Status), unix(p.PeriodEnd)
	}
	if p.Plan.Plan == store.PlanTrial {
		left := int(math.Ceil(float64(p.TrialEnd-s.now().Unix()) / (24 * 3600)))
		out.TrialDaysLeft = &left
	}
	if p.Ended() {
		out.SecretsDeletedAt = unix(p.EndedAt + store.EndedSecretsDays*24*3600)
		out.RemovedAt = unix(p.EndedAt + store.EndedRemovalDays*24*3600)
	}
	for _, id := range soldPlans {
		t := b.Plans[id]
		out.Plans = append(out.Plans, planTermsJSON{ID: id, Name: t.Name, Prices: t.Prices, Includes: append([]string{}, t.Includes...),
			Mailboxes: limitOrNil(t.Limits.Mailboxes), AIEmails: limitOrNil(t.Limits.AIEmails), Features: append([]string{}, t.Features...)})
		if out.Plans[len(out.Plans)-1].Prices == nil {
			out.Plans[len(out.Plans)-1].Prices = map[string]string{}
		}
	}
	return out
}

// billingOnly refuses a request with 404 not_available in a build that sells no plans.
func (s *server) billingOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.billing() == nil {
			writeError(w, http.StatusNotFound, "not_available", "Plan and billing is not part of this build of MailRules.", "")
			return
		}
		h(w, r)
	}
}

// currentPlan reads the signed-in user's tenant's plan, answering for a failure.
func (s *server) currentPlan(w http.ResponseWriter, r *http.Request) (store.PlanState, bool) {
	p, err := s.store.TenantPlan(r.Context(), viewer(r).TenantID, s.now().Unix())
	if err != nil {
		internalError(w, r, err)
		return store.PlanState{}, false
	}
	return p, true
}

func (s *server) handlePlan(w http.ResponseWriter, r *http.Request) {
	if p, ok := s.currentPlan(w, r); ok {
		writeJSON(w, http.StatusOK, s.planJSON(p))
	}
}

// readChoice reads a plan and interval on sale from the request.
func readChoice(w http.ResponseWriter, r *http.Request) (plan, interval string, ok bool) {
	var in struct {
		Plan     string `json:"plan"`
		Interval string `json:"interval"`
	}
	if !readJSON(w, r, &in) {
		return "", "", false
	}
	switch {
	case !slices.Contains(soldPlans, in.Plan):
		invalid(w, "plan", "Choose Personal or Pro.")
		return "", "", false
	case in.Interval != store.IntervalMonth && in.Interval != store.IntervalYear:
		invalid(w, "interval", "Choose monthly or yearly.")
		return "", "", false
	}
	return in.Plan, in.Interval, true
}

// downgradeRefused answers 409 downgrade_refused and returns true when the tenant has more
// mailboxes than plan allows, naming them so the person knows which to remove.
func (s *server) downgradeRefused(w http.ResponseWriter, r *http.Request, plan string) bool {
	t := s.billing().Plans[plan]
	if t.Limits.Mailboxes < 0 {
		return false
	}
	have, err := s.store.TenantAccounts(r.Context(), viewer(r).TenantID)
	if err != nil {
		internalError(w, r, err)
		return true
	}
	over := len(have) - t.Limits.Mailboxes
	if over <= 0 {
		return false
	}
	names := make([]string, len(have))
	for i, a := range have {
		names[i] = a.Label
	}
	mailboxes := "mailboxes"
	if t.Limits.Mailboxes == 1 {
		mailboxes = "mailbox"
	}
	what := "one of them"
	if over > 1 {
		what = fmt.Sprintf("%d of them", over)
	}
	writeError(w, http.StatusConflict, "downgrade_refused", fmt.Sprintf("%s allows %d %s, and %d are connected: %s. Remove %s on Mailboxes first.",
		t.Name, t.Limits.Mailboxes, mailboxes, len(have), strings.Join(names, ", "), what), "")
	return true
}

// billingFailed answers 502 billing_error for a failure of the module or its service.
func billingFailed(w http.ResponseWriter, r *http.Request, what string, err error) {
	slog.ErrorContext(r.Context(), "billing failed", "what", what, "error", err.Error())
	writeError(w, http.StatusBadGateway, "billing_error", "The billing service could not be reached. Try again in a minute.", "")
}

func (s *server) handlePlanCheckout(w http.ResponseWriter, r *http.Request) {
	plan, interval, ok := readChoice(w, r)
	if !ok {
		return
	}
	p, ok := s.currentPlan(w, r)
	if !ok {
		return
	}
	if p.Record != "" && !p.Ended() {
		writeError(w, http.StatusConflict, "subscribed", "You have a plan already. Switch plans instead.", "")
		return
	}
	if s.downgradeRefused(w, r, plan) {
		return
	}
	who := user(r)
	checkout, err := s.billing().Checkout(r.Context(), ext.CheckoutRequest{TenantID: who.TenantID, Email: who.Email, Plan: plan, Interval: interval})
	if err != nil {
		billingFailed(w, r, "checkout", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"checkout": json.RawMessage(checkout), "script": s.billing().Script})
}

func (s *server) handlePlanChange(w http.ResponseWriter, r *http.Request) {
	plan, interval, ok := readChoice(w, r)
	if !ok {
		return
	}
	p, ok := s.currentPlan(w, r)
	if !ok {
		return
	}
	switch {
	case p.Record == "" || p.Ended():
		writeError(w, http.StatusConflict, "not_subscribed", "Choose a plan first.", "")
		return
	case p.Plan.Plan == plan && p.Interval == interval:
		writeError(w, http.StatusConflict, "same_plan", "This is your plan already.", "")
		return
	}
	if s.downgradeRefused(w, r, plan) {
		return
	}
	if err := s.billing().ChangePlan(r.Context(), viewer(r).TenantID, p.Record, plan, interval); err != nil {
		billingFailed(w, r, "change plan", err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.planJSON(p))
}

func (s *server) handlePlanPortal(w http.ResponseWriter, r *http.Request) {
	p, ok := s.currentPlan(w, r)
	if !ok {
		return
	}
	if p.Record == "" {
		writeError(w, http.StatusConflict, "not_subscribed", "Choose a plan first.", "")
		return
	}
	url, err := s.billing().Portal(r.Context(), viewer(r).TenantID, p.Record)
	if err != nil {
		billingFailed(w, r, "portal", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": url})
}

// applyPlan acts on a change of the tenant's plan: a plan that ended stops its mailboxes, one
// bought again starts those that are not paused and still have their secret, and the open
// screens are told.
func (s *server) applyPlan(ctx context.Context, tenantID int64, before, after store.PlanState) {
	if before.Ended() != after.Ended() {
		accounts, err := s.store.TenantAccounts(ctx, tenantID)
		if err != nil {
			slog.ErrorContext(ctx, "could not list the mailboxes of a tenant whose plan changed", "tenant", tenantID, "error", err.Error())
		}
		for _, a := range accounts {
			switch {
			case after.Ended():
				s.StopAccount(a.ID)
			case a.Status != worker.StatusPaused && !a.SecretGone:
				s.StartAccount(a)
			}
		}
	}
	s.Hub.Publish(tenantID, 0, events.PlanChanged, nil)
}

// SetPlan records the tenant's plan as the billing module reports it and acts on it (ext.Host).
func (h host) SetPlan(ctx context.Context, tenantID int64, p ext.Plan) error {
	var end int64
	if !p.PeriodEnd.IsZero() {
		end = p.PeriodEnd.Unix()
	}
	before, after, err := h.s.store.SetPlan(ctx, tenantID, store.Plan{Plan: p.Plan, Interval: p.Interval, Status: p.Status,
		PeriodEnd: end, Record: p.Record}, h.s.now().Unix())
	if errors.Is(err, store.ErrNotFound) {
		return ext.ErrNoTenant
	}
	if err != nil {
		return err
	}
	h.s.applyPlan(ctx, tenantID, before, after)
	return nil
}

// Plan returns the tenant's plan as the daemon has it now (ext.Host).
func (h host) Plan(ctx context.Context, tenantID int64) (ext.PlanState, error) {
	p, err := h.s.store.TenantPlan(ctx, tenantID, h.s.now().Unix())
	if errors.Is(err, store.ErrNotFound) {
		return ext.PlanState{}, ext.ErrNoTenant
	}
	if err != nil {
		return ext.PlanState{}, err
	}
	t := func(sec int64) time.Time {
		if sec == 0 {
			return time.Time{}
		}
		return time.Unix(sec, 0)
	}
	return ext.PlanState{Plan: ext.Plan{Plan: p.Plan.Plan, Interval: p.Interval, Status: p.Status, PeriodEnd: t(p.PeriodEnd), Record: p.Record},
		TrialEnd: t(p.TrialEnd), EndedAt: t(p.EndedAt)}, nil
}

// Who is the signed-in person the request is for (ext.Host).
func (h host) Who(r *http.Request) ext.Who {
	u := user(r)
	return ext.Who{UserID: u.ID, TenantID: u.TenantID, Email: u.Email}
}

// origin is a source a checkout script may load from: an https origin, optionally with a
// wildcard for its subdomains.
var origin = regexp.MustCompile(`^https://(\*\.)?[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+(:[0-9]{1,5})?$`)

// contentPolicy is the Content-Security-Policy of the build: its own origin only, and, for a
// build that sells plans, the origins its checkout script needs.
func contentPolicy(b *ext.Billing) string {
	if b == nil {
		return "default-src 'self'"
	}
	src := b.Sources
	policy := []string{"default-src 'self'"}
	for _, d := range []struct {
		name    string
		origins []string
	}{{"script-src", src.Scripts}, {"frame-src", src.Frames}, {"connect-src", src.Connect}, {"style-src", src.Styles}, {"img-src", src.Images}} {
		if len(d.origins) > 0 {
			policy = append(policy, d.name+" 'self' "+strings.Join(d.origins, " "))
		}
	}
	return strings.Join(policy, "; ")
}

// checkBilling refuses a Billing the daemon cannot serve: a plan the build sells missing or
// unnamed, a feature that is not one of the build's, no Checkout, ChangePlan or Portal, a
// Script that is not one of the module's Public GET routes, or a source that is not an https
// origin.
func checkBilling(m ext.Module, publicGet map[string]bool, built map[string]bool) error {
	b := m.Billing
	if b.Checkout == nil || b.ChangePlan == nil || b.Portal == nil {
		return fmt.Errorf("module %s: Billing needs Checkout, ChangePlan and Portal", m.Name)
	}
	for _, id := range []string{store.PlanTrial, store.PlanPersonal, store.PlanPro} {
		t, ok := b.Plans[id]
		if !ok || t.Name == "" {
			return fmt.Errorf("module %s: Billing.Plans needs the %s plan, with a name", m.Name, id)
		}
		for _, f := range t.Features {
			if !built[f] {
				return fmt.Errorf("module %s: plan %s lists feature %q, which is not one of the build's", m.Name, id, f)
			}
		}
	}
	for id := range b.Plans {
		if !slices.Contains([]string{store.PlanTrial, store.PlanPersonal, store.PlanPro}, id) {
			return fmt.Errorf("module %s: Billing.Plans has %q, which is not a plan the daemon knows", m.Name, id)
		}
	}
	if !publicGet[b.Script] {
		return fmt.Errorf("module %s: Billing.Script %q is not one of its Public GET routes", m.Name, b.Script)
	}
	for _, list := range [][]string{b.Sources.Scripts, b.Sources.Frames, b.Sources.Connect, b.Sources.Styles, b.Sources.Images} {
		for _, o := range list {
			if !origin.MatchString(o) {
				return fmt.Errorf("module %s: Billing.Sources %q is not an https origin", m.Name, o)
			}
		}
	}
	return nil
}

// planEnded is ext.ErrPlanEnded while the tenant's plan has ended, in a build that sells
// plans: the read-only rule for what a module does on a person's behalf.
func (s *server) planEnded(ctx context.Context, tenantID int64) error {
	if s.billing() == nil {
		return nil
	}
	p, err := s.store.TenantPlan(ctx, tenantID, s.now().Unix())
	if err != nil {
		return fmt.Errorf("the tenant's plan: %w", err)
	}
	if p.Ended() {
		return ext.ErrPlanEnded
	}
	return nil
}
