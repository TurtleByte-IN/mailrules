package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/crypto"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

const (
	sessionCookie = "mailrules_session"
	csrfCookie    = "mailrules_csrf"
	csrfHeader    = "X-CSRF-Token"
	sessionTTL    = 30 * 24 * time.Hour
	minPassword   = crypto.MinPasswordLen
)

type userKey struct{}

type userJSON struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
}

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *server) setCookie(w http.ResponseWriter, name, value string, httpOnly bool, maxAge int) {
	// Secure is off only on localhost (plain HTTP); the CSRF cookie must be readable by the UI.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: both exceptions are deliberate, see above
		Name: name, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: httpOnly, Secure: s.secure, SameSite: http.SameSiteStrictMode,
	})
}

// csrf is double-submit: every response hands out a token cookie, and every
// non-GET request must echo it in the X-CSRF-Token header.
func (s *server) csrf(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(csrfCookie)
		if err != nil || c.Value == "" {
			token, err := randomToken()
			if err != nil {
				internalError(w, r, err)
				return
			}
			s.setCookie(w, csrfCookie, token, false, int(sessionTTL.Seconds()))
			c = nil
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			header := r.Header.Get(csrfHeader)
			if c == nil || header == "" || subtle.ConstantTimeCompare([]byte(header), []byte(c.Value)) != 1 {
				writeError(w, http.StatusForbidden, "csrf_failed", "Missing or wrong X-CSRF-Token header. Call GET /api/auth/me first to receive the token cookie.", "")
				return
			}
		}
		next(w, r)
	})
}

// currentUser resolves the session cookie, sliding the expiry forward at most once an hour.
func (s *server) currentUser(r *http.Request) (store.User, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return store.User{}, store.ErrNotFound
	}
	now := s.now()
	hash := hashToken(c.Value)
	u, expiresAt, err := s.store.SessionUser(r.Context(), hash, now.Unix())
	if err != nil {
		return store.User{}, err
	}
	if fresh := now.Add(sessionTTL).Unix(); fresh-expiresAt > 3600 {
		if err := s.store.ExtendSession(r.Context(), hash, fresh); err != nil {
			return store.User{}, err
		}
	}
	return u, nil
}

func (s *server) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := s.currentUser(r)
		if err != nil {
			s.writeNoSession(w, r, err)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey{}, u)))
	}
}

// writeNoSession tells the UI which screen to show: first-run setup or login.
func (s *server) writeNoSession(w http.ResponseWriter, r *http.Request, err error) {
	if !errors.Is(err, store.ErrNotFound) {
		internalError(w, r, err)
		return
	}
	has, err := s.store.HasUsers(r.Context())
	if err != nil {
		internalError(w, r, err)
		return
	}
	if !has {
		writeError(w, http.StatusUnauthorized, "setup_required", "No admin account yet. Run first-time setup.", "")
		return
	}
	writeError(w, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.", "")
}

// startSession replaces any session the browser already holds with a fresh one.
func (s *server) startSession(w http.ResponseWriter, r *http.Request, userID int64) error {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if err := s.store.DeleteSession(r.Context(), hashToken(c.Value)); err != nil {
			return err
		}
	}
	token, err := randomToken()
	if err != nil {
		return err
	}
	now := s.now()
	if err := s.store.CreateSession(r.Context(), hashToken(token), userID, now.Unix(), now.Add(sessionTTL).Unix()); err != nil {
		return err
	}
	s.setCookie(w, sessionCookie, token, true, int(sessionTTL.Seconds()))
	return nil
}

func (s *server) handleSetup(w http.ResponseWriter, r *http.Request) {
	var in credentials
	if !readJSON(w, r, &in) {
		return
	}
	in.Email = strings.TrimSpace(in.Email)
	if !strings.Contains(in.Email, "@") {
		writeError(w, http.StatusBadRequest, "invalid_input", "Enter an email address.", "email")
		return
	}
	if len(in.Password) < minPassword {
		writeError(w, http.StatusBadRequest, "invalid_input", "Password must be at least 12 characters.", "password")
		return
	}
	if has, err := s.store.HasUsers(r.Context()); err != nil {
		internalError(w, r, err)
		return
	} else if has {
		writeError(w, http.StatusConflict, "already_set_up", "Setup is already done. Sign in instead.", "")
		return
	}
	hash, err := crypto.HashPassword(in.Password)
	if err != nil {
		internalError(w, r, err)
		return
	}
	u, err := s.store.CreateFirstUser(r.Context(), in.Email, hash, s.now().Unix())
	if errors.Is(err, store.ErrAlreadySetUp) {
		writeError(w, http.StatusConflict, "already_set_up", "Setup is already done. Sign in instead.", "")
		return
	}
	if err != nil {
		internalError(w, r, err)
		return
	}
	if err := s.startSession(w, r, u.ID); err != nil {
		internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]userJSON{"user": {ID: u.ID, Email: u.Email}})
}

func (s *server) tooManyLogins(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
	writeError(w, http.StatusTooManyRequests, "rate_limited", "Too many failed sign-ins. Wait a minute and try again.", "")
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	now := s.now()
	if wait := s.logins.blockedFor(ip, now); wait > 0 {
		s.tooManyLogins(w, wait)
		return
	}
	var in credentials
	if !readJSON(w, r, &in) {
		return
	}
	u, err := s.store.UserByEmail(r.Context(), strings.TrimSpace(in.Email))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		internalError(w, r, err)
		return
	}
	hash := u.PasswordHash
	if hash == "" {
		s.dummyMu.Do(func() { s.dummy, _ = crypto.HashPassword("no such account") })
		hash = s.dummy
	}
	if !crypto.VerifyPassword(hash, in.Password) || u.ID == 0 {
		s.logins.fail(ip, now)
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "Wrong email or password.", "")
		return
	}
	if err := s.startSession(w, r, u.ID); err != nil {
		internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]userJSON{"user": {ID: u.ID, Email: u.Email}})
}

type passwordChange struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// handlePasswordChange replaces the signed-in admin's password. It needs the current
// password, so a stolen session alone cannot take the account over, and it ends every
// session, this browser's too: this browser then gets a fresh one so the owner stays in.
func (s *server) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	u, _ := r.Context().Value(userKey{}).(store.User)
	// Wrong guesses at the current password are counted per user, in the sign-in limiter
	// under a key no IP address can equal, so a stolen session cannot brute-force it.
	key := "password:" + strconv.FormatInt(u.ID, 10)
	now := s.now()
	if wait := s.logins.blockedFor(key, now); wait > 0 {
		s.tooManyLogins(w, wait)
		return
	}
	var in passwordChange
	if !readJSON(w, r, &in) {
		return
	}
	if !crypto.VerifyPassword(u.PasswordHash, in.CurrentPassword) {
		s.logins.fail(key, now)
		writeError(w, http.StatusBadRequest, "invalid_input", "The current password is wrong.", "current_password")
		return
	}
	if len(in.NewPassword) < minPassword {
		writeError(w, http.StatusBadRequest, "invalid_input", "Password must be at least 12 characters.", "new_password")
		return
	}
	hash, err := crypto.HashPassword(in.NewPassword)
	if err != nil {
		internalError(w, r, err)
		return
	}
	if err := s.store.SetPassword(r.Context(), u.ID, hash); err != nil {
		internalError(w, r, err)
		return
	}
	if err := s.startSession(w, r, u.ID); err != nil {
		internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]userJSON{"user": {ID: u.ID, Email: u.Email}})
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if err := s.store.DeleteSession(r.Context(), hashToken(c.Value)); err != nil {
			internalError(w, r, err)
			return
		}
	}
	s.setCookie(w, sessionCookie, "", true, -1)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleMe(w http.ResponseWriter, r *http.Request) {
	u, err := s.currentUser(r)
	if err != nil {
		s.writeNoSession(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]userJSON{"user": {ID: u.ID, Email: u.Email}})
}

// failureLimiter blocks a client after max failures inside window.
// ponytail: in process memory, so a second daemon instance would count separately;
// move to the sessions database if the daemon ever runs as more than one process.
type failureLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	fails  map[string][]time.Time
}

func newFailureLimiter(limit int, window time.Duration) *failureLimiter {
	return &failureLimiter{max: limit, window: window, fails: map[string][]time.Time{}}
}

// recent drops failures older than the window. Caller holds mu.
func (l *failureLimiter) recent(key string, now time.Time) []time.Time {
	kept := l.fails[key][:0]
	for _, t := range l.fails[key] {
		if now.Sub(t) < l.window {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.fails, key)
		return nil
	}
	l.fails[key] = kept
	return kept
}

func (l *failureLimiter) blockedFor(key string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.recent(key, now)
	if len(kept) < l.max {
		return 0
	}
	return l.window - now.Sub(kept[0])
}

func (l *failureLimiter) fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails[key] = append(l.recent(key, now), now)
}
