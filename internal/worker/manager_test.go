package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/store"
)

func TestManager(t *testing.T) {
	e := newEnv(t)
	m := &Manager{}
	if _, err := m.Mailbox(e.sup.Account.ID); !errors.Is(err, ErrNotConnected) {
		t.Errorf("mailbox of an unknown account: %v", err)
	}
	m.Lock(999)() // an account without a supervisor has no processor to hold back

	ctx, cancel := context.WithCancel(t.Context())
	m.Start(ctx, e.sup)
	if _, err := m.Mailbox(e.sup.Account.ID + 1); !errors.Is(err, ErrNotConnected) {
		t.Errorf("mailbox of another account: %v", err)
	}
	eventually(t, "the account's mailbox", func() bool {
		mb, err := m.Mailbox(e.sup.Account.ID)
		return err == nil && mb != nil
	})
	eventually(t, "status live", func() bool {
		a, _ := e.st.Account(t.Context(), e.sup.Account.ID)
		return a.Status == StatusLive
	})

	// While the account is locked, new mail waits; it is processed once the lock is released.
	unlock := m.Lock(e.sup.Account.ID)
	ref := e.mb.Deliver("INBOX", eml("held back"))
	time.Sleep(30 * time.Millisecond) // long enough for the watcher to report it and the processor to reach the lock
	if got := e.state(t, ref); got != "" {
		t.Errorf("a message was processed while the account was locked (state %s)", got)
	}
	unlock()
	eventually(t, "processing after unlock", func() bool { return e.state(t, ref) == store.StateActed })

	cancel()
	m.Wait()
	if _, err := m.Mailbox(e.sup.Account.ID); !errors.Is(err, ErrNotConnected) {
		t.Errorf("mailbox after shutdown: %v", err)
	}
}
