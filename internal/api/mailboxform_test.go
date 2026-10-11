package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/worker"
)

// What the fake Bridge of formModule knows. Each word is distinct, so a test can look for
// it in what the daemon stored, logged or answered.
const (
	formProtonPassword  = "proton-account-pw-31"   // #nosec G101 -- a test value
	formCode            = "424242"                 // the two-factor code
	formMailboxPassword = "proton-mailbox-pw-77"   // #nosec G101 -- a test value
	formBridgePassword  = "bridge-made-imap-pw-55" // #nosec G101 -- what Bridge gives mail apps
	formBridgePassword2 = "bridge-made-imap-pw-66" // #nosec G101 -- what Bridge gives after signing in again
	formFingerprint     = "AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89"
)

// formModule answers a Proton sign-in form the way the cloud module does with the
// operator's Bridge: free@ is on the free plan, which Bridge refuses; two@ has two-factor
// sign-in and a separate mailbox password; every other address signs in with the password
// alone. Posted with account_id (Reconnect), it gives that mailbox Bridge's new password.
func formModule() ext.Module {
	return ext.Module{
		Name: "proton_cloud",
		Routes: func(h ext.Host) []ext.Route {
			return []ext.Route{{Method: http.MethodPost, Path: "/api/proton/sign-in", Handler: func(w http.ResponseWriter, r *http.Request) {
				var in ext.MailboxFormInput
				if !h.ReadJSON(w, r, &in) {
					return
				}
				answer := func(s ext.MailboxFormStep) { h.WriteJSON(w, http.StatusOK, s) }
				add := func(address string) {
					if in.AccountID != 0 {
						if err := h.ReconnectMailbox(r.Context(), in.AccountID, address, formBridgePassword2); err != nil {
							answer(ext.MailboxFormStep{Step: ext.MailboxStepSignIn, Message: "failed: " + err.Error()})
							return
						}
						answer(ext.MailboxFormStep{AccountID: in.AccountID})
						return
					}
					id, err := h.AddMailbox(r.Context(), ext.NewMailbox{Provider: "proton", Address: address, Secret: formBridgePassword,
						Password: true, TLSMode: "starttls", CertFingerprint: formFingerprint})
					switch {
					case errors.Is(err, ext.ErrMailboxExists):
						answer(ext.MailboxFormStep{Step: ext.MailboxStepSignIn, Message: "This mailbox is already connected."})
					case err != nil:
						answer(ext.MailboxFormStep{Step: ext.MailboxStepSignIn, Message: "failed: " + err.Error()})
					default:
						answer(ext.MailboxFormStep{AccountID: id})
					}
				}
				switch in.Step {
				case ext.MailboxStepSignIn:
					switch {
					case in.Password != formProtonPassword:
						answer(ext.MailboxFormStep{Step: ext.MailboxStepSignIn, Message: "Proton refused that password."})
					case strings.HasPrefix(in.Username, "free@"):
						answer(ext.MailboxFormStep{Step: ext.MailboxStepSignIn, Message: "Bridge needs a paid Proton plan."})
					case strings.HasPrefix(in.Username, "two@"):
						answer(ext.MailboxFormStep{Step: ext.MailboxStepCode, State: "s:" + in.Username})
					default:
						add(in.Username)
					}
				case ext.MailboxStepCode:
					if in.Code != formCode {
						answer(ext.MailboxFormStep{Step: ext.MailboxStepCode, State: in.State, Message: "That code is wrong."})
						return
					}
					answer(ext.MailboxFormStep{Step: ext.MailboxStepMailboxPassword, State: in.State})
				case ext.MailboxStepMailboxPassword:
					if in.Password != formMailboxPassword {
						answer(ext.MailboxFormStep{Step: ext.MailboxStepMailboxPassword, State: in.State, Message: "That mailbox password is wrong."})
						return
					}
					add(strings.TrimPrefix(in.State, "s:"))
				default:
					h.Invalid(w, "step", "unknown step")
				}
			}}}
		},
		MailboxForms: []ext.MailboxForm{{Provider: "proton", Path: "/api/proton/sign-in"}},
	}
}

// The free build offers Proton only through a Bridge of the person's own, with its
// password; the form tile needs the module.
func TestFreeBuildHasNoProtonForm(t *testing.T) {
	e := newEnv(t)
	e.signIn()
	if p := e.presets()["proton"]; p["form_url"] != nil || p["password"] != true {
		t.Errorf("proton preset = %v, want the Bridge-password one", p)
	}
}

func TestMailboxForm(t *testing.T) {
	l := captureLogs(t, slog.LevelDebug)
	var passwords []string // what each IMAP login was given
	var e *env
	e = newEnvWith(t, func(o *Options) {
		o.Connect = func(_ context.Context, acct store.Account, password string) (mail.Mailbox, string, error) {
			e.dialed, passwords = acct, append(passwords, password)
			if e.connectErr != nil {
				return nil, "", e.connectErr
			}
			return e.box(acct), acct.Username, nil
		}
	}, formModule())
	e.signIn()

	// Only the form's tile: no Bridge-password tile beside it.
	p := e.presets()["proton"]
	if p["form_url"] != "/api/proton/sign-in" || p["password"] != false || p["one_click_url"] != nil {
		t.Fatalf("proton preset = %v", p)
	}
	if g := e.presets()["generic"]; g["form_url"] != nil || g["password"] != true {
		t.Errorf("generic preset = %v", g)
	}

	// post sends one step as the web app does, both sides checked against the contract.
	post := func(in ext.MailboxFormInput) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(in)
		var sent map[string]any
		_ = json.Unmarshal(raw, &sent)
		conform(t, e.doc, "MailboxFormInput", sent)
		out := e.call(http.MethodPost, "/api/proton/sign-in", string(raw), http.StatusOK)
		conform(t, e.doc, "MailboxFormStep", out)
		return out
	}

	// A wrong password and the free plan come back as sentences, on the same step.
	if a := post(ext.MailboxFormInput{Step: "sign_in", Username: "two@proton.test", Password: "nope"}); a["step"] != "sign_in" || a["message"] != "Proton refused that password." {
		t.Errorf("wrong password = %v", a)
	}
	if a := post(ext.MailboxFormInput{Step: "sign_in", Username: "free@proton.test", Password: formProtonPassword}); a["step"] != "sign_in" || a["message"] != "Bridge needs a paid Proton plan." {
		t.Errorf("free plan = %v", a)
	}

	// Two-factor sign-in, a wrong code retried, then the mailbox password.
	a := post(ext.MailboxFormInput{Step: "sign_in", Username: "two@proton.test", Password: formProtonPassword})
	if a["step"] != "code" || a["state"] == "" {
		t.Fatalf("after the password = %v", a)
	}
	state := a["state"].(string)
	if a := post(ext.MailboxFormInput{Step: "code", State: state, Code: "000000"}); a["step"] != "code" || a["message"] != "That code is wrong." {
		t.Errorf("wrong code = %v", a)
	}
	if a := post(ext.MailboxFormInput{Step: "code", State: state, Code: formCode}); a["step"] != "mailbox_password" {
		t.Fatalf("after the code = %v", a)
	}
	a = post(ext.MailboxFormInput{Step: "mailbox_password", State: state, Password: formMailboxPassword})
	if a["account_id"] != float64(1) || a["step"] != nil {
		t.Fatalf("after the mailbox password = %v", a)
	}

	// The mailbox is a password mailbox on Bridge, over STARTTLS, pinned to Bridge's
	// certificate, and its stored secret is the Bridge password and nothing the person typed.
	acct, err := e.st.Account(t.Context(), 1)
	if err != nil || acct.OAuth || acct.Preset != "proton" || acct.Host != "127.0.0.1" || acct.Port != 1143 || acct.TLSMode != "starttls" ||
		acct.Username != "two@proton.test" || acct.CertFingerprint != formFingerprint {
		t.Fatalf("stored account = %+v, %v", acct, err)
	}
	if e.dialed.CertFingerprint != acct.CertFingerprint || e.dialed.TLSMode != "starttls" {
		t.Errorf("the first login dialed %+v", e.dialed)
	}
	if got, _ := e.st.AccountSecret(t.Context(), e.sett.Master, 1); got != formBridgePassword {
		t.Errorf("stored secret = %q, want the Bridge password", got)
	}
	if len(passwords) != 1 || passwords[0] != formBridgePassword {
		t.Errorf("IMAP logins used %q", passwords)
	}
	view := e.call(http.MethodGet, "/api/accounts/1", "", http.StatusOK)["account"].(map[string]any)
	conform(t, e.doc, "Account", view)
	if view["one_click"] != false {
		t.Errorf("account = %v", view)
	}
	e.live()

	// Adding it again is refused by the daemon and said by the module.
	post(ext.MailboxFormInput{Step: "sign_in", Username: "two@proton.test", Password: formProtonPassword})
	post(ext.MailboxFormInput{Step: "code", State: state, Code: formCode})
	if a := post(ext.MailboxFormInput{Step: "mailbox_password", State: state, Password: formMailboxPassword}); a["message"] != "This mailbox is already connected." {
		t.Errorf("adding it again = %v", a)
	}

	// A login Bridge refuses stores nothing.
	e.connectErr = fmt.Errorf("login: %w", mail.ErrAuth)
	if a := post(ext.MailboxFormInput{Step: "sign_in", Username: "one@proton.test", Password: formProtonPassword}); !strings.Contains(fmt.Sprint(a["message"]), "failed") {
		t.Errorf("a refused login = %v", a)
	}
	e.connectErr = nil
	if list := e.call(http.MethodGet, "/api/accounts", "", http.StatusOK)["items"].([]any); len(list) != 1 {
		t.Errorf("accounts after a refused login = %d", len(list))
	}

	// Nothing the person typed reaches the logs, the database or the API; nor does the
	// Bridge password.
	var dump strings.Builder
	rows, err := e.db.QueryContext(t.Context(), "SELECT * FROM accounts")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&dump, "%s\n", vals)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	logged := l.buf.String()
	l.mu.Unlock()
	for _, secret := range []string{formProtonPassword, formCode, formMailboxPassword, formBridgePassword} {
		if strings.Contains(logged, secret) {
			t.Errorf("the logs hold %q", secret)
		}
		if strings.Contains(dump.String(), secret) {
			t.Errorf("the accounts table holds %q in the clear", secret)
		}
		if strings.Contains(fmt.Sprint(view), secret) {
			t.Errorf("the account view holds %q", secret)
		}
	}
}

// A mailbox a module's form added logs in with Bridge's password, which the person never
// saw: Edit offers neither it nor the server, a refused login asks them to reconnect, and
// Reconnect opens the form again with the mailbox's id. The module then gives that mailbox,
// and only that one of its owner, as its own address, the new password, which the daemon
// tries, stores encrypted in place of the old one, and starts the same mailbox with.
func TestReconnectFormMailbox(t *testing.T) {
	var passwords []string // what each IMAP login was given
	var e *env
	e = newEnvWith(t, func(o *Options) {
		o.Connect = func(_ context.Context, acct store.Account, password string) (mail.Mailbox, string, error) {
			e.dialed, passwords = acct, append(passwords, password)
			if e.connectErr != nil {
				return nil, "", e.connectErr
			}
			return e.box(acct), acct.Username, nil
		}
	}, formModule(), signInModule())
	e.signInAs("owner", "owner@corp.test", "org-1")
	post := func(b *env, in ext.MailboxFormInput) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(in)
		var sent map[string]any
		_ = json.Unmarshal(raw, &sent)
		conform(t, e.doc, "MailboxFormInput", sent)
		out := b.call(http.MethodPost, "/api/proton/sign-in", string(raw), http.StatusOK)
		conform(t, e.doc, "MailboxFormStep", out)
		return out
	}
	if a := post(e, ext.MailboxFormInput{Step: "sign_in", Username: "jo@proton.test", Password: formProtonPassword}); a["account_id"] != float64(1) {
		t.Fatalf("add = %v", a)
	}
	e.live()
	before, _ := e.st.Account(t.Context(), 1)
	view := e.call(http.MethodGet, "/api/accounts/1", "", http.StatusOK)["account"].(map[string]any)
	conform(t, e.doc, "Account", view)
	if view["module"] != true || view["one_click"] != false {
		t.Errorf("account = %v, want a module's mailbox that is not one-click", view)
	}

	// Edit: the password and the server are the module's; the rest is the person's.
	for _, f := range []string{`"password":"pw"`, `"host":"imap.example.test"`, `"port":143`, `"tls_mode":"implicit"`} {
		e.refuse(http.MethodPatch, "/api/accounts/1", "{"+f+"}", http.StatusBadRequest, "invalid_input", strings.Trim(strings.Split(f, ":")[0], `"`))
	}
	e.call(http.MethodPatch, "/api/accounts/1", `{"label":"Jo at Proton","watch_folder":"INBOX"}`, http.StatusOK)
	if a, _ := e.st.Account(t.Context(), 1); a.Label != "Jo at Proton" || a.Host != before.Host || a.Port != before.Port || !a.Module {
		t.Errorf("after Edit = %+v", a)
	}
	if got, _ := e.st.AccountSecret(t.Context(), e.sett.Master, 1); got != formBridgePassword {
		t.Errorf("Edit changed the secret to %q", got)
	}

	// Bridge lost the account's session: its password is refused, and the person is sent
	// to Reconnect, not to a password they never had.
	e.connectErr = fmt.Errorf("login: %w", mail.ErrAuth)
	r := e.do(http.MethodPost, "/api/accounts/1/test", "")
	if r.status != http.StatusUnprocessableEntity || r.body.Error.Code != "reconnect_needed" || r.body.Error.Path != "" ||
		r.body.Error.Message != "Proton Mail no longer accepts MailRules' sign-in. Reconnect to sign in again." {
		t.Errorf("stored test with the old password = %d %s", r.status, r.raw)
	}
	if err := e.st.SetAccountStatus(t.Context(), 1, worker.StatusReconnectNeeded, "authentication failed", 1); err != nil {
		t.Fatal(err)
	}

	// Reconnecting is refused for a mailbox that does not exist, as another address, for
	// someone else's mailbox (another team's, or a teammate's even when shared), and while
	// the new password does not work; none of it changes the secret.
	other, mate := e.browser(), e.browser()
	other.signInAs("stranger", "stranger@else.test", "org-2")
	mate.signInAs("mate", "mate@corp.test", "org-1")
	e.call(http.MethodPatch, "/api/accounts/1", `{"shared":true}`, http.StatusOK)
	jo := ext.MailboxFormInput{Step: "sign_in", Username: "jo@proton.test", Password: formProtonPassword, AccountID: 1}
	for _, c := range []struct {
		name string
		b    *env
		in   ext.MailboxFormInput
		want string
	}{
		{"an unknown mailbox", e, ext.MailboxFormInput{Step: "sign_in", Username: "jo@proton.test", Password: formProtonPassword, AccountID: 9}, ext.ErrNoMailbox.Error()},
		{"another address", e, ext.MailboxFormInput{Step: "sign_in", Username: "someone@proton.test", Password: formProtonPassword, AccountID: 1}, ext.ErrMailboxMismatch.Error()},
		{"another team", other, jo, ext.ErrNoMailbox.Error()},
		{"a teammate", mate, jo, ext.ErrNoMailbox.Error()},
		{"a new password Bridge refuses", e, jo, "authentication failed"},
	} {
		if a := post(c.b, c.in); a["account_id"] != nil || !strings.Contains(fmt.Sprint(a["message"]), c.want) {
			t.Errorf("reconnect with %s = %v, want a message with %q", c.name, a, c.want)
		}
	}
	if got, _ := e.st.AccountSecret(t.Context(), e.sett.Master, 1); got != formBridgePassword {
		t.Errorf("a refused reconnect changed the secret to %q", got)
	}
	if a, _ := e.st.Account(t.Context(), 1); a.Status != worker.StatusReconnectNeeded {
		t.Errorf("a refused reconnect left the mailbox %s", a.Status)
	}

	// Through the form, as its own address: the new password is tried, then stored,
	// encrypted, and the same mailbox starts again.
	e.connectErr = nil
	passwords = nil
	logins := e.logins.Load()
	if a := post(e, jo); a["account_id"] != float64(1) || a["step"] != nil || a["message"] != nil {
		t.Fatalf("reconnect = %v", a)
	}
	if len(passwords) != 1 || passwords[0] != formBridgePassword2 {
		t.Errorf("the reconnect logged in with %q", passwords)
	}
	if got, _ := e.st.AccountSecret(t.Context(), e.sett.Master, 1); got != formBridgePassword2 {
		t.Errorf("secret after reconnecting = %q", got)
	}
	var sealed []byte
	if err := e.db.QueryRowContext(t.Context(), `SELECT secret_enc FROM accounts WHERE id = 1`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), formBridgePassword2) {
		t.Error("the new password is stored in the clear")
	}
	e.live()
	if e.logins.Load() == logins {
		t.Error("the mailbox was not started again")
	}
	after, _ := e.st.Account(t.Context(), 1)
	if after.ID != before.ID || after.CreatedAt != before.CreatedAt || after.Label != "Jo at Proton" || after.Host != before.Host || !after.Module || after.LastError != "" {
		t.Errorf("after reconnecting = %+v, want the same mailbox", after)
	}
	if list := e.call(http.MethodGet, "/api/accounts", "", http.StatusOK)["items"].([]any); len(list) != 1 {
		t.Errorf("accounts after reconnecting = %d", len(list))
	}
}

// A module adds a password mailbox only for a provider its form connects, with a TLS mode
// and a fingerprint the daemon can use.
func TestAddMailboxWithPassword(t *testing.T) {
	var got []error
	mod := formModule()
	mod.Routes = func(h ext.Host) []ext.Route {
		return []ext.Route{{Method: http.MethodPost, Path: "/api/proton/sign-in", Handler: func(w http.ResponseWriter, r *http.Request) {
			for _, m := range []ext.NewMailbox{
				{Provider: "gmail", Address: "me@gmail.test", Secret: "s", Password: true},
				{Provider: "proton", Address: "me@proton.test", Secret: "s"}, // not one-click
				{Provider: "proton", Address: "me@proton.test", Secret: "s", Password: true, TLSMode: "ssl"},
				{Provider: "proton", Address: "me@proton.test", Secret: "s", Password: true, CertFingerprint: "nope"},
				{Provider: "proton", Address: "", Secret: "s", Password: true},
			} {
				_, err := h.AddMailbox(r.Context(), m)
				got = append(got, err)
			}
			h.WriteJSON(w, http.StatusOK, ext.MailboxFormStep{})
		}}}
	}
	e := newEnv(t, mod)
	e.signIn()
	e.call(http.MethodPost, "/api/proton/sign-in", `{"step":"sign_in"}`, http.StatusOK)
	for i, want := range []string{"not one of the providers", "not one of the providers", "neither implicit nor starttls", "fingerprint", "an address and a secret"} {
		if got[i] == nil || !strings.Contains(got[i].Error(), want) {
			t.Errorf("case %d: err = %v, want one saying %q", i, got[i], want)
		}
	}
	if list := e.call(http.MethodGet, "/api/accounts", "", http.StatusOK)["items"].([]any); len(list) != 0 {
		t.Errorf("accounts = %v", list)
	}
}

func TestCheckModulesMailboxForm(t *testing.T) {
	ok := func(http.ResponseWriter, *http.Request) {}
	routes := func(method string, public bool) func(ext.Host) []ext.Route {
		return func(ext.Host) []ext.Route {
			return []ext.Route{{Method: method, Path: "/api/p/sign-in", Public: public, Handler: ok}}
		}
	}
	mod := func(name, provider, path, method string, public bool) ext.Module {
		return ext.Module{Name: name, Routes: routes(method, public), MailboxForms: []ext.MailboxForm{{Provider: provider, Path: path}}}
	}
	for _, c := range []struct {
		name    string
		modules []ext.Module
		want    string
	}{
		{"proton", []ext.Module{mod("p", "proton", "/api/p/sign-in", http.MethodPost, false)}, ""},
		{"another provider", []ext.Module{mod("p", "icloud", "/api/p/sign-in", http.MethodPost, false)}, `provider "icloud"`},
		{"Path names no route", []ext.Module{mod("p", "proton", "/api/p/nope", http.MethodPost, false)}, `Path "/api/p/nope"`},
		{"Path is a GET", []ext.Module{mod("p", "proton", "/api/p/sign-in", http.MethodGet, false)}, "POST routes"},
		{"Path is public", []ext.Module{mod("p", "proton", "/api/p/sign-in", http.MethodPost, true)}, "signed-in user"},
		{"Path outside /api/", []ext.Module{{Name: "p", MailboxForms: []ext.MailboxForm{{Provider: "proton", Path: "/p/sign-in"}},
			Routes: func(ext.Host) []ext.Route {
				return []ext.Route{{Method: http.MethodPost, Path: "/p/sign-in", Handler: ok}}
			}}}, "under /api/"},
		{"two forms for proton", []ext.Module{mod("a", "proton", "/api/p/sign-in", http.MethodPost, false), mod("b", "proton", "/api/p/sign-in", http.MethodPost, false)},
			"both have a sign-in form for proton"},
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
