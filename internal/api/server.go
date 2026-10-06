// Package api serves the HTTP API described in api/openapi.yaml.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	planned      bool // contract only: answers 501 until its milestone (x-status: planned in the spec)
}

// routes is the single list of endpoints; a test keeps it in step with api/openapi.yaml.
func (s *server) routes() []route {
	get, post, put, patch, del := http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete
	on := func(method, path string, h http.HandlerFunc) route {
		return route{method: method, path: path, handler: h}
	}
	later := func(method, path string) route {
		return route{method: method, path: path, handler: notImplemented, planned: true}
	}
	return []route{
		{post, "/api/auth/setup", s.handleSetup, true, false},
		{post, "/api/auth/login", s.handleLogin, true, false},
		{post, "/api/auth/logout", s.handleLogout, true, false},
		{get, "/api/auth/me", s.handleMe, true, false},

		on(get, "/api/presets", s.handlePresets),
		on(post, "/api/accounts/test", s.handleAccountTest),
		on(get, "/api/accounts", s.handleAccounts),
		on(post, "/api/accounts", s.handleAccountCreate),
		on(get, "/api/accounts/{id}", s.handleAccount),
		on(patch, "/api/accounts/{id}", s.handleAccountPatch),
		on(del, "/api/accounts/{id}", s.handleAccountDelete),
		on(post, "/api/accounts/{id}/reconnect", s.handleAccountReconnect),
		on(get, "/api/accounts/{id}/folders", s.handleAccountFolders),

		on(get, "/api/rules", s.handleRules),
		on(post, "/api/rules/compose", s.handleCompose),
		on(post, "/api/rules/batch", s.handleRulesBatch),
		on(post, "/api/rules/reorder", s.handleRulesReorder),
		on(post, "/api/rules/test", s.handleRulesTest),
		on(get, "/api/rules/export", s.handleRulesExport),
		on(post, "/api/rules/import", s.handleRulesImport),
		on(get, "/api/rules/{id}", s.handleRule),
		on(patch, "/api/rules/{id}", s.handleRulePatch),
		on(del, "/api/rules/{id}", s.handleRuleDelete),
		on(post, "/api/rules/{id}/compose", s.handleRecompose),
		on(post, "/api/rules/{id}/undo", s.handleRuleUndo),

		later(get, "/api/senders"),                // M9
		later(put, "/api/senders/{type}/{value}"), // M9
		later(del, "/api/senders/{type}/{value}"), // M9

		on(get, "/api/activity", s.handleActivity),
		on(get, "/api/messages/{id}", s.handleMessage),
		on(post, "/api/messages/{id}/correct", s.handleCorrect),
		on(get, "/api/review", s.handleReview),
		on(post, "/api/review/{message_id}/resolve", s.handleReviewResolve),
		on(post, "/api/actions/undo", s.handleUndoSince),
		on(post, "/api/actions/{id}/undo", s.handleActionUndo),
		on(get, "/api/batches/{id}", s.handleBatch),
		on(post, "/api/batches/{id}/undo", s.handleBatchUndo),

		later(post, "/api/cleanup/preview"), // M9
		later(post, "/api/cleanup/run"),     // M9
		later(get, "/api/templates"),        // M9
		later(get, "/api/stats/summary"),    // M9
		later(get, "/api/stats/usage"),      // M9

		on(get, "/api/settings", s.handleSettings),
		on(patch, "/api/settings", s.handleSettingsPatch),
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

func notImplemented(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotImplemented, "not_implemented", "This part of MailRules is not built yet.", "")
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

// internalError logs the cause and tells the client nothing about it.
func internalError(w http.ResponseWriter, r *http.Request, err error) {
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
	switch {
	case errors.Is(err, store.ErrNotFound):
		notFound(w, what)
	case errors.Is(err, actions.ErrGone):
		writeError(w, http.StatusConflict, "message_gone", "The message was moved or deleted outside MailRules, so this cannot be undone.", "")
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
