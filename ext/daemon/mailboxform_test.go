package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/emersion/go-imap/v2"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/mail/imap/imaptest"
)

// fakeBridge is a module that connects Proton mailboxes through a sign-in form, as the
// cloud module does with the operator's Proton Mail Bridge: the account has two-factor
// sign-in and a separate mailbox password, and once signed in, "Bridge" (the in-memory
// IMAP server, over STARTTLS with a certificate it made itself) gives the daemon the IMAP
// password and the certificate to pin.
type fakeBridge struct {
	srv *imaptest.Server

	mu  sync.Mutex
	pin string // the fingerprint the module hands the daemon
}

const (
	bridgeProtonPassword  = "proton-account-password" // #nosec G101 -- a test value
	bridgeCode            = "123456"
	bridgeMailboxPassword = "proton-mailbox-password" // #nosec G101 -- a test value
)

func (b *fakeBridge) module() ext.Module {
	return ext.Module{
		Name: "protontest",
		Routes: func(h ext.Host) []ext.Route {
			return []ext.Route{{Method: http.MethodPost, Path: "/api/protontest/sign-in", Handler: func(w http.ResponseWriter, r *http.Request) {
				var in ext.MailboxFormInput
				if !h.ReadJSON(w, r, &in) {
					return
				}
				answer := func(s ext.MailboxFormStep) { h.WriteJSON(w, http.StatusOK, s) }
				switch {
				case in.Step == ext.MailboxStepSignIn && in.Password != bridgeProtonPassword:
					answer(ext.MailboxFormStep{Step: ext.MailboxStepSignIn, Message: "Proton refused that password."})
				case in.Step == ext.MailboxStepSignIn:
					answer(ext.MailboxFormStep{Step: ext.MailboxStepCode, State: "login-1"})
				case in.Step == ext.MailboxStepCode && in.Code != bridgeCode:
					answer(ext.MailboxFormStep{Step: ext.MailboxStepCode, State: in.State, Message: "That code is wrong."})
				case in.Step == ext.MailboxStepCode:
					answer(ext.MailboxFormStep{Step: ext.MailboxStepMailboxPassword, State: in.State})
				case in.Step == ext.MailboxStepMailboxPassword && in.Password == bridgeMailboxPassword && in.State == "login-1":
					b.mu.Lock()
					pin := b.pin
					b.mu.Unlock()
					id, err := h.AddMailbox(r.Context(), ext.NewMailbox{Provider: "proton", Address: imaptest.Username,
						Host: b.srv.Host, Port: b.srv.Port, TLSMode: "starttls", Secret: imaptest.Password, Password: true, CertFingerprint: pin})
					if err != nil {
						answer(ext.MailboxFormStep{Step: ext.MailboxStepSignIn, Message: "Bridge could not be read: " + err.Error()})
						return
					}
					answer(ext.MailboxFormStep{AccountID: id})
				default:
					answer(ext.MailboxFormStep{Step: in.Step, State: in.State, Message: "That mailbox password is wrong."})
				}
			}}}
		},
		MailboxForms: []ext.MailboxForm{{Provider: "proton", Path: "/api/protontest/sign-in"}},
	}
}

// A module's sign-in form connects a Proton mailbox, and the daemon reads it from Bridge
// over STARTTLS, trusting Bridge's self-made certificate only by the pinned fingerprint,
// and sorts its mail.
func TestMailboxFormEndToEnd(t *testing.T) {
	srv := imaptest.StartSTARTTLS(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapMove: {}, imap.CapUIDPlus: {}})
	if err := srv.User.Create("Receipts", nil); err != nil {
		t.Fatal(err)
	}
	bridge := &fakeBridge{srv: srv, pin: strings.Repeat("00", 32)}
	d := serveForTest(t, nil, bridge.module()) // the system's roots: Bridge's certificate is trusted only by its pin
	c := d.c

	// The wizard offers Proton through the module's form only.
	_, body := c.do(http.MethodGet, "/api/presets", nil, http.StatusOK)
	var presetList struct {
		Items []struct {
			Name     string  `json:"name"`
			Password bool    `json:"password"`
			FormURL  *string `json:"form_url"`
		}
	}
	if err := json.Unmarshal(body, &presetList); err != nil {
		t.Fatal(err)
	}
	form := ""
	for _, p := range presetList.Items {
		if p.Name == "proton" && p.FormURL != nil && !p.Password {
			form = *p.FormURL
		}
	}
	if form != "/api/protontest/sign-in" {
		t.Fatalf("presets = %s", body)
	}

	step := func(in ext.MailboxFormInput) ext.MailboxFormStep {
		t.Helper()
		_, body := c.do(http.MethodPost, form, in, http.StatusOK)
		var out ext.MailboxFormStep
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	signIn := func() ext.MailboxFormStep {
		t.Helper()
		if s := step(ext.MailboxFormInput{Step: "sign_in", Username: "me@proton.test", Password: "wrong"}); s.Message != "Proton refused that password." {
			t.Errorf("wrong password = %+v", s)
		}
		s := step(ext.MailboxFormInput{Step: "sign_in", Username: "me@proton.test", Password: bridgeProtonPassword})
		if s.Step != "code" {
			t.Fatalf("after the password = %+v", s)
		}
		if s := step(ext.MailboxFormInput{Step: "code", State: s.State, Code: "000000"}); s.Message != "That code is wrong." {
			t.Errorf("wrong code = %+v", s)
		}
		s = step(ext.MailboxFormInput{Step: "code", State: s.State, Code: bridgeCode})
		if s.Step != "mailbox_password" {
			t.Fatalf("after the code = %+v", s)
		}
		return step(ext.MailboxFormInput{Step: "mailbox_password", State: s.State, Password: bridgeMailboxPassword})
	}

	// With a pin that is not Bridge's certificate, nothing is connected.
	if s := signIn(); s.AccountID != 0 || !strings.Contains(s.Message, "certificate") {
		t.Fatalf("with another pin = %+v", s)
	}
	bridge.mu.Lock()
	bridge.pin = srv.Fingerprint()
	bridge.mu.Unlock()
	s := signIn()
	if s.AccountID == 0 {
		t.Fatalf("with Bridge's pin = %+v", s)
	}
	id := s.AccountID
	_, body = c.do(http.MethodGet, "/api/accounts/"+strconv.FormatInt(id, 10), nil, http.StatusOK)
	var view struct {
		Account struct {
			Preset          string `json:"preset"`
			TLSMode         string `json:"tls_mode"`
			CertFingerprint string `json:"cert_fingerprint"`
			OneClick        bool   `json:"one_click"`
		}
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	if a := view.Account; a.Preset != "proton" || a.TLSMode != "starttls" || a.CertFingerprint != srv.Fingerprint() || a.OneClick {
		t.Fatalf("account = %+v", a)
	}
	eventually(t, "the mailbox goes live", func() (bool, string) { a := c.account(id); return a.Status == "live", a.Status })

	// A rule sorts new mail, with dry-run off.
	c.do(http.MethodPatch, "/api/settings", map[string]any{"dry_run": false}, http.StatusOK)
	c.do(http.MethodPost, "/api/rules/batch", map[string]any{"rules": []map[string]any{{"name": "Receipts",
		"conditions": map[string]any{"field": "subject", "op": "contains_any", "value": []string{"Receipt"}},
		"actions":    []map[string]any{{"type": "move", "folder": "Receipts"}}}}}, http.StatusCreated)
	srv.Append(t, "INBOX", receipt(1))
	eventually(t, "the receipt is sorted", func() (bool, string) {
		n := count(t, srv, "Receipts")
		return n == 1, fmt.Sprint(n)
	})

	// What is stored is Bridge's IMAP password, never what the person typed.
	_, body = c.do(http.MethodGet, "/api/accounts", nil, http.StatusOK)
	for _, typed := range []string{bridgeProtonPassword, bridgeMailboxPassword, imaptest.Password} {
		if strings.Contains(string(body), typed) {
			t.Errorf("the API shows %q", typed)
		}
	}
	if got := d.storedSecret(t, id); got != imaptest.Password {
		t.Errorf("stored secret = %q, want the Bridge password", got)
	}
}
