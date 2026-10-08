package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// client is a browser stand-in: it keeps cookies and echoes the CSRF token.
type client struct {
	t       *testing.T
	h       http.Handler
	cookies map[string]string
	noCSRF  bool
}

type reply struct {
	status int
	header http.Header
	body   struct {
		User  userJSON `json:"user"`
		Error apiError `json:"error"`
	}
	setCookies map[string]*http.Cookie
	raw        []byte
}

// object is the response body as a JSON object.
func (r reply) object(t *testing.T) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(r.raw, &v); err != nil {
		t.Fatalf("body is not a JSON object: %s", r.raw)
	}
	return v
}

func (c *client) do(method, path, body string) reply {
	c.t.Helper()
	return c.send(c.t.Context(), method, path, body, nil)
}

// send is do with a request context of its own (cancel it to play a client that drops the
// request) and extra request headers.
func (c *client) send(ctx context.Context, method, path, body string, header http.Header) reply {
	c.t.Helper()
	req := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
	for name, values := range header {
		req.Header[name] = values
	}
	for name, value := range c.cookies {
		req.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	if !c.noCSRF {
		req.Header.Set(csrfHeader, c.cookies[csrfCookie])
	}
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	out := reply{status: rec.Code, header: rec.Header(), setCookies: map[string]*http.Cookie{}, raw: rec.Body.Bytes()}
	for _, ck := range rec.Result().Cookies() {
		out.setCookies[ck.Name] = ck
		if ck.MaxAge < 0 {
			delete(c.cookies, ck.Name)
		} else {
			c.cookies[ck.Name] = ck.Value
		}
	}
	if rec.Header().Get("Content-Type") == "application/json" {
		if err := json.Unmarshal(rec.Body.Bytes(), &out.body); err != nil {
			c.t.Fatalf("%s %s: body is not JSON: %s", method, path, rec.Body.String())
		}
	}
	return out
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newClient(t *testing.T) (*client, *clock) {
	t.Helper()
	e := newEnv(t)
	return e.client, e.ck
}

func expect(t *testing.T, r reply, status int, code string) {
	t.Helper()
	if r.status != status || r.body.Error.Code != code {
		t.Fatalf("got %d %q (%s), want %d %q", r.status, r.body.Error.Code, r.body.Error.Message, status, code)
	}
}

const goodBody = `{"email":"me@icloud.com","password":"correct horse battery"}`

func TestHealthz(t *testing.T) {
	c, _ := newClient(t)
	if r := c.do(http.MethodGet, "/healthz", ""); r.status != http.StatusOK {
		t.Fatalf("healthz = %d", r.status)
	}
}

// The embedded UI answers every path the API and the ops endpoints do not claim, and
// never one of theirs.
func TestUIAndAPIShareTheRouter(t *testing.T) {
	c, _ := newClient(t)
	home := c.do(http.MethodGet, "/", "")
	if home.status != http.StatusOK || !strings.HasPrefix(home.header.Get("Content-Type"), "text/html") {
		t.Fatalf("GET / = %d %q, want the UI's index.html", home.status, home.header.Get("Content-Type"))
	}
	if got := home.header.Get("Content-Security-Policy"); got != "default-src 'self'" {
		t.Errorf("GET / Content-Security-Policy = %q", got)
	}
	if got := home.header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("GET / Cache-Control = %q, want no-cache", got)
	}
	// A client-side route gets the same page.
	if r := c.do(http.MethodGet, "/rules/12", ""); r.status != http.StatusOK || string(r.raw) != string(home.raw) {
		t.Errorf("GET /rules/12 = %d, want index.html", r.status)
	}
	// The API still answers JSON, for a real endpoint and for one that does not exist.
	if r := c.do(http.MethodGet, "/api/auth/me", ""); r.status != http.StatusUnauthorized || r.body.Error.Code != "setup_required" {
		t.Errorf("GET /api/auth/me = %d %q, want 401 setup_required", r.status, r.body.Error.Code)
	}
	for _, path := range []string{"/api/nope", "/api/", "/api/rules/1/nope"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			r := c.do(method, path, "")
			if r.status != http.StatusNotFound || r.body.Error.Code != "not_found" || r.header.Get("Content-Type") != "application/json" {
				t.Errorf("%s %s = %d %q %q, want a JSON 404", method, path, r.status, r.header.Get("Content-Type"), r.raw)
			}
		}
	}
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		if r := c.do(http.MethodGet, path, ""); r.status != http.StatusOK || strings.HasPrefix(r.header.Get("Content-Type"), "text/html") {
			t.Errorf("GET %s = %d %q, want the ops endpoint", path, r.status, r.header.Get("Content-Type"))
		}
		if r := c.do(http.MethodPost, path, ""); r.status != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d, want 405", path, r.status)
		}
	}
}

func TestSetupLoginLogout(t *testing.T) {
	c, _ := newClient(t)

	// A POST before any GET has no CSRF cookie to echo.
	expect(t, c.do(http.MethodPost, "/api/auth/setup", goodBody), http.StatusForbidden, "csrf_failed")

	r := c.do(http.MethodGet, "/api/auth/me", "")
	expect(t, r, http.StatusUnauthorized, "setup_required")
	if c.cookies[csrfCookie] == "" {
		t.Fatal("no CSRF cookie handed out")
	}

	c.noCSRF = true
	expect(t, c.do(http.MethodPost, "/api/auth/setup", goodBody), http.StatusForbidden, "csrf_failed")
	c.noCSRF = false

	for body, path := range map[string]string{
		`{"email":"nope","password":"correct horse battery"}`: "email",
		`{"email":"me@icloud.com","password":"short"}`:        "password",
	} {
		r := c.do(http.MethodPost, "/api/auth/setup", body)
		expect(t, r, http.StatusBadRequest, "invalid_input")
		if r.body.Error.Path != path {
			t.Errorf("error path = %q, want %q", r.body.Error.Path, path)
		}
	}
	expect(t, c.do(http.MethodPost, "/api/auth/setup", `{"email":`), http.StatusBadRequest, "invalid_json")
	expect(t, c.do(http.MethodPost, "/api/auth/setup", `{"email":"a@b","password":"correct horse battery","admin":true}`), http.StatusBadRequest, "invalid_json")

	r = c.do(http.MethodPost, "/api/auth/setup", goodBody)
	if r.status != http.StatusCreated || r.body.User.Email != "me@icloud.com" {
		t.Fatalf("setup = %d %+v", r.status, r.body)
	}
	sc := r.setCookies[sessionCookie]
	if sc == nil || !sc.HttpOnly || sc.SameSite != http.SameSiteStrictMode || sc.Secure {
		t.Fatalf("session cookie flags wrong: %+v", sc)
	}
	if r := c.do(http.MethodGet, "/api/auth/me", ""); r.status != http.StatusOK || r.body.User.ID == 0 {
		t.Fatalf("me after setup = %d", r.status)
	}
	expect(t, c.do(http.MethodPost, "/api/auth/setup", goodBody), http.StatusConflict, "already_set_up")

	if r := c.do(http.MethodPost, "/api/auth/logout", ""); r.status != http.StatusNoContent {
		t.Fatalf("logout = %d", r.status)
	}
	expect(t, c.do(http.MethodGet, "/api/auth/me", ""), http.StatusUnauthorized, "unauthenticated")
	if r := c.do(http.MethodPost, "/api/auth/logout", ""); r.status != http.StatusNoContent {
		t.Fatalf("logout when signed out = %d", r.status)
	}

	// Signing in again rotates the token: the old one stops working.
	if r := c.do(http.MethodPost, "/api/auth/login", goodBody); r.status != http.StatusOK {
		t.Fatalf("login = %d", r.status)
	}
	first := c.cookies[sessionCookie]
	if r := c.do(http.MethodPost, "/api/auth/login", goodBody); r.status != http.StatusOK {
		t.Fatalf("second login = %d", r.status)
	}
	if c.cookies[sessionCookie] == first {
		t.Fatal("session token not rotated on login")
	}
	c.cookies[sessionCookie] = first
	expect(t, c.do(http.MethodGet, "/api/auth/me", ""), http.StatusUnauthorized, "unauthenticated")
}

func TestSessionExpiry(t *testing.T) {
	c, ck := newClient(t)
	c.do(http.MethodGet, "/api/auth/me", "")
	c.do(http.MethodPost, "/api/auth/setup", goodBody)

	// Use within the window slides it: 20 days + 20 days is past the original 30.
	ck.t = ck.t.Add(20 * 24 * time.Hour)
	if r := c.do(http.MethodGet, "/api/auth/me", ""); r.status != http.StatusOK {
		t.Fatalf("day 20 = %d", r.status)
	}
	ck.t = ck.t.Add(20 * 24 * time.Hour)
	if r := c.do(http.MethodGet, "/api/auth/me", ""); r.status != http.StatusOK {
		t.Fatalf("day 40 after sliding = %d", r.status)
	}
	ck.t = ck.t.Add(31 * 24 * time.Hour)
	expect(t, c.do(http.MethodGet, "/api/auth/me", ""), http.StatusUnauthorized, "unauthenticated")
}

func TestLoginRateLimit(t *testing.T) {
	c, ck := newClient(t)
	c.do(http.MethodGet, "/api/auth/me", "")
	c.do(http.MethodPost, "/api/auth/setup", goodBody)
	c.do(http.MethodPost, "/api/auth/logout", "")

	wrong := `{"email":"me@icloud.com","password":"wrong wrong wrong"}`
	unknown := `{"email":"nobody@icloud.com","password":"correct horse battery"}`
	for i, body := range []string{wrong, unknown, wrong, unknown, wrong} {
		r := c.do(http.MethodPost, "/api/auth/login", body)
		if r.status != http.StatusUnauthorized || r.body.Error.Code != "invalid_credentials" {
			t.Fatalf("attempt %d = %d %q", i+1, r.status, r.body.Error.Code)
		}
	}
	// The sixth is refused even with the right password.
	r := c.do(http.MethodPost, "/api/auth/login", goodBody)
	expect(t, r, http.StatusTooManyRequests, "rate_limited")
	if r.header.Get("Retry-After") == "" {
		t.Error("no Retry-After header")
	}
	ck.t = ck.t.Add(61 * time.Second)
	if r := c.do(http.MethodPost, "/api/auth/login", goodBody); r.status != http.StatusOK {
		t.Fatalf("login after the window = %d %q", r.status, r.body.Error.Code)
	}
}

func passwordBody(current, next string) string {
	b, _ := json.Marshal(map[string]string{"current_password": current, "new_password": next})
	return string(b)
}

const newPassword = "a different long password"

func TestChangePassword(t *testing.T) {
	c, _ := newClient(t)
	c.do(http.MethodGet, "/api/auth/me", "")

	// No session: no change, and the answer says which screen to show.
	expect(t, c.do(http.MethodPost, "/api/auth/password", passwordBody("correct horse battery", newPassword)), http.StatusUnauthorized, "setup_required")

	c.do(http.MethodPost, "/api/auth/setup", goodBody)
	// A second browser holds its own session.
	other := &client{t: t, h: c.h, cookies: map[string]string{}}
	other.do(http.MethodGet, "/api/auth/me", "")
	if r := other.do(http.MethodPost, "/api/auth/login", goodBody); r.status != http.StatusOK {
		t.Fatalf("second browser login = %d", r.status)
	}
	before := c.cookies[sessionCookie]

	// A request without the CSRF token is refused before anything else.
	c.noCSRF = true
	expect(t, c.do(http.MethodPost, "/api/auth/password", passwordBody("correct horse battery", newPassword)), http.StatusForbidden, "csrf_failed")
	c.noCSRF = false

	bad := []struct {
		name, body, code, path string
	}{
		{"wrong current password", passwordBody("not my password", newPassword), "invalid_input", "current_password"},
		{"empty current password", passwordBody("", newPassword), "invalid_input", "current_password"},
		{"new password too short", passwordBody("correct horse battery", "short"), "invalid_input", "new_password"},
		{"new password missing", `{"current_password":"correct horse battery"}`, "invalid_input", "new_password"},
		{"unknown field", `{"current_password":"correct horse battery","new_password":"` + newPassword + `","email":"x@y.z"}`, "invalid_json", ""},
		{"not JSON", `{"current_password":`, "invalid_json", ""},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			r := c.do(http.MethodPost, "/api/auth/password", tt.body)
			expect(t, r, http.StatusBadRequest, tt.code)
			if r.body.Error.Path != tt.path {
				t.Errorf("error path = %q, want %q", r.body.Error.Path, tt.path)
			}
			if _, replaced := r.setCookies[sessionCookie]; replaced {
				t.Error("a refused change replaced the session")
			}
		})
	}
	// Nothing changed: both browsers are still in, and the old password still works.
	if r := other.do(http.MethodGet, "/api/auth/me", ""); r.status != http.StatusOK {
		t.Fatalf("other browser after refused changes = %d", r.status)
	}

	r := c.do(http.MethodPost, "/api/auth/password", passwordBody("correct horse battery", newPassword))
	if r.status != http.StatusOK || r.body.User.Email != "me@icloud.com" {
		t.Fatalf("change = %d %+v", r.status, r.body)
	}
	sc := r.setCookies[sessionCookie]
	if sc == nil || !sc.HttpOnly || sc.SameSite != http.SameSiteStrictMode || sc.Value == before {
		t.Fatalf("session cookie after a change = %+v, want a fresh HttpOnly one", sc)
	}
	if strings.Contains(string(r.raw), newPassword) || strings.Contains(string(r.raw), "correct horse") {
		t.Fatalf("response echoes a password: %s", r.raw)
	}
	// This browser stays signed in on the new session; the other one, and the old token, do not.
	if r := c.do(http.MethodGet, "/api/auth/me", ""); r.status != http.StatusOK {
		t.Fatalf("me after change = %d", r.status)
	}
	expect(t, other.do(http.MethodGet, "/api/auth/me", ""), http.StatusUnauthorized, "unauthenticated")
	stale := &client{t: t, h: c.h, cookies: map[string]string{sessionCookie: before}}
	expect(t, stale.do(http.MethodGet, "/api/auth/me", ""), http.StatusUnauthorized, "unauthenticated")

	// The old password no longer signs in; the new one does.
	c.do(http.MethodPost, "/api/auth/logout", "")
	expect(t, c.do(http.MethodPost, "/api/auth/login", goodBody), http.StatusUnauthorized, "invalid_credentials")
	if r := c.do(http.MethodPost, "/api/auth/login", `{"email":"me@icloud.com","password":"`+newPassword+`"}`); r.status != http.StatusOK {
		t.Fatalf("login with the new password = %d %q", r.status, r.body.Error.Code)
	}
}

// Guessing the current password from a stolen session is limited like signing in.
func TestChangePasswordRateLimit(t *testing.T) {
	c, ck := newClient(t)
	c.do(http.MethodGet, "/api/auth/me", "")
	c.do(http.MethodPost, "/api/auth/setup", goodBody)

	for i := range 5 {
		expect(t, c.do(http.MethodPost, "/api/auth/password", passwordBody("guess "+strconv.Itoa(i)+" guess", newPassword)), http.StatusBadRequest, "invalid_input")
	}
	// The sixth is refused even with the right password.
	r := c.do(http.MethodPost, "/api/auth/password", passwordBody("correct horse battery", newPassword))
	expect(t, r, http.StatusTooManyRequests, "rate_limited")
	if r.header.Get("Retry-After") == "" {
		t.Error("no Retry-After header")
	}
	// It does not lock sign-in, which has its own count.
	c.do(http.MethodPost, "/api/auth/logout", "")
	if r := c.do(http.MethodPost, "/api/auth/login", goodBody); r.status != http.StatusOK {
		t.Fatalf("login while password changes are limited = %d", r.status)
	}
	ck.t = ck.t.Add(61 * time.Second)
	if r := c.do(http.MethodPost, "/api/auth/password", passwordBody("correct horse battery", newPassword)); r.status != http.StatusOK {
		t.Fatalf("change after the window = %d %q", r.status, r.body.Error.Code)
	}
}
