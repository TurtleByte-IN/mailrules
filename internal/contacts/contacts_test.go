package contacts

import (
	"errors"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/mailtest"
	"github.com/TurtleByte-IN/mailrules/internal/message"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

func TestSyncAndFill(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	st := store.New(db)
	u, _ := st.CreateFirstUser(ctx, "me@icloud.com", "hash", 1)
	acct, err := st.CreateAccount(ctx, make([]byte, 32), store.Account{UserID: u.ID, Label: "x", Preset: "generic", Host: "h", Port: 993, TLSMode: "implicit", Username: "me"}, "pw")
	if err != nil {
		t.Fatal(err)
	}

	mb := mailtest.New(acct.ID)
	mb.AddFolder("Sent", mail.RoleSent)
	mb.Deliver("Sent", "To: Ann <ann@example.org>\nCc: Bob@Example.org\nSubject: hi\n\nbody\n")
	mb.Deliver("Sent", "To: ann@example.org\nSubject: again\n\nbody\n")

	if n, err := Sync(ctx, mb, st, acct.ID, "Sent"); err != nil || n != 2 {
		t.Fatalf("Sync = %d, %v", n, err)
	}
	if n, err := Sync(ctx, mb, st, acct.ID, "Sent"); err != nil || n != 2 { // running again changes nothing
		t.Fatalf("second Sync = %d, %v", n, err)
	}
	if _, err := Sync(ctx, mb, st, acct.ID, "Missing"); !errors.Is(err, mail.ErrNoFolder) {
		t.Errorf("Sync of a missing folder: %v", err)
	}

	for _, tc := range []struct {
		from string
		want bool
	}{{"bob@example.org", true}, {"ann@example.org", true}, {"stranger@example.org", false}} {
		s := &message.Summary{AccountID: acct.ID, From: tc.from}
		if err := Fill(ctx, st, s); err != nil || s.IsContact != tc.want || s.RepliedBefore != tc.want {
			t.Errorf("%s: contact %v replied %v, %v", tc.from, s.IsContact, s.RepliedBefore, err)
		}
	}
}
