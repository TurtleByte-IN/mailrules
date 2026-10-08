package daemon

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/worker"
)

// accountPollInterval is how often a running daemon looks for mailboxes added behind its
// back, by `mailrules accounts add` in another process.
const accountPollInterval = 5 * time.Second

// accountWatcher starts a supervisor for every account the daemon has not seen yet. The
// command line writes straight to the database and cannot signal the daemon, so the
// daemon polls the accounts table. An account is acted on once: one that was paused,
// stopped or removed afterwards is left as the user left it.
type accountWatcher struct {
	st    *store.Store
	begin func(store.Account) // starts a supervisor

	mu   sync.Mutex
	seen map[int64]bool
}

func newAccountWatcher(st *store.Store, begin func(store.Account)) *accountWatcher {
	return &accountWatcher{st: st, begin: begin, seen: map[int64]bool{}}
}

// Start is how serve and the HTTP layer start an account; it also records the account as seen.
func (w *accountWatcher) Start(acct store.Account) {
	w.mu.Lock()
	w.seen[acct.ID] = true
	w.mu.Unlock()
	w.begin(acct)
}

// Seed records the accounts that existed at startup, paused or not.
func (w *accountWatcher) Seed(accounts []store.Account) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, a := range accounts {
		w.seen[a.ID] = true
	}
}

// Poll starts each account not seen before and returns how many it started.
func (w *accountWatcher) Poll(ctx context.Context) (int, error) {
	accounts, err := w.st.Accounts(ctx)
	if err != nil {
		return 0, err
	}
	var fresh []store.Account
	w.mu.Lock()
	for _, a := range accounts {
		if !w.seen[a.ID] {
			w.seen[a.ID] = true
			fresh = append(fresh, a)
		}
	}
	w.mu.Unlock()
	n := 0
	for _, a := range fresh {
		if a.Status == worker.StatusPaused {
			continue
		}
		slog.Info("account added outside the daemon; starting it", "account", a.ID, "host", a.Host)
		w.begin(a)
		n++
	}
	return n, nil
}

// Run polls every interval until ctx is done.
func (w *accountWatcher) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := w.Poll(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("could not look for new accounts", "error", err)
			}
		}
	}
}
