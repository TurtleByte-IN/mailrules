// Package api serves the HTTP API described in api/openapi.yaml.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/actions"
	"github.com/TurtleByte-IN/mailrules/internal/crypto"
	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/publicnet"
	"github.com/TurtleByte-IN/mailrules/internal/settings"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/summary"
	"github.com/TurtleByte-IN/mailrules/internal/telemetry"
	"github.com/TurtleByte-IN/mailrules/internal/unsubscribe"
	"github.com/TurtleByte-IN/mailrules/internal/web"
	"github.com/TurtleByte-IN/mailrules/internal/worker"
)

// Options configures the HTTP handler.
type Options struct {
	Store         *store.Store
	SecureCookies bool // set when the UI is not served from localhost
	// TrustedProxies are the peers whose X-Forwarded-For header is believed when counting
	// failed sign-ins (MAILRULES_TRUSTED_PROXIES). Empty: the header is never read.
	TrustedProxies []netip.Prefix
	Now            func() time.Time // nil means time.Now

	Hub      *events.Hub        // the SSE stream's source; nil = a private hub nothing publishes to
	Exec     *actions.Exec      // undo and corrections: the only way this package changes a mailbox
	Settings *settings.Settings // the settings the user changes at runtime
	Models   ModelSource        // the models the composer and tester use; nil = Settings
	Metrics  *telemetry.Metrics // nil = a private registry
	Version  string             // shown in GET /api/settings

	// StartCheck starts a cleanup check of an account in the background and returns it,
	// running (worker.Manager.StartCheck). Checks holds the account checks in memory, and
	// SortAll applies finished checks' kept rows, one undoable batch per mailbox, all started
	// or none (worker.Manager.SortAll).
	StartCheck func(c worker.Cleanup, fingerprint string, run worker.CheckFunc) (*worker.Check, error)
	Checks     *worker.CheckStore
	SortAll    func(ctx context.Context, runs []worker.SortRun) ([]store.Batch, error)
	// Connect logs in to a mail server with credentials that are not stored yet, trying
	// the usernames the preset allows, and returns the connection with the username that worked.
	Connect func(ctx context.Context, acct store.Account, password string) (mail.Mailbox, string, error)
	// ConnectOAuth logs in to a one-click mailbox (store.Account.OAuth) through the module
	// that signs in to its preset. secret nil: the account's stored secret, replaced in the
	// store when the module rotates it. Otherwise a secret not stored yet, replaced in place
	// when the module rotates it, so what is stored afterwards is the one that works.
	ConnectOAuth func(ctx context.Context, acct store.Account, secret *string) (mail.Mailbox, error)
	// Resolver looks up the server a person names, which in the hosted build must be on
	// the public internet (publicnet.Check); nil is the system's resolver.
	Resolver publicnet.Resolver
	// StartAccount (re)starts watching an account; StopAccount stops it and waits until
	// mail already queued is finished.
	StartAccount func(acct store.Account)
	StopAccount  func(accountID int64)
	// Modules are the features compiled in from outside this repository. Their routes are
	// served like the built-in ones, and each one's name is on in GET /api/settings
	// features. A route of the contract that only a module provides is refused with 404
	// not_available while no module serves it.
	Modules []ext.Module
	// Summary is the summary email; nil = one built from Settings.Env that cannot send.
	Summary *summary.Service
	// Unsubscriber calls the one-click unsubscribe links Senders offers; nil = one that
	// reaches only the public internet (unsubscribe.New with Resolver).
	Unsubscriber unsubscribe.Unsubscriber
}

type server struct {
	Options
	store  *store.Store
	secure bool
	now    func() time.Time
	logins *failureLimiter // failed sign-ins per client address
	// accountLogins counts failed sign-ins per email, so rotating addresses cannot
	// brute-force the one admin.
	accountLogins *failureLimiter
	dummy         string // hash verified when the email is unknown, so timing does not reveal accounts
	dummyMu       sync.Once
	csp           string // the Content-Security-Policy of every response (contentPolicy)
}

type route struct {
	method, path string
	handler      http.HandlerFunc
	public       bool // reachable without a session
	webhook      bool // a call from another server: no CSRF check (only with public)
	// role is the least role the signed-in person needs (store.Role*); empty: any member.
	// what says, for the refusal, what it is needed for.
	role, what string
}

// need is the route served only to a person whose role is at least role.
func (r route) need(role, what string) route {
	r.role, r.what = role, what
	return r
}

// routes is the single list of endpoints, the modules' included; a test keeps it in step
// with api/openapi.yaml. Who may call each is the permission matrix (team.go): a route with
// no role is every member's.
func (s *server) routes() []route {
	get, post, put, patch, del := http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete
	owner, admin := store.RoleOwner, store.RoleAdmin
	on := func(method, path string, h http.HandlerFunc) route {
		return route{method: method, path: path, handler: h}
	}
	builtIn := []route{
		{method: post, path: "/api/auth/setup", handler: s.passwordOnly(s.handleSetup), public: true},
		{method: post, path: "/api/auth/login", handler: s.passwordOnly(s.handleLogin), public: true},
		{method: post, path: "/api/auth/logout", handler: s.handleLogout, public: true},
		{method: get, path: "/api/auth/me", handler: s.handleMe, public: true},
		// Public only so that a sign-in build refuses it before asking for a session.
		{method: post, path: "/api/auth/password", handler: s.passwordOnly(s.requireSession(s.handlePasswordChange)), public: true},
		// Public only so that a build without account deletion refuses it before asking for a session.
		{method: post, path: "/api/auth/delete-account", handler: s.deletionOnly(s.requireSession(s.handleDeleteAccount)), public: true},

		// Mailboxes: everyone adds and manages their own (ownAccount); what they see is theirs
		// and the ones shared with them.
		on(get, "/api/presets", s.handlePresets),
		on(post, "/api/accounts/test", s.handleAccountTest),
		on(get, "/api/accounts", s.handleAccounts),
		on(post, "/api/accounts", s.handleAccountCreate),
		on(get, "/api/accounts/{id}", s.handleAccount),
		on(patch, "/api/accounts/{id}", s.handleAccountPatch),
		on(del, "/api/accounts/{id}", s.handleAccountDelete),
		on(post, "/api/accounts/{id}/reconnect", s.handleAccountReconnect),
		on(post, "/api/accounts/{id}/test", s.handleStoredAccountTest),
		on(get, "/api/accounts/{id}/folders", s.handleAccountFolders),

		// Rules are the team's: everyone reads them and undoes what they did to the mail they
		// see; writing, composing and testing them is an admin's.
		on(get, "/api/rules", s.handleRules),
		on(post, "/api/rules/compose", s.handleCompose).need(admin, toEditRules),
		on(post, "/api/rules/batch", s.handleRulesBatch).need(admin, toEditRules),
		on(post, "/api/rules/reorder", s.handleRulesReorder).need(admin, toEditRules),
		on(post, "/api/rules/test", s.handleRulesTest).need(admin, toEditRules),
		on(get, "/api/rules/export", s.handleRulesExport),
		on(post, "/api/rules/import", s.handleRulesImport).need(admin, toEditRules),
		on(get, "/api/rules/{id}", s.handleRule),
		on(patch, "/api/rules/{id}", s.handleRulePatch).need(admin, toEditRules),
		on(del, "/api/rules/{id}", s.handleRuleDelete).need(admin, toEditRules),
		on(post, "/api/rules/{id}/compose", s.handleRecompose).need(admin, toEditRules),
		on(post, "/api/rules/{id}/undo", s.handleRuleUndo),

		on(get, "/api/senders", s.handleSenders),
		on(put, "/api/senders/{type}/{value}", s.handleSenderPut).need(admin, toEditSenders),
		on(del, "/api/senders/{type}/{value}", s.handleSenderDelete).need(admin, toEditSenders),
		// Unsubscribing is everyone's; the sender rule it adds after is an admin's (handler).
		on(post, "/api/senders/{type}/{value}/unsubscribe", s.handleSenderUnsubscribe),

		// Mail: everyone acts on the mail they see. A correction that makes a sender rule
		// (always_for) and a Yes to a question are an admin's (handlers).
		on(get, "/api/activity", s.handleActivity),
		on(get, "/api/activity/questions", s.questionsOnly(s.handleQuestions)),
		on(post, "/api/activity/questions/{id}/answer", s.questionsOnly(s.handleQuestionAnswer)),
		on(get, "/api/messages/{id}", s.handleMessage),
		on(post, "/api/messages/{id}/correct", s.handleCorrect),
		on(post, "/api/messages/{id}/undo", s.handleMessageUndo),
		on(get, "/api/review", s.handleReview),
		on(post, "/api/review/{message_id}/resolve", s.handleReviewResolve),
		on(post, "/api/actions/undo", s.handleUndoSince),
		on(post, "/api/actions/{id}/undo", s.handleActionUndo),
		on(get, "/api/batches", s.handleBatches),
		on(get, "/api/batches/{id}", s.handleBatch),
		on(post, "/api/batches/{id}/undo", s.handleBatchUndo),

		on(post, "/api/cleanup/check", s.handleCleanupCheckStart),
		on(get, "/api/cleanup/check", s.handleCleanupCheckGet),
		on(get, "/api/cleanup/checks", s.handleCleanupChecks),
		on(del, "/api/cleanup/check", s.handleCleanupCheckDelete),
		on(put, "/api/cleanup/check/selection", s.handleCleanupSelection),
		on(post, "/api/cleanup/run", s.handleCleanupRun),
		on(get, "/api/templates", s.handleTemplates),
		on(get, "/api/stats/summary", s.handleStatsSummary),
		on(get, "/api/stats/usage", s.handleStatsUsage),
		on(get, "/api/limits", s.handleLimits),
		// Every role sees the plan's status; the plans on sale and their prices are an Owner's (planJSON).
		on(get, "/api/plan", s.billingOnly(s.handlePlan)),
		on(post, "/api/plan/checkout", s.billingOnly(s.handlePlanCheckout)).need(owner, toManagePlan),
		on(post, "/api/plan/change", s.billingOnly(s.handlePlanChange)).need(owner, toManagePlan),
		on(get, "/api/plan/portal", s.billingOnly(s.handlePlanPortal)).need(owner, toManagePlan),
		on(post, "/api/plan/company-sign-in", s.billingOnly(s.handlePlanCompanySignIn)).need(owner, toManagePlan),

		on(get, "/api/settings", s.handleSettings),
		on(patch, "/api/settings", s.handleSettingsPatch).need(admin, toEditSettings),
		on(get, "/api/settings/anthropic-workspaces", s.handleAnthropicWorkspaces).need(admin, toEditSettings),
		on(get, "/api/events", s.handleEvents),
		on(get, "/api/summary/preview", s.handleSummaryPreview),
		on(get, "/api/summary/preview.html", s.handleSummaryPreviewPage),
		on(post, "/api/summary/test", s.handleSummaryTest),

		on(get, "/api/notifications", s.notificationsOnly(s.handleNotifications)),
		on(post, "/api/notifications/destinations", s.notificationsOnly(s.handleDestinationCreate)).need(admin, toEditSettings),
		on(del, "/api/notifications/destinations/{id}", s.notificationsOnly(s.handleDestinationDelete)).need(admin, toEditSettings),

		// The team: everyone sees who is in it; admins invite, change roles and remove (only
		// an owner touches an owner: handlers), and download the audit log.
		on(get, "/api/members", s.teamOnly(s.handleMembers)),
		on(patch, "/api/members/{id}", s.teamOnly(s.handleMemberPatch)).need(admin, toManageMembers),
		on(del, "/api/members/{id}", s.teamOnly(s.handleMemberDelete)).need(admin, toManageMembers),
		on(get, "/api/invitations", s.teamOnly(s.handleInvitations)).need(admin, toManageMembers),
		on(post, "/api/invitations", s.teamOnly(s.handleInvitationCreate)).need(admin, toManageMembers),
		on(del, "/api/invitations/{id}", s.teamOnly(s.handleInvitationDelete)).need(admin, toManageMembers),
		on(get, "/api/audit", s.teamOnly(s.handleAudit)).need(admin, toSeeAudit),

		// Company sign-in: an owner or admin sees how it stands and opens the sign-in service's
		// admin portal to set it up, while the team may use it.
		on(get, "/api/company-sign-in", s.companySignInOnly(s.handleCompanySignIn)).need(admin, toSetUpSignIn),
		on(post, "/api/company-sign-in/portal", s.companySignInOnly(s.handleCompanySignInPortal)).need(admin, toSetUpSignIn),
	}
	return append(builtIn, s.moduleRoutes()...)
}

// NewHandler returns the daemon's HTTP routes.
func NewHandler(o Options) http.Handler {
	s := &server{Options: o, store: o.Store, secure: o.SecureCookies, now: o.Now, logins: newFailureLimiter(5, time.Minute), accountLogins: newFailureLimiter(maxAccountFailures, time.Minute),
		csp: contentPolicy(billingOf(o.Modules))}
	if s.now == nil {
		s.now = time.Now
	}
	if s.Hub == nil {
		s.Hub = events.NewHub()
	}
	if s.Metrics == nil {
		s.Metrics = telemetry.NewMetrics(o.Version)
	}
	if s.ConnectOAuth == nil {
		s.ConnectOAuth = func(context.Context, store.Account, *string) (mail.Mailbox, error) {
			return nil, fmt.Errorf("%w: this build has no one-click sign-in", mail.ErrReconnect)
		}
	}
	if s.Summary == nil {
		s.Summary = summary.New(o.Store, o.Settings.Env, nil)
		s.Summary.Now = s.now
	}
	if s.Unsubscriber == nil {
		s.Unsubscriber = unsubscribe.New(o.Resolver)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	// Ready means the daemon can do its work, which needs the database.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.store.Ping(r.Context()); err != nil {
			slog.ErrorContext(r.Context(), "not ready", "error", err.Error())
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("GET /metrics", s.Metrics.Handler())
	// A tenant whose plan has ended changes nothing but its plan (readOnly). A route that
	// needs a role refuses anyone with less first (requireRole).
	for _, r := range s.routes() {
		var h http.Handler = r.handler
		if !r.public {
			inner := r.handler
			if !readOnlyExempt(r.path) {
				inner = s.readOnly(r.handler).ServeHTTP
			}
			if r.role != "" {
				inner = s.requireRole(r.role, r.what, inner)
			}
			h = s.requireSession(inner)
		}
		if !r.webhook {
			h = s.csrf(h.ServeHTTP)
		}
		mux.Handle(r.method+" "+r.path, h)
	}
	// Everything else under /api/ is a JSON 404, so a mistyped endpoint never gets the UI's
	// index.html. The UI takes what is left; the patterns above are more specific and win.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) { notFound(w, "endpoint") })
	mux.Handle("/", web.Handler())
	return s.harden(mux)
}

// statusWriter remembers the status code for the request metrics.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write sends the body; a body sent without WriteHeader is a 200.
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach Flush for the event stream.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// pattern is what is logged and counted as a request's route: the pattern the mux matched,
// which it sets on the request before the handler runs, never the raw path.
type pattern struct{ r *http.Request }

func (p pattern) LogValue() slog.Value { return slog.StringValue(p.String()) }

func (p pattern) String() string {
	if p.r.Pattern == "" {
		return "unmatched"
	}
	return p.r.Pattern
}

// harden sets the security headers on every response and counts the request. What is
// logged with the request's context has its route and, once the session is known, its
// tenant (requireSession). A handler that panics is logged at error with its stack and
// answered 500, or, when it had begun its answer, cut off; the daemon goes on.
func (s *server) harden(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", s.csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		sw := &statusWriter{ResponseWriter: w}
		start := time.Now()
		ctx := telemetry.WithAttrs(r.Context())
		r = r.WithContext(ctx)
		telemetry.AddAttrs(ctx, slog.Any("route", pattern{r}))
		observe := func() {
			if sw.status == 0 {
				sw.status = http.StatusOK
			}
			s.Metrics.ObserveHTTP(r.Method, pattern{r}.String(), sw.status, time.Since(start))
		}
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) { // a handler ending the connection on purpose
				panic(v)
			}
			telemetry.LogPanic(ctx, "a request panicked", v, "method", r.Method)
			if sw.status != 0 {
				observe()
				panic(http.ErrAbortHandler) // the client must not take a cut-off answer for a whole one
			}
			writeError(sw, http.StatusInternalServerError, "internal", "Something went wrong on the server.", "")
			observe()
		}()
		next.ServeHTTP(sw, r)
		observe()
	})
}

type apiError struct {
	Code    string    `json:"code"`
	Message string    `json:"message"`
	Path    string    `json:"path,omitempty"`
	SignIn  string    `json:"sign_in,omitempty"` // where to sign in, when a module signs people in
	Cert    *certJSON `json:"cert,omitempty"`    // the server certificate to accept, on cert_untrusted and cert_changed
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("write response", "error", err.Error())
	}
}

func writeError(w http.ResponseWriter, status int, code, message, path string) {
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: message, Path: path}})
}

// statusClientClosed is what a request is counted as when the client dropped it before the
// answer was ready (nginx's 499). Nobody receives it.
const statusClientClosed = 499

// clientGone reports whether the client dropped the request, which has then been logged
// as a plain fact (not an error: nothing failed) and counted. Whatever failed after that
// failed because the request was cancelled, so the caller has nothing more to answer.
func clientGone(w http.ResponseWriter, r *http.Request) bool {
	if r.Context().Err() == nil {
		return false
	}
	slog.InfoContext(r.Context(), "request cancelled by the client", "method", r.Method, "path", r.URL.Path)
	w.WriteHeader(statusClientClosed)
	return true
}

// internalError logs the cause and tells the client nothing about it. A request that needs
// a secret the key service cannot open right now (crypto.ErrKeyUnavailable) is 503
// key_unavailable instead: nothing is wrong with it, and it works again once the service
// answers.
func internalError(w http.ResponseWriter, r *http.Request, err error) {
	if clientGone(w, r) {
		return
	}
	if errors.Is(err, crypto.ErrKeyUnavailable) {
		slog.WarnContext(r.Context(), "request needs a key the key service cannot unwrap right now", "method", r.Method, "path", r.URL.Path, "error", err.Error())
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusServiceUnavailable, "key_unavailable",
			"The key service that protects your stored passwords and keys cannot be reached right now. Try again in a minute.", "")
		return
	}
	slog.ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "error", err.Error())
	writeError(w, http.StatusInternalServerError, "internal", "Something went wrong on the server.", "")
}

// readJSON decodes a small JSON body, rejecting unknown fields.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "The request body is not valid JSON for this endpoint.", "")
		return false
	}
	return true
}

// readPatch decodes a JSON object into the fields named in dst (JSON key to pointer) and
// reports which keys were sent, so a field left out stays as it is and a field set to null
// is cleared. Unknown keys are rejected, as readJSON does.
func readPatch(w http.ResponseWriter, r *http.Request, dst map[string]any) (map[string]bool, bool) {
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&raw); err != nil || raw == nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "The request body is not valid JSON for this endpoint.", "")
		return nil, false
	}
	sent := map[string]bool{}
	for key, value := range raw {
		p, ok := dst[key]
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_json", "The request body is not valid JSON for this endpoint.", key)
			return nil, false
		}
		// A field that is sent replaces the old value whole: the decoder would otherwise
		// reuse what is there (a slice's old elements keep the fields the new ones leave out).
		reflect.ValueOf(p).Elem().SetZero()
		dec := json.NewDecoder(bytes.NewReader(value))
		dec.DisallowUnknownFields()
		if err := dec.Decode(p); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_input", "This field has the wrong type or shape.", key)
			return nil, false
		}
		sent[key] = true
	}
	return sent, true
}

func invalid(w http.ResponseWriter, path, message string) {
	writeError(w, http.StatusBadRequest, "invalid_input", message, path)
}

func notFound(w http.ResponseWriter, what string) {
	writeError(w, http.StatusNotFound, "not_found", "No such "+what+".", "")
}

// pathID reads an integer path parameter; anything else cannot name a row.
func pathID(w http.ResponseWriter, r *http.Request, name, what string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		notFound(w, what)
		return 0, false
	}
	return id, true
}

func user(r *http.Request) store.User {
	u, _ := r.Context().Value(userKey{}).(store.User)
	return u
}

// fail answers for an error from the store or the executor.
func fail(w http.ResponseWriter, r *http.Request, err error, what string) {
	if clientGone(w, r) {
		return
	}
	var noFolder *actions.NoFolderError
	switch {
	case errors.Is(err, store.ErrNotFound):
		notFound(w, what)
	case errors.Is(err, actions.ErrTooOld):
		writeError(w, http.StatusConflict, "too_old", fmt.Sprintf("This was done more than %d days ago, so it can no longer be undone.", store.UndoDays), "")
	case errors.Is(err, actions.ErrLeftReview):
		writeError(w, http.StatusConflict, "message_gone", "This email was moved or deleted outside MailRules, so it was taken off the list.", "")
	case errors.Is(err, actions.ErrGone):
		writeError(w, http.StatusConflict, "message_gone", "The message was moved or deleted outside MailRules, so this cannot be undone.", "")
	case errors.As(err, &noFolder):
		writeError(w, http.StatusUnprocessableEntity, "no_special_folder", "This mail account has no "+noFolder.Role+
			" folder, so that action cannot be carried out. Choose a rule that moves the mail to a named folder instead.", "")
	case errors.Is(err, worker.ErrNotConnected):
		writeError(w, http.StatusConflict, "account_offline", "The mail account is not connected right now. Try again once it is live.", "")
	case isMailError(err):
		slog.WarnContext(r.Context(), "mail server error", "method", r.Method, "path", r.URL.Path, "error", err.Error())
		writeError(w, http.StatusBadGateway, "mailbox_error", "The mail server refused the change or could not be reached. Try again.", "")
	default:
		internalError(w, r, err)
	}
}

func isMailError(err error) bool {
	for _, target := range []error{mail.ErrAuth, mail.ErrReconnect, mail.ErrTLS, mail.ErrConnection, mail.ErrThrottled, mail.ErrUnsupported, mail.ErrNoFolder, mail.ErrNotFound} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// ts is a timestamp that may be absent: 0 becomes null.
func ts(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}
