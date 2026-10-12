package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/store/storetest"
)

// A mailbox whose secret was deleted (a forgotten person's) never reaches the server: a
// password mailbox fails as a refused sign-in, a one-click one as needing to sign in to the
// provider again, and the module is not asked for a token.
func TestDeletedSecretDoesNotLogIn(t *testing.T) {
	ctx := t.Context()
	st, db := storetest.New(t)
	u, err := st.SignInIdentity(ctx, store.Identity{Provider: "p", Subject: "s", Email: "a@example.test", Tenant: "o"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	asked := 0
	logins := newMailLogins(st, []ext.Module{{Name: "m", Mailboxes: &ext.MailboxSignIn{Providers: []string{"gmail"},
		Login: func(context.Context, string, string) (ext.MailToken, error) {
			asked++
			return ext.MailToken{AccessToken: "t"}, nil
		}}}})
	for _, c := range []struct {
		acct store.Account
		want error
	}{
		{store.Account{UserID: u.ID, Label: "pw", Preset: "generic", Host: "127.0.0.1", Port: 1, TLSMode: "implicit", Username: "pw@example.test"}, mail.ErrAuth},
		{store.Account{UserID: u.ID, Label: "oc", Preset: "gmail", Host: "127.0.0.1", Port: 1, TLSMode: "implicit", Username: "oc@example.test", OAuth: true}, mail.ErrReconnect},
	} {
		a, err := st.CreateAccount(ctx, c.acct, "secret")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE accounts SET secret_enc = ?, dek_enc = ? WHERE id = ?`, []byte{}, []byte{}, a.ID); err != nil {
			t.Fatal(err)
		}
		// A one-click mailbox reaches the server and fails at its first login, when the
		// token is asked for: that is stored.
		if a.OAuth {
			_, err = logins.stored(a)(ctx)
		} else {
			_, err = openAccount(logins, a)(ctx)
		}
		if !errors.Is(err, c.want) || !errors.Is(err, store.ErrNoSecret) {
			t.Errorf("%s: %v, want %v and ErrNoSecret", a.Label, err, c.want)
		}
	}
	if asked != 0 {
		t.Errorf("the module was asked for %d tokens", asked)
	}
}
