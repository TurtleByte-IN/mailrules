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
	mu   sync.Mutex
	sups map[int64]*Supervisor
	wg   sync.WaitGroup
}

// Start runs a supervisor until ctx is done.
func (m *Manager) Start(ctx context.Context, s *Supervisor) {
	m.mu.Lock()
	if m.sups == nil {
		m.sups = map[int64]*Supervisor{}
	}
	m.sups[s.Account.ID] = s
	m.mu.Unlock()
	m.wg.Go(func() { s.Run(ctx) })
}

// Wait blocks until every supervisor has stopped.
func (m *Manager) Wait() { m.wg.Wait() }

func (m *Manager) supervisor(accountID int64) *Supervisor {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sups[accountID]
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
