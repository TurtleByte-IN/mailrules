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
	"github.com/TurtleByte-IN/mailrules/internal/mail/presets"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/worker"
)

// oneClickPresets are the presets a module may sign in to (ext.MailboxSignIn.Providers):
// the providers whose IMAP servers take OAuth access tokens (SASL XOAUTH2).
var oneClickPresets = []string{"gmail", "outlook"}

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

// AddMailbox connects a one-click mailbox for the signed-in user (ext.Host).
func (h host) AddMailbox(ctx context.Context, m ext.NewMailbox) (int64, error) {
	who, ok := ctxUser(ctx)
	if !ok {
		return 0, errNoUser
	}
	sign := h.s.mailboxSignIn()
	preset, ok := presets.Get(m.Provider)
	if sign == nil || !ok || !slices.Contains(sign.Providers, m.Provider) {
		return 0, fmt.Errorf("add mailbox: %q is not one of the providers the module signs in to", m.Provider)
	}
	address := strings.TrimSpace(m.Address)
	if address == "" || m.Secret == "" {
		return 0, errors.New("add mailbox: an address and a secret are needed")
	}
	a := store.Account{Label: address, Preset: preset.Name, Host: preset.Host, Port: preset.Port, TLSMode: preset.TLSMode,
		Username: address, WatchFolder: "INBOX", OAuth: true}
	if m.Host != "" {
		a.Host = m.Host
	}
	if m.Port != 0 {
		a.Port = m.Port
	}
	exists, err := h.s.alreadyConnected(ctx, who.TenantID, a.Host, a.Username)
	if err != nil {
		return 0, fmt.Errorf("add mailbox: %w", err)
	}
	if exists {
		return 0, ext.ErrMailboxExists
	}
	secret := m.Secret // replaced when the module rotates it while logging in
	mb, err := h.s.ConnectOAuth(ctx, a, &secret)
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
