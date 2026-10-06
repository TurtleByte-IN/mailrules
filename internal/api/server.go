// Package api serves the HTTP API described in api/openapi.yaml.
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// Options configures the HTTP handler.
type Options struct {
	Store         *store.Store
	SecureCookies bool             // set when the UI is not served from localhost
	Now           func() time.Time // nil means time.Now
}

type server struct {
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
	return []route{
		{http.MethodPost, "/api/auth/setup", s.handleSetup, true},
		{http.MethodPost, "/api/auth/login", s.handleLogin, true},
		{http.MethodPost, "/api/auth/logout", s.handleLogout, true},
		{http.MethodGet, "/api/auth/me", s.handleMe, true},
	}
}

// NewHandler returns the daemon's HTTP routes.
func NewHandler(o Options) http.Handler {
	s := &server{store: o.Store, secure: o.SecureCookies, now: o.Now, logins: newFailureLimiter(5, time.Minute)}
	if s.now == nil {
		s.now = time.Now
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	for _, r := range s.routes() {
		h := r.handler
		if !r.public {
			h = s.requireSession(h)
		}
		mux.Handle(r.method+" "+r.path, s.csrf(h))
	}
	return mux
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
