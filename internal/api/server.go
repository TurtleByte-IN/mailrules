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
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/actions"
	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/settings"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/telemetry"
	"github.com/TurtleByte-IN/mailrules/internal/web"
	"github.com/TurtleByte-IN/mailrules/internal/worker"
)

// Options configures the HTTP handler.
type Options struct {
	Store         *store.Store
	SecureCookies bool             // set when the UI is not served from localhost
	Now           func() time.Time // nil means time.Now

	Hub      *events.Hub        // the SSE stream's source; nil = a private hub nothing publishes to
	Exec     *actions.Exec      // undo and corrections: the only way this package changes a mailbox
	Settings *settings.Settings // the settings the user changes at runtime
	Models   ModelSource        // the models the composer and tester use; nil = Settings
	Master   []byte             // seals account passwords
	Metrics  *telemetry.Metrics // nil = a private registry
	Version  string             // shown in GET /api/settings

	// StartCheck starts a cleanup check of an account in the background and returns it,
	// running (worker.Manager.StartCheck). Checks holds the account checks in memory, and
	// Sort applies a finished check's kept rows as one undoable batch (worker.Manager.Sort).
	StartCheck func(c worker.Cleanup, fingerprint string, run worker.CheckFunc) (*worker.Check, error)
	Checks     *worker.CheckStore
	Sort       func(ctx context.Context, sr worker.SortRun) (store.Batch, error)
	// Connect logs in to a mail server with credentials that are not stored yet, trying
	// the usernames the preset allows, and returns the connection with the username that worked.
	Connect func(ctx context.Context, acct store.Account, password string) (mail.Mailbox, string, error)
	// StartAccount (re)starts watching an account; StopAccount stops it and waits until
	// mail already queued is finished.
	StartAccount func(acct store.Account)
	StopAccount  func(accountID int64)
}

type server struct {
	Options
	store   *store.Store
	secure  bool
	now     func() time.Time
	logins  *failureLimiter
	dummy   string // hash verified when the email is unknown, so timing does not reveal accounts
	dummyMu sync.Once
}

type route struct {
	method, path string
	handler      http.HandlerFunc
	public       bool // reachable without a session
}

// routes is the single list of endpoints; a test keeps it in step with api/openapi.yaml.
func (s *server) routes() []route {
	get, post, put, patch, del := http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete
	on := func(method, path string, h http.HandlerFunc) route {
		return route{method: method, path: path, handler: h}
	}
	return []route{
		{post, "/api/auth/setup", s.handleSetup, true},
		{post, "/api/auth/login", s.handleLogin, true},
		{post, "/api/auth/logout", s.handleLogout, true},
		{get, "/api/auth/me", s.handleMe, true},

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

		on(get, "/api/rules", s.handleRules),
		on(post, "/api/rules/compose", s.handleCompose),
		on(post, "/api/rules/batch", s.handleRulesBatch),
		on(post, "/api/rules/reorder", s.handleRulesReorder),
		on(post, "/api/rules/test", s.handleRulesTest),
		on(post, "/api/rules/suggest", s.handleRulesSuggest),
		on(get, "/api/rules/export", s.handleRulesExport),
		on(post, "/api/rules/import", s.handleRulesImport),
		on(get, "/api/rules/{id}", s.handleRule),
		on(patch, "/api/rules/{id}", s.handleRulePatch),
		on(del, "/api/rules/{id}", s.handleRuleDelete),
		on(post, "/api/rules/{id}/compose", s.handleRecompose),
		on(post, "/api/rules/{id}/undo", s.handleRuleUndo),

		on(get, "/api/senders", s.handleSenders),
		on(put, "/api/senders/{type}/{value}", s.handleSenderPut),
		on(del, "/api/senders/{type}/{value}", s.handleSenderDelete),

		on(get, "/api/activity", s.handleActivity),
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
		on(del, "/api/cleanup/check", s.handleCleanupCheckDelete),
		on(put, "/api/cleanup/check/selection", s.handleCleanupSelection),
		on(post, "/api/cleanup/run", s.handleCleanupRun),
		on(get, "/api/templates", s.handleTemplates),
		on(get, "/api/stats/summary", s.handleStatsSummary),
		on(get, "/api/stats/usage", s.handleStatsUsage),

		on(get, "/api/settings", s.handleSettings),
		on(patch, "/api/settings", s.handleSettingsPatch),
		on(get, "/api/settings/anthropic-workspaces", s.handleAnthropicWorkspaces),
		on(get, "/api/events", s.handleEvents),
	}
}

// NewHandler returns the daemon's HTTP routes.
func NewHandler(o Options) http.Handler {
	s := &server{Options: o, store: o.Store, secure: o.SecureCookies, now: o.Now, logins: newFailureLimiter(5, time.Minute)}
	if s.now == nil {
		s.now = time.Now
	}
	if s.Hub == nil {
		s.Hub = events.NewHub()
	}
	if s.Metrics == nil {
		s.Metrics = telemetry.NewMetrics(o.Version)
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
	for _, r := range s.routes() {
		h := r.handler
		if !r.public {
			h = s.requireSession(h)
		}
		mux.Handle(r.method+" "+r.path, s.csrf(h))
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

// Unwrap lets http.ResponseController reach Flush for the event stream.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// harden sets the security headers on every response and counts the request.
func (s *server) harden(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		sw := &statusWriter{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(sw, r)
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		pattern := r.Pattern // set by the mux; empty when nothing matched
		if pattern == "" {
			pattern = "unmatched"
		}
		s.Metrics.ObserveHTTP(r.Method, pattern, sw.status, time.Since(start))
	})
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Path    string `json:"path,omitempty"`
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

// internalError logs the cause and tells the client nothing about it.
func internalError(w http.ResponseWriter, r *http.Request, err error) {
	if clientGone(w, r) {
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
	for _, target := range []error{mail.ErrAuth, mail.ErrTLS, mail.ErrConnection, mail.ErrThrottled, mail.ErrUnsupported, mail.ErrNoFolder, mail.ErrNotFound} {
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
