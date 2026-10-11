package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/presets"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/worker"
)

// oneClickPresets are the presets a module may sign in to (ext.MailboxSignIn.Providers):
// the providers whose IMAP servers take OAuth access tokens (SASL XOAUTH2).
var oneClickPresets = []string{"gmail", "outlook"}

// formPresets are the presets a module may connect through a sign-in form
// (ext.MailboxForm.Provider): Proton, whose mailboxes the hosted build reads through the
// operator's Proton Mail Bridge.
var formPresets = []string{"proton"}

// mailboxSignIn is the module that connects mailboxes with one-click sign-in, or nil.
func (s *server) mailboxSignIn() *ext.MailboxSignIn {
	for i := range s.Modules {
		if s.Modules[i].Mailboxes != nil {
			return s.Modules[i].Mailboxes
		}
	}
	return nil
}

// oneClickURL is where the browser starts connecting a mailbox of preset with one-click
// sign-in, or nil when no module of the build signs in to it.
func (s *server) oneClickURL(preset string) *string {
	m := s.mailboxSignIn()
	if m == nil || !slices.Contains(m.Providers, preset) {
		return nil
	}
	u := m.Connect + "?provider=" + url.QueryEscape(preset)
	return &u
}

// mailboxForm is the sign-in form a module of the build answers for preset, or nil.
func (s *server) mailboxForm(preset string) *ext.MailboxForm {
	for i := range s.Modules {
		for j := range s.Modules[i].MailboxForms {
			if f := &s.Modules[i].MailboxForms[j]; f.Provider == preset {
				return f
			}
		}
	}
	return nil
}

// formURL is where the wizard posts the steps of preset's sign-in form, or nil when no
// module of the build answers one.
func (s *server) formURL(preset string) *string {
	if f := s.mailboxForm(preset); f != nil {
		return &f.Path
	}
	return nil
}

// AddMailbox connects a mailbox a module signs in to for the signed-in user (ext.Host):
// a one-click mailbox, or with m.Password one a module's sign-in form connected.
func (h host) AddMailbox(ctx context.Context, m ext.NewMailbox) (int64, error) {
	who, ok := ctxUser(ctx)
	if !ok {
		return 0, errNoUser
	}
	preset, ok := presets.Get(m.Provider)
	if m.Password {
		ok = ok && h.s.mailboxForm(m.Provider) != nil
	} else {
		sign := h.s.mailboxSignIn()
		ok = ok && sign != nil && slices.Contains(sign.Providers, m.Provider)
	}
	if !ok {
		return 0, fmt.Errorf("add mailbox: %q is not one of the providers the module signs in to", m.Provider)
	}
	address := strings.TrimSpace(m.Address)
	if address == "" || m.Secret == "" {
		return 0, errors.New("add mailbox: an address and a secret are needed")
	}
	a := store.Account{Label: address, Preset: preset.Name, Host: preset.Host, Port: preset.Port, TLSMode: preset.TLSMode,
		Username: address, WatchFolder: "INBOX", OAuth: !m.Password}
	if m.Host != "" {
		a.Host = m.Host
	}
	if m.Port != 0 {
		a.Port = m.Port
	}
	if m.TLSMode != "" {
		a.TLSMode = m.TLSMode
	}
	if a.TLSMode != presets.TLSImplicit && a.TLSMode != presets.TLSStartTLS {
		return 0, fmt.Errorf("add mailbox: TLS mode %q is neither implicit nor starttls", a.TLSMode)
	}
	if m.CertFingerprint != "" {
		fp, err := mail.ParseFingerprint(m.CertFingerprint)
		if err != nil {
			return 0, fmt.Errorf("add mailbox: %w", err)
		}
		a.CertFingerprint = fp
	}
	exists, err := h.s.alreadyConnected(ctx, who.TenantID, a.Host, a.Username)
	if err != nil {
		return 0, fmt.Errorf("add mailbox: %w", err)
	}
	if exists {
		return 0, ext.ErrMailboxExists
	}
	secret := m.Secret // replaced when the module rotates it while logging in
	var mb mail.Mailbox
	if m.Password {
		mb, a.Username, err = h.s.Connect(ctx, a, secret)
	} else {
		mb, err = h.s.ConnectOAuth(ctx, a, &secret)
	}
	if err != nil {
		return 0, fmt.Errorf("add mailbox: %w", err)
	}
	c, err := firstConnect(ctx, mb, a)
	if err != nil {
		return 0, fmt.Errorf("add mailbox: %w", err)
	}
	acct, err := h.s.saveAccount(ctx, who.ID, c, secret)
	if err != nil {
		return 0, fmt.Errorf("add mailbox: %w", err)
	}
	return acct.ID, nil
}

// ReconnectMailbox gives a one-click mailbox of the signed-in user a new secret and
// connects it again (ext.Host).
func (h host) ReconnectMailbox(ctx context.Context, accountID int64, address, secret string) error {
	who, ok := ctxUser(ctx)
	if !ok {
		return errNoUser
	}
	a, err := h.s.store.Account(ctx, accountID)
	if errors.Is(err, store.ErrNotFound) {
		return ext.ErrNoMailbox
	}
	if err != nil {
		return fmt.Errorf("reconnect mailbox: %w", err)
	}
	sign := h.s.mailboxSignIn()
	if a.UserID != who.ID || !a.OAuth || sign == nil || !slices.Contains(sign.Providers, a.Preset) {
		return ext.ErrNoMailbox
	}
	if !strings.EqualFold(strings.TrimSpace(address), a.Username) {
		return ext.ErrMailboxMismatch
	}
	if secret == "" {
		return errors.New("reconnect mailbox: a secret is needed")
	}
	// The new secret is tried before it replaces the stored one.
	mb, err := h.s.ConnectOAuth(ctx, a, &secret)
	if err != nil {
		return fmt.Errorf("reconnect mailbox: %w", err)
	}
	_ = mb.Close()
	h.s.StopAccount(a.ID) // before the row changes, so the old supervisor cannot write over it
	if err := h.s.store.SetAccountSecret(ctx, h.s.Master, a.ID, secret); err != nil {
		return fmt.Errorf("reconnect mailbox: %w", err)
	}
	if a.Status != worker.StatusPaused {
		a.Status, a.LastError, a.LastEventAt = "new", "", h.s.now().Unix() // until the supervisor reports
		if err := h.s.store.SetAccountStatus(ctx, a.ID, a.Status, "", a.LastEventAt); err != nil {
			return fmt.Errorf("reconnect mailbox: %w", err)
		}
		h.s.StartAccount(a)
	}
	h.s.Hub.Publish(a.TenantID, a.ID, events.AccountStatus, a)
	return nil
}

// MailboxAdded sends the browser to the wizard's Rules step for the mailbox (ext.Host).
func (h host) MailboxAdded(w http.ResponseWriter, r *http.Request, accountID int64) {
	http.Redirect(w, r, "/#/accounts?added="+strconv.FormatInt(accountID, 10), http.StatusSeeOther)
}

// MailboxReconnected sends the browser to Mailboxes (ext.Host).
func (h host) MailboxReconnected(w http.ResponseWriter, r *http.Request, accountID int64) {
	http.Redirect(w, r, "/#/accounts?reconnected="+strconv.FormatInt(accountID, 10), http.StatusSeeOther)
}

// MailboxFailed sends the browser to Mailboxes with code (ext.Host).
func (h host) MailboxFailed(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, "/#/accounts?mailbox_error="+url.QueryEscape(code), http.StatusSeeOther)
}
