package daemon

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/config"
	"github.com/TurtleByte-IN/mailrules/internal/mail/imap/imaptest"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// fakeOAuth is a module that connects Gmail and Outlook mailboxes with one-click sign-in
// against the in-memory IMAP server. Its provider hands out a new access token at every
// login, and replaces some refresh tokens with new ones as it does.
type fakeOAuth struct {
	srv *imaptest.Server

	mu      sync.Mutex
	issued  int
	given   []string          // the secret each Login was handed, in order
	rotate  map[string]string // refresh token -> the one that replaces it at its next use
	revoked map[string]bool
}

func (f *fakeOAuth) login(_ context.Context, provider, secret string) (ext.MailToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.given = append(f.given, secret)
	if provider != "gmail" {
		return ext.MailToken{}, fmt.Errorf("unexpected provider %q", provider)
	}
	if f.revoked[secret] {
		return ext.MailToken{}, fmt.Errorf("invalid_grant: %w", ext.ErrReconnect)
	}
	f.issued++
	tok := ext.MailToken{AccessToken: "access-" + strconv.Itoa(f.issued), Secret: f.rotate[secret]}
	f.srv.AllowToken(tok.AccessToken)
	return tok, nil
}

func (f *fakeOAuth) secrets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.given)
}

// module is the module the daemon is built with. Its Connect route stands in for the
// provider's consent screen and callback: it adds the mailbox, or with account=<id> gives it
// a new refresh token.
func (f *fakeOAuth) module() ext.Module {
	return ext.Module{
		Name: "oauthtest",
		Routes: func(h ext.Host) []ext.Route {
			return []ext.Route{{Method: http.MethodGet, Path: "/api/oauthtest/connect", Handler: func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if id, err := strconv.ParseInt(q.Get("account"), 10, 64); err == nil {
					if err := h.ReconnectMailbox(r.Context(), id, strings.ToUpper(imaptest.Username), "refresh-2"); err != nil {
						h.MailboxFailed(w, r, ext.MailboxConnectFailed)
						return
					}
					h.MailboxReconnected(w, r, id)
					return
				}
				id, err := h.AddMailbox(r.Context(), ext.NewMailbox{Provider: q.Get("provider"), Address: imaptest.Username,
					Host: f.srv.Host, Port: f.srv.Port, Secret: "refresh-1"})
				switch {
				case err == nil:
					h.MailboxAdded(w, r, id)
				case errors.Is(err, ext.ErrMailboxExists):
					h.MailboxFailed(w, r, ext.MailboxExists)
				default:
					h.MailboxFailed(w, r, ext.MailboxConnectFailed)
				}
			}}}
		},
		Mailboxes: &ext.MailboxSignIn{Providers: []string{"gmail", "outlook"}, Connect: "/api/oauthtest/connect", Login: f.login},
	}
}

// client is a signed-in browser of the daemon: it keeps the cookies, echoes the CSRF token
// and does not follow redirects, so a test sees where it is sent.
type client struct {
	t    *testing.T
	base string
	jar  *cookiejar.Jar
	http *http.Client
}

func (c *client) do(method, path string, body any, want int) (http.Header, []byte) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(c.t.Context(), method, c.base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	u, _ := url.Parse(c.base)
	for _, ck := range c.jar.Cookies(u) {
		if ck.Name == "mailrules_csrf" {
			req.Header.Set("X-CSRF-Token", ck.Value)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		c.t.Fatalf("%s %s = %d, want %d: %s", method, path, resp.StatusCode, want, out)
	}
	return resp.Header, out
}

type accountView struct {
	ID       int64  `json:"id"`
	Status   string `json:"status"`
	OneClick bool   `json:"one_click"`
	Preset   string `json:"preset"`
}

func (c *client) account(id int64) accountView {
	c.t.Helper()
	_, body := c.do(http.MethodGet, "/api/accounts/"+strconv.FormatInt(id, 10), nil, http.StatusOK)
	var out struct{ Account accountView }
	if err := json.Unmarshal(body, &out); err != nil {
		c.t.Fatal(err)
	}
	return out.Account
}

// eventually waits until ok holds, failing with what last stood in its way.
func eventually(t *testing.T, what string, ok func() (bool, string)) {
	t.Helper()
	var last string
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		var done bool
		if done, last = ok(); done {
			return
		}
	}
	t.Fatalf("%s: never happened; last %s", what, last)
}

func count(t *testing.T, srv *imaptest.Server, folder string) uint32 {
	t.Helper()
	st, err := srv.User.Status(folder, &imap.StatusOptions{NumMessages: true})
	if err != nil {
		t.Fatal(err)
	}
	return *st.NumMessages
}

func receipt(n int) string {
	return fmt.Sprintf("From: Shop <orders@shop.example>\r\nTo: %s\r\nSubject: Receipt %d\r\nMessage-ID: <r%d@shop.example>\r\nDate: Mon, 05 Oct 2026 10:00:00 +0000\r\n\r\nThanks for your order.\r\n",
		imaptest.Username, n, n)
}

// running is a daemon serveIMAP runs for a test, with a browser signed in to it.
type running struct {
	c        *client
	cfg      *config.Config
	dir      string // the data directory
	shutdown func() // stops the daemon; later calls do nothing
}

// serveForTest runs the daemon built with modules, trusting imapTLS for IMAP (nil: the
// system's roots), and signs a browser in through first-run setup.
func serveForTest(t *testing.T, imapTLS *tls.Config, modules ...ext.Module) running {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	dir := t.TempDir()
	master := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	env := map[string]string{"MAILRULES_DATA_DIR": dir, "MAILRULES_LISTEN": addr, "LOG_LEVEL": "error", "MAILRULES_MASTER_KEY": master}
	cfg, err := config.Load(nil, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- serveIMAP(ctx, cfg, "test", modules, imapTLS) }()
	stopped := false
	shutdown := func() {
		if !stopped {
			stopped = true
			stop()
			if err := <-done; err != nil {
				t.Errorf("serve: %v", err)
			}
		}
	}
	t.Cleanup(shutdown)

	jar, _ := cookiejar.New(nil)
	c := &client{t: t, base: "http://" + addr, jar: jar, http: &http.Client{Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	eventually(t, "the daemon starts", func() (bool, string) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/healthz", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false, err.Error()
		}
		_ = resp.Body.Close()
		return true, ""
	})
	c.do(http.MethodGet, "/api/auth/me", nil, http.StatusUnauthorized) // hands out the CSRF cookie
	c.do(http.MethodPost, "/api/auth/setup", map[string]string{"email": "admin@example.test", "password": "correct horse battery"}, http.StatusCreated)
	return running{c: c, cfg: cfg, dir: dir, shutdown: shutdown}
}

// storedSecret is the secret the stopped daemon of r stored for a mailbox.
func (r running) storedSecret(t *testing.T, id int64) string {
	t.Helper()
	r.shutdown()
	db, err := store.Open(t.Context(), r.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := store.New(db)
	key, err := openMasterKey(t.Context(), r.cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.AccountSecret(t.Context(), key, id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A module connects a Gmail mailbox with one-click sign-in, and the daemon does the rest
// over IMAP with XOAUTH2: it sorts mail, comes back with a fresh token when the provider
// ends the session, stores the refresh tokens the provider rotates, stops in
// reconnect_needed when access is revoked, and resumes once the person signs in again.
func TestOneClickMailboxEndToEnd(t *testing.T) {
	srv := imaptest.Start(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapMove: {}, imap.CapUIDPlus: {}})
	if err := srv.User.Create("Receipts", nil); err != nil {
		t.Fatal(err)
	}
	fake := &fakeOAuth{srv: srv, rotate: map[string]string{"refresh-1": "refresh-1b", "refresh-1b": "refresh-1c"}, revoked: map[string]bool{}}

	d := serveForTest(t, srv.TLS, fake.module())
	c := d.c

	// The wizard offers one-click Gmail and Outlook through the module.
	_, body := c.do(http.MethodGet, "/api/presets", nil, http.StatusOK)
	var presetList struct {
		Items []struct {
			Name        string  `json:"name"`
			Password    bool    `json:"password"`
			OneClickURL *string `json:"one_click_url"`
		}
	}
	if err := json.Unmarshal(body, &presetList); err != nil {
		t.Fatal(err)
	}
	oneClick := map[string]string{}
	for _, p := range presetList.Items {
		if p.OneClickURL != nil {
			oneClick[p.Name] = *p.OneClickURL
		}
		if p.Name == "outlook" && p.Password {
			t.Error("Outlook is offered with a password")
		}
	}
	if oneClick["gmail"] != "/api/oauthtest/connect?provider=gmail" || oneClick["outlook"] != "/api/oauthtest/connect?provider=outlook" {
		t.Fatalf("one-click tiles = %v", oneClick)
	}
	if _, body := c.do(http.MethodGet, "/api/settings", nil, http.StatusOK); !strings.Contains(string(body), `"oauth_providers":true`) {
		t.Errorf("features.oauth_providers is not on: %s", body)
	}

	// The tile sends the browser through the module, which adds the mailbox and sends it on
	// to the wizard's Rules step.
	hdr, _ := c.do(http.MethodGet, oneClick["gmail"], nil, http.StatusSeeOther)
	loc := hdr.Get("Location")
	idText, ok := strings.CutPrefix(loc, "/#/accounts?added=")
	if !ok {
		t.Fatalf("after adding, the browser goes to %q", loc)
	}
	id, _ := strconv.ParseInt(idText, 10, 64)
	if a := c.account(id); !a.OneClick || a.Preset != "gmail" {
		t.Fatalf("account = %+v, want a one-click Gmail mailbox", a)
	}
	eventually(t, "the mailbox goes live", func() (bool, string) { a := c.account(id); return a.Status == "live", a.Status })
	if hdr, _ := c.do(http.MethodGet, oneClick["gmail"], nil, http.StatusSeeOther); hdr.Get("Location") != "/#/accounts?mailbox_error=exists" {
		t.Errorf("adding it again goes to %q", hdr.Get("Location"))
	}

	// The provider rotated the refresh token at the first login (when it was added) and
	// again at the next; each new one was stored before it was used, and the old ones are
	// never handed out again.
	eventually(t, "the second rotation is used", func() (bool, string) {
		got := fake.secrets()
		return slices.Contains(got, "refresh-1c"), strings.Join(got, ",")
	})
	uses := func(got []string, secret string) int {
		n := 0
		for _, s := range got {
			if s == secret {
				n++
			}
		}
		return n
	}
	if got := fake.secrets(); got[0] != "refresh-1" || uses(got, "refresh-1") != 1 || uses(got, "refresh-1b") != 1 {
		t.Errorf("secrets handed to the module = %v: a rotated one was used twice", got)
	}

	// A rule sorts new mail, with dry-run off.
	c.do(http.MethodPatch, "/api/settings", map[string]any{"dry_run": false}, http.StatusOK)
	c.do(http.MethodPost, "/api/rules/batch", map[string]any{"rules": []map[string]any{{"name": "Receipts",
		"conditions": map[string]any{"field": "subject", "op": "contains_any", "value": []string{"Receipt"}},
		"actions":    []map[string]any{{"type": "move", "folder": "Receipts"}}}}}, http.StatusCreated)
	srv.Append(t, "INBOX", receipt(1))
	eventually(t, "the first receipt is sorted", func() (bool, string) {
		n := count(t, srv, "Receipts")
		return n == 1, fmt.Sprint(n)
	})

	// The provider ends every session an hour in, and the tokens with it. That is a normal
	// reconnect with a fresh token: the status never leaves live, and mail goes on being
	// sorted.
	logins := len(srv.TokenLogins())
	srv.ExpireSessions()
	srv.Append(t, "INBOX", receipt(2))
	eventually(t, "the second receipt is sorted after the session expired", func() (bool, string) {
		n := count(t, srv, "Receipts")
		return n == 2, fmt.Sprint(n)
	})
	if a := c.account(id); a.Status != "live" {
		t.Errorf("status after the provider closed the session = %s, want live", a.Status)
	}
	if got := srv.TokenLogins(); len(got) <= logins {
		t.Errorf("no new login after the session expired: %v", got)
	}

	// The person revokes MailRules' access: the mailbox stops in reconnect_needed and mail
	// stays where it is.
	fake.mu.Lock()
	fake.revoked["refresh-1c"] = true
	fake.mu.Unlock()
	srv.ExpireSessions()
	eventually(t, "the mailbox needs reconnecting", func() (bool, string) {
		a := c.account(id)
		return a.Status == "reconnect_needed", a.Status
	})
	srv.Append(t, "INBOX", receipt(3))
	time.Sleep(300 * time.Millisecond)
	if n := count(t, srv, "Receipts"); n != 2 {
		t.Errorf("Receipts holds %d while the mailbox needs reconnecting, want 2", n)
	}

	// Reconnect: the person signs in again, the module hands over the new refresh token,
	// and the mail that waited is sorted.
	hdr, _ = c.do(http.MethodGet, oneClick["gmail"]+"&account="+idText, nil, http.StatusSeeOther)
	if loc := hdr.Get("Location"); loc != "/#/accounts?reconnected="+idText {
		t.Fatalf("after reconnecting, the browser goes to %q", loc)
	}
	eventually(t, "the waiting receipt is sorted after reconnecting", func() (bool, string) {
		a := c.account(id)
		n := count(t, srv, "Receipts")
		return a.Status == "live" && n == 3, fmt.Sprint(a.Status, " ", n)
	})

	// The secret is stored encrypted and never in an answer of the API.
	_, body = c.do(http.MethodGet, "/api/accounts", nil, http.StatusOK)
	if strings.Contains(string(body), "refresh-") {
		t.Errorf("the API shows the secret: %s", body)
	}
	if got := d.storedSecret(t, id); got != "refresh-2" {
		t.Errorf("stored secret = %q; want the one the reconnect handed over", got)
	}
}
