package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// presets lists GET /api/presets by name, each checked against the contract.
func (e *env) presets() map[string]map[string]any {
	e.t.Helper()
	out := map[string]map[string]any{}
	for _, it := range e.call(http.MethodGet, "/api/presets", "", http.StatusOK)["items"].([]any) {
		p := it.(map[string]any)
		conform(e.t, e.doc, "Preset", p)
		out[p["name"].(string)] = p
	}
	return out
}

// The free build has no one-click sign-in: no Outlook tile, the Gmail tile takes an app
// password, and Outlook cannot be connected with a password.
func TestFreeBuildHasNoOneClick(t *testing.T) {
	e := newEnv(t)
	e.signIn()
	ps := e.presets()
	if _, ok := ps["outlook"]; ok {
		t.Error("the free build offers Outlook")
	}
	if g := ps["gmail"]; g["one_click_url"] != nil || g["password"] != true || g["secret_label"] != "App password" {
		t.Errorf("gmail preset = %v, want the app-password one", g)
	}
	if on := e.call(http.MethodGet, "/api/settings", "", http.StatusOK)["features"].(map[string]any)["oauth_providers"]; on != false {
		t.Errorf("features.oauth_providers = %v without a module", on)
	}
	e.refuse(http.MethodPost, "/api/accounts/test", `{"preset":"outlook","username":"me@outlook.com","password":"pw"}`,
		http.StatusBadRequest, "invalid_input", "preset")
	e.refuse(http.MethodPost, "/api/accounts", `{"preset":"outlook","username":"me@outlook.com","password":"pw"}`,
		http.StatusBadRequest, "invalid_input", "preset")
}

// oneClickModule connects mailboxes through the host the way a real module's Connect route
// does, answering what the host said as JSON when it is not a redirect.
func oneClickModule() ext.Module {
	return ext.Module{
		Name: "oneclick",
		Routes: func(h ext.Host) []ext.Route {
			return []ext.Route{{Method: http.MethodGet, Path: "/api/oneclick/connect", Handler: func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if acct := q.Get("account"); acct != "" {
					id, _ := strconv.ParseInt(acct, 10, 64)
					if err := h.ReconnectMailbox(r.Context(), id, q.Get("address"), q.Get("secret")); err != nil {
						h.WriteJSON(w, http.StatusOK, map[string]string{"failed": err.Error()})
						return
					}
					h.MailboxReconnected(w, r, id)
					return
				}
				id, err := h.AddMailbox(r.Context(), ext.NewMailbox{Provider: q.Get("provider"), Address: q.Get("address"), Secret: q.Get("secret")})
				switch {
				case errors.Is(err, ext.ErrMailboxExists):
					h.MailboxFailed(w, r, ext.MailboxExists)
				case err != nil:
					h.WriteJSON(w, http.StatusOK, map[string]string{"failed": err.Error()})
				default:
					h.MailboxAdded(w, r, id)
				}
			}}}
		},
		Mailboxes: &ext.MailboxSignIn{Providers: []string{"gmail", "outlook"}, Connect: "/api/oneclick/connect",
			Login: func(context.Context, string, string) (ext.MailToken, error) { return ext.MailToken{}, nil }},
	}
}

func TestOneClickMailboxes(t *testing.T) {
	var (
		oauthErr error
		secrets  []string // what ConnectOAuth was given: the secret, or "stored"
	)
	var e *env
	e = newEnvWith(t, func(o *Options) {
		o.ConnectOAuth = func(_ context.Context, acct store.Account, secret *string) (mail.Mailbox, error) {
			if secret == nil {
				secrets = append(secrets, "stored")
			} else {
				secrets = append(secrets, *secret)
				*secret += "-rotated" // the provider replaced the refresh token at this login
			}
			if oauthErr != nil {
				return nil, oauthErr
			}
			return e.box(acct), nil
		}
	}, oneClickModule())
	e.signIn()

	ps := e.presets()
	if g, o := ps["gmail"], ps["outlook"]; g["one_click_url"] != "/api/oneclick/connect?provider=gmail" || g["password"] != true ||
		o["one_click_url"] != "/api/oneclick/connect?provider=outlook" || o["password"] != false {
		t.Fatalf("presets: gmail %v, outlook %v", g, o)
	}
	if on := e.call(http.MethodGet, "/api/settings", "", http.StatusOK)["features"].(map[string]any)["oauth_providers"]; on != true {
		t.Errorf("features.oauth_providers = %v with the module", on)
	}

	// A password mailbox first, then a one-click Outlook one.
	e.call(http.MethodPost, "/api/accounts", accountBody, http.StatusCreated)
	r := e.do(http.MethodGet, "/api/oneclick/connect?provider=outlook&address=jo@outlook.test&secret=refresh-1", "")
	if r.status != http.StatusSeeOther || r.header.Get("Location") != "/#/accounts?added=2" {
		t.Fatalf("add = %d %s %s", r.status, r.header.Get("Location"), r.raw)
	}
	a, err := e.st.Account(t.Context(), 2)
	if err != nil || !a.OAuth || a.Preset != "outlook" || a.Host != "outlook.office365.com" || a.Port != 993 || a.Username != "jo@outlook.test" {
		t.Fatalf("stored account = %+v, %v", a, err)
	}
	if got, _ := e.st.AccountSecret(t.Context(), e.sett.Master, 2); got != "refresh-1-rotated" {
		t.Errorf("stored secret = %q, want the one the provider rotated to", got)
	}
	view := e.call(http.MethodGet, "/api/accounts/2", "", http.StatusOK)["account"].(map[string]any)
	conform(t, e.doc, "Account", view)
	if view["one_click"] != true || strings.Contains(fmt.Sprint(view), "refresh") {
		t.Errorf("account = %v", view)
	}
	if r := e.do(http.MethodGet, "/api/oneclick/connect?provider=outlook&address=JO@outlook.test&secret=x", ""); r.header.Get("Location") != "/#/accounts?mailbox_error=exists" {
		t.Errorf("adding it again = %d %s", r.status, r.header.Get("Location"))
	}
	if r := e.do(http.MethodGet, "/api/oneclick/connect?provider=yahoo&address=jo@yahoo.test&secret=x", ""); !strings.Contains(string(r.raw), "not one of the providers") {
		t.Errorf("a provider the module does not sign in to = %s", r.raw)
	}

	// Its sign-in and server are the module's; the rest is edited as for any mailbox.
	for _, f := range []string{`"password":"pw"`, `"host":"imap.example.test"`, `"port":143`, `"tls_mode":"starttls"`, `"cert_fingerprint":""`} {
		e.refuse(http.MethodPatch, "/api/accounts/2", "{"+f+"}", http.StatusBadRequest, "invalid_input", strings.Trim(strings.Split(f, ":")[0], `"`))
	}
	e.call(http.MethodPatch, "/api/accounts/2", `{"label":"Work"}`, http.StatusOK)

	// The stored test signs in through the module with the stored secret.
	secrets = nil
	e.call(http.MethodPost, "/api/accounts/2/test", "", http.StatusOK)
	oauthErr = fmt.Errorf("invalid_grant: %w", mail.ErrReconnect)
	e.refuse(http.MethodPost, "/api/accounts/2/test", "", http.StatusUnprocessableEntity, "reconnect_needed", "")
	if !slices.Equal(secrets, []string{"stored", "stored"}) {
		t.Errorf("stored tests used %v", secrets)
	}

	// Reconnecting: only the owner's one-click mailbox, only as its own address, and only
	// with a secret that works.
	for _, c := range []struct{ query, want string }{
		{"account=1&address=me@example.test&secret=s", ext.ErrNoMailbox.Error()},
		{"account=9&address=jo@outlook.test&secret=s", ext.ErrNoMailbox.Error()},
		{"account=2&address=someone@outlook.test&secret=s", ext.ErrMailboxMismatch.Error()},
		{"account=2&address=jo@outlook.test&secret=refresh-2", "invalid_grant"},
	} {
		if r := e.do(http.MethodGet, "/api/oneclick/connect?"+c.query, ""); !strings.Contains(string(r.raw), c.want) {
			t.Errorf("reconnect %s = %d %s, want %q", c.query, r.status, r.raw, c.want)
		}
	}
	if got, _ := e.st.AccountSecret(t.Context(), e.sett.Master, 2); got != "refresh-1-rotated" {
		t.Errorf("a failed reconnect changed the secret to %q", got)
	}
	oauthErr = nil
	if err := e.st.SetAccountStatus(t.Context(), 2, "reconnect_needed", "revoked", 1); err != nil {
		t.Fatal(err)
	}
	r = e.do(http.MethodGet, "/api/oneclick/connect?account=2&address=jo@outlook.test&secret=refresh-2", "")
	if r.status != http.StatusSeeOther || r.header.Get("Location") != "/#/accounts?reconnected=2" {
		t.Fatalf("reconnect = %d %s %s", r.status, r.header.Get("Location"), r.raw)
	}
	if got, _ := e.st.AccountSecret(t.Context(), e.sett.Master, 2); got != "refresh-2-rotated" {
		t.Errorf("secret after reconnecting = %q", got)
	}
	eventually(t, "the reconnected mailbox to be live", func() bool {
		a, err := e.st.Account(t.Context(), 2)
		return err == nil && a.Status == "live"
	})
}

func TestCheckModulesOneClick(t *testing.T) {
	ok := func(http.ResponseWriter, *http.Request) {}
	login := func(context.Context, string, string) (ext.MailToken, error) { return ext.MailToken{}, nil }
	connect := func(public bool) func(ext.Host) []ext.Route {
		return func(ext.Host) []ext.Route {
			return []ext.Route{{Method: http.MethodGet, Path: "/api/o/connect", Public: public, Handler: ok}}
		}
	}
	mod := func(name string, p []string, path string, l func(context.Context, string, string) (ext.MailToken, error), public bool) ext.Module {
		return ext.Module{Name: name, Routes: connect(public), Mailboxes: &ext.MailboxSignIn{Providers: p, Connect: path, Login: l}}
	}
	for _, c := range []struct {
		name    string
		modules []ext.Module
		want    string
	}{
		{"gmail and outlook", []ext.Module{mod("o", []string{"gmail", "outlook"}, "/api/o/connect", login, false)}, ""},
		{"no login", []ext.Module{mod("o", []string{"gmail"}, "/api/o/connect", nil, false)}, "needs Login"},
		{"no provider", []ext.Module{mod("o", nil, "/api/o/connect", login, false)}, "needs Login and at least one provider"},
		{"a provider without XOAUTH2", []ext.Module{mod("o", []string{"icloud"}, "/api/o/connect", login, false)}, `provider "icloud"`},
		{"Connect names no route", []ext.Module{mod("o", []string{"gmail"}, "/api/o/nope", login, false)}, `Connect "/api/o/nope"`},
		{"Connect is public", []ext.Module{mod("o", []string{"gmail"}, "/api/o/connect", login, true)}, "signed-in user"},
		{"two modules", []ext.Module{mod("a", []string{"gmail"}, "/api/o/connect", login, false), mod("b", []string{"outlook"}, "/api/o/connect", login, false)},
			"more than one module connects mailboxes"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := CheckModules("selfhost", c.modules)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("err = %v, want one saying %q", err, c.want)
			}
		})
	}
}
