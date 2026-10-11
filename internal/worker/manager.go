package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
)

// ErrNotConnected means the account has no open connection right now.
var ErrNotConnected = errors.New("account is not connected")

// Manager runs the supervisors and is how the action executor reaches an account
// (it implements actions.Accounts).
type Manager struct {
	mu       sync.Mutex
	sups     map[int64]*running
	cleaning map[int64]bool // accounts with a cleanup Sort going
	checks   *CheckStore    // the current cleanup check of each account, in memory (MAI-44)
	wg       sync.WaitGroup // supervisors, cleanup Sorts and cleanup checks
}

// Checks is the account cleanup checks held in the daemon's memory (MAI-44). It is created
// on first use, so a Manager needs no constructor.
func (m *Manager) Checks() *CheckStore {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.checks == nil {
		m.checks = NewCheckStore()
	}
	return m.checks
}

type running struct {
	sup    *Supervisor
	ctx    context.Context // done when the supervisor is stopped; a cleanup run of the account stops with it
	cancel context.CancelFunc
	done   chan struct{}
}

// Start runs a supervisor until ctx is done or Stop is called for its account. An
// account that is already running is stopped first. An account whose secret was deleted
// (store.Account.SecretGone) is only stopped: it cannot log in until its owner enters a
// new password, which starts it again.
func (m *Manager) Start(ctx context.Context, s *Supervisor) {
	m.Stop(s.Account.ID)
	if s.Account.SecretGone {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	r := &running{sup: s, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	m.mu.Lock()
	if m.sups == nil {
		m.sups = map[int64]*running{}
	}
	m.sups[s.Account.ID] = r
	m.mu.Unlock()
	m.wg.Go(func() {
		defer close(r.done)
		defer cancel()
		s.Run(ctx)
	})
}

// Stop ends an account's supervisor and waits until it has: mail already queued gets the
// supervisor's drain time to finish. Stopping an account that is not running does nothing.
func (m *Manager) Stop(accountID int64) {
	m.mu.Lock()
	r := m.sups[accountID]
	delete(m.sups, accountID)
	m.mu.Unlock()
	if r != nil {
		r.cancel()
		<-r.done
	}
}

// Wait blocks until every supervisor and every cleanup run has stopped.
func (m *Manager) Wait() { m.wg.Wait() }

func (m *Manager) supervisor(accountID int64) *Supervisor {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.sups[accountID]; r != nil {
		return r.sup
	}
	return nil
}

// Mailbox returns the account's open connection, or ErrNotConnected.
func (m *Manager) Mailbox(accountID int64) (mail.Mailbox, error) {
	if s := m.supervisor(accountID); s != nil {
		if mb := s.Mailbox(); mb != nil {
			return mb, nil
		}
	}
	return nil, fmt.Errorf("account %d: %w", accountID, ErrNotConnected)
}

// Lock waits until the account's processor is between messages and holds it there until
// unlock is called. Undo and corrections move mail back into the watched folder; holding
// the lock until that is on record keeps the processor from sorting it again as new mail.
func (m *Manager) Lock(accountID int64) (unlock func()) {
	s := m.supervisor(accountID)
	if s == nil {
		return func() {}
	}
	s.work.Lock()
	return s.work.Unlock
}
