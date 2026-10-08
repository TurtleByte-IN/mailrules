// Package mailertest is an in-memory mailer.Sender for tests of the packages that send email.
package mailertest

import (
	"context"
	"slices"
	"sync"

	"github.com/TurtleByte-IN/mailrules/internal/mailer"
)

// Sender keeps every message it is given. Set Err to make the next sends fail, as a mail
// server that cannot be reached or refuses would; a failed send keeps nothing.
type Sender struct {
	mu   sync.Mutex
	sent []mailer.Message
	Err  error
	// Tries counts every Send, failed ones included.
	tries int
}

var _ mailer.Sender = (*Sender)(nil)

// Send records m, or returns Err.
func (s *Sender) Send(ctx context.Context, m mailer.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tries++
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.Err != nil {
		return s.Err
	}
	s.sent = append(s.sent, m)
	return nil
}

// SetErr changes Err while sends may be running.
func (s *Sender) SetErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Err = err
}

// Sent returns the messages sent so far, oldest first.
func (s *Sender) Sent() []mailer.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.sent)
}

// Tries counts the sends attempted, failed ones included.
func (s *Sender) Tries() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tries
}
