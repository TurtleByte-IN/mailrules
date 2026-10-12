package store

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/crypto"
)

// told is what removed_mailboxes holds, as the module would be told it: mailbox, provider,
// address and secret.
type told struct {
	account           int64
	preset, user, sec string
	oauth             bool
}

func (w fworld) waiting(t *testing.T) []told {
	t.Helper()
	rs, err := w.s.WaitingRemovedMailboxes(t.Context(), 1<<40, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []told
	for _, r := range rs {
		sec, err := w.s.RemovedMailboxSecret(t.Context(), r.ID)
		if err != nil {
			t.Fatalf("secret of %d: %v", r.ID, err)
		}
		out = append(out, told{r.AccountID, r.Preset, r.Username, sec, r.OAuth})
	}
	return out
}

// A mailbox a module connected is queued for it, with its secret, in the transaction that
// removes the mailbox or deletes its secret, once per secret; a mailbox the person connected
// with their own password never is.
func TestRemovedMailboxesQueued(t *testing.T) {
	sqliteOnly(t, "fails the delete with a SQLite trigger (RAISE)")
	w := forgetWorld(t)
	s, ctx := w.s, t.Context()
	// The solo person's and the leaver's shared mailbox are one-click ones, the leaver's
	// private one a sign-in form's; the rest are password mailboxes.
	for id, set := range map[int64]string{
		w.soloBox:      `oauth = TRUE, module = TRUE, preset = 'gmail'`,
		w.leaverShared: `oauth = TRUE, module = TRUE, preset = 'outlook'`,
		w.leaverPriv:   `module = TRUE, preset = 'proton'`,
	} {
		if _, err := w.db.ExecContext(ctx, `UPDATE accounts SET `+set+` WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}

	// Removing a password mailbox tells nobody; a removal that fails queues nothing.
	if _, err := s.DeleteAccount(ctx, w.otherBox, fNow); err != nil {
		t.Fatal(err)
	}
	if _, err := w.db.ExecContext(ctx, `CREATE TRIGGER refuse BEFORE DELETE ON accounts BEGIN SELECT RAISE(ABORT, 'no'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteAccount(ctx, w.soloBox, fNow); err == nil {
		t.Fatal("the refusing trigger let the removal through")
	}
	if got := w.waiting(t); len(got) != 0 {
		t.Fatalf("queued %+v", got)
	}
	if _, err := w.db.ExecContext(ctx, `DROP TRIGGER refuse`); err != nil {
		t.Fatal(err)
	}

	// Forgetting the leaver queues both of their module mailboxes with the secrets it deletes.
	if _, err := s.ForgetIdentity(ctx, "workos", "user_leaver", fNow); err != nil {
		t.Fatal(err)
	}
	want := []told{
		{w.leaverPriv, "proton", "leaverpriv@imap.test", "pw-leaverpriv", false},
		{w.leaverShared, "outlook", "leavershared@imap.test", "pw-leavershared", true},
	}
	if got := w.waiting(t); !slices.Equal(got, want) {
		t.Fatalf("after forgetting: %+v\nwant %+v", got, want)
	}
	// Forgetting again, and the removal of their data later, have no secret left to tell of.
	if _, err := s.ForgetIdentity(ctx, "workos", "user_leaver", fNow+1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveForgotten(ctx, w.leaver.ID, fDue); err != nil {
		t.Fatal(err)
	}
	if got := w.waiting(t); !slices.Equal(got, want) {
		t.Fatalf("told twice: %+v", got)
	}

	// Forgetting the solo person queues their mailbox; given a new secret since, removing it
	// queues it again, as a second, separate one.
	if _, err := s.ForgetIdentity(ctx, "workos", "user_solo", fNow); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountSecret(ctx, w.soloBox, "refresh-new"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteAccount(ctx, w.soloBox, fNow+2); err != nil {
		t.Fatal(err)
	}
	want = append(want, told{w.soloBox, "gmail", "solo@imap.test", "pw-solo", true}, told{w.soloBox, "gmail", "solo@imap.test", "refresh-new", true})
	if got := w.waiting(t); !slices.Equal(got, want) {
		t.Fatalf("after the solo mailbox: %+v\nwant %+v", got, want)
	}
}

// A queued mailbox waits until it is due, is handed over again after a refusal, is gone once
// taken, and is dropped with its secret after RemovedMailboxDays.
func TestRemovedMailboxLifecycle(t *testing.T) {
	w := forgetWorld(t)
	s, ctx := w.s, t.Context()
	for _, id := range []int64{w.soloBox, w.otherBox} {
		if _, err := w.db.ExecContext(ctx, `UPDATE accounts SET module = TRUE, preset = 'proton' WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DeleteAccount(ctx, w.soloBox, fNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteAccount(ctx, w.otherBox, fNow+10); err != nil {
		t.Fatal(err)
	}
	ids := func(now int64) []int64 {
		t.Helper()
		rs, err := s.WaitingRemovedMailboxes(ctx, now, 10)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, r := range rs {
			out = append(out, r.AccountID)
		}
		return out
	}
	if got := ids(fNow + 9); !slices.Equal(got, []int64{w.soloBox}) {
		t.Fatalf("due at fNow+9: %v", got)
	}
	rs, _ := s.WaitingRemovedMailboxes(ctx, fNow, 10)
	first := rs[0].ID
	if err := s.RemovedMailboxRetry(ctx, first, fNow+100); err != nil {
		t.Fatal(err)
	}
	if got := ids(fNow + 99); !slices.Equal(got, []int64{w.otherBox}) {
		t.Fatalf("a refused one came back before its time: %v", got)
	}
	if rs, _ := s.WaitingRemovedMailboxes(ctx, fNow+100, 10); len(rs) != 2 || rs[1].ID != first || rs[1].Attempts != 1 {
		t.Fatalf("after the retry: %+v", rs)
	}
	if _, err := s.WithKeys(crypto.MasterKey(bytes.Repeat([]byte{8}, 32))).RemovedMailboxSecret(ctx, first); !errors.Is(err, crypto.ErrDecrypt) {
		t.Errorf("another master key: %v, want ErrDecrypt", err)
	}

	if err := s.RemovedMailboxTaken(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemovedMailboxSecret(ctx, first); !errors.Is(err, ErrNotFound) {
		t.Errorf("a taken one's secret: %v, want ErrNotFound", err)
	}
	if got := ids(fNow + 1000); !slices.Equal(got, []int64{w.otherBox}) {
		t.Fatalf("after taking one: %v", got)
	}

	if gone, err := s.ExpireRemovedMailboxes(ctx, fNow+10+RemovedMailboxDays*day-1); err != nil || len(gone) != 0 {
		t.Fatalf("expired before its time: %+v, %v", gone, err)
	}
	gone, err := s.ExpireRemovedMailboxes(ctx, fNow+10+RemovedMailboxDays*day)
	if err != nil || len(gone) != 1 || gone[0].AccountID != w.otherBox {
		t.Fatalf("expired = %+v, %v", gone, err)
	}
	var n int
	if err := w.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM removed_mailboxes`).Scan(&n); err != nil || n != 0 {
		t.Errorf("rows left: %d, %v", n, err)
	}
	// A mailbox queued once the table is empty gets an ID never handed over before.
	if _, err := w.db.ExecContext(ctx, `UPDATE accounts SET module = TRUE WHERE id = ?`, w.mateBox); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteAccount(ctx, w.mateBox, fNow); err != nil {
		t.Fatal(err)
	}
	if rs, _ := s.WaitingRemovedMailboxes(ctx, fNow, 10); len(rs) != 1 || rs[0].ID <= gone[0].ID {
		t.Errorf("after emptying: %+v; want an ID above %d", rs, gone[0].ID)
	}
}
