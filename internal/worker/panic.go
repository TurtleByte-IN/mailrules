package worker

import (
	"context"
	"errors"

	"github.com/TurtleByte-IN/mailrules/internal/telemetry"
)

// errPanicked is what a piece of a mailbox's work fails with when it panicked. The panic is
// logged at error with its stack, and the mailbox goes on as after any other failure: a
// panic in one mailbox's work never stops the daemon, as a panic serving a request does not.
var errPanicked = errors.New("MailRules hit an internal error; trying again")

// guard runs fn, a piece of the work of mailbox account, and turns a panic in it into
// errPanicked after logging it under msg.
func guard(ctx context.Context, msg string, account int64, fn func() error) (err error) {
	defer func() {
		if v := recover(); v != nil {
			telemetry.LogPanic(ctx, msg, v, "account", account)
			err = errPanicked
		}
	}()
	return fn()
}

// locked runs fn holding the mailbox's processor lock (Manager.Lock), which it lets go even
// when fn panics: a lock left held would stall the mailbox, and every undo of it, for good.
func (s *Supervisor) locked(fn func() error) error {
	s.work.Lock()
	defer s.work.Unlock()
	return fn()
}
