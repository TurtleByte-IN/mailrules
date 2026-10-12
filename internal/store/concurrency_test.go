package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Transactions that read before they write must not fail while a mailbox supervisor writes
// meanwhile. A deferred transaction that has read cannot then write while another
// connection holds the write lock (SQLITE_BUSY at once: SQLite does not wait the busy
// timeout for a transaction that has read), nor once that connection has committed
// (SQLITE_BUSY_SNAPSHOT, "database is locked (517)"). Each run's transaction starts while a
// supervisor is in the middle of a write, which lets go a few milliseconds later: one that
// takes the write lock at its BEGIN waits for it, one that reads first fails.
//
// The supervisor writes at set times, not nonstop. A connection waiting for the write lock
// tries again only after sleeping (up to 100 ms), so writers that take the lock back the
// moment they let it go can keep it from that connection without end. The supervisors
// that wrote nonstop here waited behind 200 runs back to back until the last was done, and
// on a slow runner (CI, -race) that was past the 5 s busy timeout (MAI-214).
func TestTransactionsWithConcurrentWriters(t *testing.T) {
	for _, c := range []struct {
		name string
		// prepare sets up run i and returns the call to make while the supervisor writes.
		prepare func(ctx context.Context, s *Store, i int) (func() error, error)
	}{
		{"sign in identity", func(ctx context.Context, s *Store, i int) (func() error, error) {
			return func() error {
				_, err := s.SignInIdentity(ctx, Identity{Provider: "workos", Subject: fmt.Sprintf("user_%d", i),
					Email: fmt.Sprintf("p%d@example.test", i), Tenant: "org"}, 100)
				return err
			}, nil
		}},
		{"create account", func(ctx context.Context, s *Store, i int) (func() error, error) {
			u, err := s.FirstUser(ctx)
			if err != nil {
				return nil, err
			}
			return func() error {
				_, err := s.CreateAccount(ctx, Account{UserID: u.ID, Label: "m", Preset: "icloud",
					Host: "imap.mail.me.com", Port: 993, TLSMode: "implicit", Username: fmt.Sprintf("m%d@icloud.com", i)}, "pw")
				return err
			}, nil
		}},
		{"delete account", func(ctx context.Context, s *Store, i int) (func() error, error) {
			u, err := s.FirstUser(ctx)
			if err != nil {
				return nil, err
			}
			a, err := s.CreateAccount(ctx, Account{UserID: u.ID, Label: "m", Preset: "icloud",
				Host: "imap.mail.me.com", Port: 993, TLSMode: "implicit", Username: fmt.Sprintf("m%d@icloud.com", i)}, "pw")
			if err != nil {
				return nil, err
			}
			return func() error {
				_, err := s.DeleteAccount(ctx, a.ID, 100)
				return err
			}, nil
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, db := open(t) // file-backed, in WAL mode
			ctx := t.Context()
			busy := newAccount(t, s, "busy@icloud.com", "pw")

			for i := range 50 {
				call, err := c.prepare(ctx, s, i)
				if err != nil {
					t.Fatalf("run %d: %v", i, err)
				}
				writing, done := make(chan error, 1), make(chan struct{})
				var supervisor sync.WaitGroup
				supervisor.Go(func() { // recording its connection state
					tx, err := db.BeginTx(ctx, nil)
					if err == nil {
						defer func() { _ = tx.Rollback() }() // a no-op after Commit
						_, err = tx.ExecContext(ctx, `UPDATE accounts SET status = 'connected', last_event_at = ? WHERE id = ?`, i, busy.ID)
					}
					writing <- err
					if err != nil {
						return
					}
					select {
					case <-done:
					case <-time.After(5 * time.Millisecond):
					}
					if err := tx.Commit(); err != nil {
						t.Errorf("supervisor write: %v", err)
					}
				})
				if err := <-writing; err != nil {
					t.Fatalf("supervisor write: %v", err)
				}
				err = call()
				close(done)
				supervisor.Wait()
				if err != nil {
					t.Fatalf("run %d: %v", i, err)
				}
			}
		})
	}
}
