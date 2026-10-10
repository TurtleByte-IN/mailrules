package daemon

import (
	"context"
	"crypto/tls"
	"fmt"
	"slices"
	"sync"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/imap"
	"github.com/TurtleByte-IN/mailrules/internal/mail/presets"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// mailLogins signs in to one-click mailboxes (store.Account.OAuth) through the module that
// connects them (ext.MailboxSignIn): before each IMAP login it hands the module the
// mailbox's secret and takes back an access token, storing a rotated secret first. The
// secret is decrypted for each login and kept nowhere else.
type mailLogins struct {
	st        *store.Store
	master    []byte
	signIn    *ext.MailboxSignIn // nil: the build connects no mailbox this way
	tlsConfig *tls.Config        // nil outside tests: verify against the system roots

	mu    sync.Mutex
	locks map[int64]*sync.Mutex // per mailbox: its connections log in one at a time
}

func newMailLogins(st *store.Store, master []byte, modules []ext.Module) *mailLogins {
	l := &mailLogins{st: st, master: master, locks: map[int64]*sync.Mutex{}}
	for i := range modules {
		if modules[i].Mailboxes != nil {
			l.signIn = modules[i].Mailboxes
		}
	}
	return l
}

// lock serialises the logins of one mailbox, so a secret the module rotates on one
// connection is the one the next connection hands it.
func (l *mailLogins) lock(accountID int64) *sync.Mutex {
	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.locks[accountID]
	if m == nil {
		m = &sync.Mutex{}
		l.locks[accountID] = m
	}
	return m
}

// login asks the module for an access token for a mailbox of provider with secret.
func (l *mailLogins) login(ctx context.Context, provider, secret string) (ext.MailToken, error) {
	if l.signIn == nil || !slices.Contains(l.signIn.Providers, provider) {
		return ext.MailToken{}, fmt.Errorf("%w: this build has no one-click sign-in for %s mailboxes", mail.ErrReconnect, provider)
	}
	return l.signIn.Login(ctx, provider, secret)
}

// stored is imap.Config.Token for a stored mailbox: each call reads its secret, asks the
// module, and stores a rotated secret before the token is used.
func (l *mailLogins) stored(acct store.Account) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		mu := l.lock(acct.ID)
		mu.Lock()
		defer mu.Unlock()
		secret, err := l.st.AccountSecret(ctx, l.master, acct.ID)
		if err != nil {
			return "", err
		}
		tok, err := l.login(ctx, acct.Preset, secret)
		if err != nil {
			return "", err
		}
		if tok.Secret != "" && tok.Secret != secret {
			if err := l.st.SetAccountSecret(ctx, l.master, acct.ID, tok.Secret); err != nil {
				return "", fmt.Errorf("store the renewed secret: %w", err)
			}
		}
		return tok.AccessToken, nil
	}
}

// fresh is imap.Config.Token for a secret not stored yet; *secret follows its rotations.
// Its connection's logins are one at a time.
func (l *mailLogins) fresh(provider string, secret *string) func(context.Context) (string, error) {
	var mu sync.Mutex
	return func(ctx context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		tok, err := l.login(ctx, provider, *secret)
		if err != nil {
			return "", err
		}
		if tok.Secret != "" {
			*secret = tok.Secret
		}
		return tok.AccessToken, nil
	}
}

// open logs in to a one-click mailbox: with its stored secret when secret is nil, else
// with secret, which is not stored yet (api.Options.ConnectOAuth).
func (l *mailLogins) open(ctx context.Context, acct store.Account, secret *string) (mail.Mailbox, error) {
	token := l.stored(acct)
	if secret != nil {
		token = l.fresh(acct.Preset, secret)
	}
	preset, _ := presets.Get(acct.Preset)
	mb, err := imap.Open(ctx, imap.Config{
		AccountID: acct.ID, Host: acct.Host, Port: acct.Port, TLSMode: acct.TLSMode,
		Username: acct.Username, Token: token, Preset: preset, TLSConfig: l.tlsConfig, CertFingerprint: acct.CertFingerprint,
	})
	if err != nil {
		return nil, err // not mb: a nil *imap.Mailbox is not a nil mail.Mailbox
	}
	return mb, nil
}
