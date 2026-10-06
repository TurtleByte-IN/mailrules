package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	req := httptest.NewRequestWithContext(c.t.Context(), method, path, strings.NewReader(body))
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
