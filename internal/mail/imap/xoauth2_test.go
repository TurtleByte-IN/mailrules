package imap

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/imap/imaptest"
)

// issuer is a fake OAuth provider: each call issues a new access token the test server
// accepts, as a token endpoint does.
type issuer struct {
	s  *imaptest.Server
	mu sync.Mutex
	n  int
}

func (i *issuer) token(context.Context) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.n++
	tok := fmt.Sprintf("access-%d", i.n)
	i.s.AllowToken(tok)
	return tok, nil
}

func oauthConfig(s *imaptest.Server, token func(context.Context) (string, error)) Config {
	cfg := config(s)
	cfg.Password, cfg.Token = "", token
	return cfg
}

func TestXOAuth2Login(t *testing.T) {
	s := imaptest.Start(t, capsMove)
	s.Append(t, "INBOX", eml(1))
	iss := &issuer{s: s}
	m := open(t, oauthConfig(s, iss.token))
	if got := uids(t, m, "INBOX"); len(got) != 1 {
		t.Fatalf("uids = %v", got)
	}
	if got := s.TokenLogins(); !slices.Equal(got, []string{"access-1"}) {
		t.Errorf("token logins = %v, want the one issued", got)
	}
}

func TestXOAuth2Errors(t *testing.T) {
	s := imaptest.Start(t, capsMove)
	revoked := fmt.Errorf("refresh token revoked: %w", mail.ErrReconnect)
	for _, tc := range []struct {
		name  string
		token func(context.Context) (string, error)
		want  error
		not   error
	}{
		{"the server refuses the token", func(context.Context) (string, error) { return "expired-token", nil }, mail.ErrReconnect, mail.ErrAuth},
		{"the module says reconnect", func(context.Context) (string, error) { return "", revoked }, mail.ErrReconnect, mail.ErrConnection},
		{"the token endpoint is down", func(context.Context) (string, error) { return "", errors.New("token endpoint: 503") }, mail.ErrConnection, mail.ErrReconnect},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Open(t.Context(), oauthConfig(s, tc.token))
			if !errors.Is(err, tc.want) || errors.Is(err, tc.not) {
				t.Fatalf("err = %v, want %v and not %v", err, tc.want, tc.not)
			}
			if strings.Contains(err.Error(), "expired-token") {
				t.Errorf("the error shows the token: %v", err)
			}
		})
	}
}

// Gmail and Outlook end a session when its access token expires, about an hour in. The
// watcher and the worker connection come back with a fresh token, nothing is missed, and
// nothing is reported as an error.
func TestXOAuth2SessionExpiry(t *testing.T) {
	s := imaptest.Start(t, capsMove)
	iss := &issuer{s: s}
	m := open(t, oauthConfig(s, iss.token))
	out := watch(t, m, 0)
	expect(t, out, s.Append(t, "INBOX", eml(1))) // the watcher is idling
	before := len(s.TokenLogins())

	for round := range 2 {
		s.ExpireSessions() // every token issued so far stops working
		expect(t, out, s.Append(t, "INBOX", eml(10+round)))
		var err error
		for range 50 {
			if _, _, err = m.FetchSince(t.Context(), "INBOX", time.Time{}, 0); err == nil {
				break
			}
			if !errors.Is(err, mail.ErrConnection) {
				t.Fatalf("round %d: worker err = %v, want a lost connection at most", round, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("round %d: the worker never came back: %v", round, err)
		}
	}
	logins := s.TokenLogins()
	if len(logins) < before+4 { // the watcher and the worker, twice
		t.Fatalf("token logins = %v, want fresh ones after each expiry", logins)
	}
	seen := map[string]bool{}
	for _, tok := range logins {
		if seen[tok] {
			t.Errorf("token %s was used for two logins; each login must ask for a fresh one", tok)
		}
		seen[tok] = true
	}
}

func TestWatchStopsOnReconnect(t *testing.T) {
	s := imaptest.Start(t, capsMove)
	iss := &issuer{s: s}
	var revoked sync.Map
	m := open(t, oauthConfig(s, func(ctx context.Context) (string, error) {
		if _, ok := revoked.Load("yes"); ok {
			return "", fmt.Errorf("invalid_grant: %w", mail.ErrReconnect)
		}
		return iss.token(ctx)
	}))
	revoked.Store("yes", true) // the person revoked access after Open
	s.ExpireSessions()
	err := m.Watch(t.Context(), "INBOX", 0, make(chan mail.NewMail))
	if !errors.Is(err, mail.ErrReconnect) {
		t.Fatalf("Watch err = %v, want ErrReconnect without retrying", err)
	}
}
