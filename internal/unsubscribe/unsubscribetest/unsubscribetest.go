// Package unsubscribetest is an in-memory unsubscribe.Unsubscriber for tests of the packages
// that unsubscribe.
package unsubscribetest

import (
	"context"
	"slices"
	"sync"

	"github.com/TurtleByte-IN/mailrules/internal/unsubscribe"
)

// Fake keeps every link it is asked to call. Err, when set, answers each call instead of
// success, as a site that refuses or cannot be reached would; a failed call is kept too.
type Fake struct {
	mu    sync.Mutex
	calls []string
	Err   func(link string) error
}

var _ unsubscribe.Unsubscriber = (*Fake)(nil)

// Unsubscribe records link and returns Err's answer for it, or nil.
func (f *Fake) Unsubscribe(ctx context.Context, link string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, link)
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.Err != nil {
		return f.Err(link)
	}
	return nil
}

// SetErr changes Err while calls may be running.
func (f *Fake) SetErr(err func(link string) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Err = err
}

// Calls returns the links called so far, oldest first.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}
