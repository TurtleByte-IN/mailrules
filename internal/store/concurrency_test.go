package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Transactions that read before they write must not fail while mailbox supervisors write
// meanwhile: a deferred transaction's read snapshot cannot be upgraded to a write once
// another connection has committed (SQLITE_BUSY_SNAPSHOT, "database is locked (517)").
func TestTransactionsWithConcurrentWriters(t *testing.T) {
	for _, c := range []struct {
		name string
		run  func(ctx context.Context, s *Store, i int) error
	}{
		{"sign in identity", func(ctx context.Context, s *Store, i int) error {
			_, err := s.SignInIdentity(ctx, Identity{Provider: "workos", Subject: fmt.Sprintf("user_%d", i),
				Email: fmt.Sprintf("p%d@example.test", i), Tenant: "org"}, 100)
			return err
		}},
		{"delete account", func(ctx context.Context, s *Store, i int) error {
			u, err := s.FirstUser(ctx)
			if err != nil {
				return err
			}
			a, err := s.CreateAccount(ctx, testMaster, Account{UserID: u.ID, Label: "m", Preset: "icloud",
				Host: "imap.mail.me.com", Port: 993, TLSMode: "implicit", Username: fmt.Sprintf("m%d@icloud.com", i)}, "pw")
			if err != nil {
				return err
			}
			_, err = s.DeleteAccount(ctx, a.ID, 100)
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := open(t) // file-backed, in WAL mode
			ctx := t.Context()
			busy := newAccount(t, s, "busy@icloud.com", "pw")

			stop := make(chan struct{})
			var wg sync.WaitGroup
			for range 2 { // the supervisors, recording their connection state over and over
				wg.Go(func() {
					for n := int64(0); ; n++ {
						select {
						case <-stop:
							return
						case <-time.After(100 * time.Microsecond):
						}
						if err := s.SetAccountStatus(ctx, busy.ID, "connected", "", n); err != nil {
							t.Errorf("supervisor write: %v", err)
							return
						}
					}
				})
			}
			for i := range 200 {
				if err := c.run(ctx, s, i); err != nil {
					t.Errorf("run %d: %v", i, err)
					break
				}
			}
			close(stop)
			wg.Wait()
		})
	}
}
