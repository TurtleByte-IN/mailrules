package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/store"
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
	out := reply{status: rec.Code, header: rec.Header(), setCookies: map[string]*http.Cookie{}}
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
	db, err := store.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	ck := &clock{t: time.Unix(1_800_000_000, 0)}
	return &client{t: t, h: NewHandler(Options{Store: store.New(db), Now: ck.now}), cookies: map[string]string{}}, ck
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
	if r := c.do(http.MethodGet, "/nope", ""); r.status != http.StatusNotFound {
		t.Fatalf("unknown path = %d", r.status)
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

// The route table and the OpenAPI contract are two lists of the same thing; keep them equal.
func TestRoutesMatchOpenAPI(t *testing.T) {
	spec, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var inSpec []string
	path := ""
	for _, line := range strings.Split(string(spec), "\n") {
		if m := regexp.MustCompile(`^  (/api/\S+):$`).FindStringSubmatch(line); m != nil {
			path = m[1]
		} else if m := regexp.MustCompile(`^    (get|post|put|patch|delete):$`).FindStringSubmatch(line); m != nil && path != "" {
			inSpec = append(inSpec, strings.ToUpper(m[1])+" "+path)
		} else if !strings.HasPrefix(line, "    ") && line != "" && !strings.HasPrefix(line, "  /") {
			path = ""
		}
	}
	var served []string
	for _, r := range (&server{}).routes() {
		served = append(served, r.method+" "+r.path)
	}
	sort.Strings(inSpec)
	sort.Strings(served)
	if strings.Join(inSpec, "\n") != strings.Join(served, "\n") {
		t.Fatalf("api/openapi.yaml and the route table differ\nspec:\n%s\nserved:\n%s", strings.Join(inSpec, "\n"), strings.Join(served, "\n"))
	}
}
