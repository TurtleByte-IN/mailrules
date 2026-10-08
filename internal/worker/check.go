package worker

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/composer"
	"github.com/TurtleByte-IN/mailrules/internal/events"
)

// Check statuses, as the API reports them.
const (
	CheckRunning = "running" // the real run-through is still going
	CheckReady   = "ready"   // it is through; the rows are kept, waiting for Sort or Discard
	CheckFailed  = "failed"  // the mail server or the model failed
	CheckStale   = "stale"   // the rules changed since the check: Sort is refused until a new one
)

// CheckStore keeps the current cleanup check of each account in the daemon's memory, one
// per account, guarded by one mutex.
//
// MAI-44 knowingly overrides the working agreement that no state lives in process memory a
// restart or a second instance would break. Tilak decided (2026-10-06) that a cleanup
// check — the real, paid run-through of a mailbox that Sort then applies without asking the
// model again — is held here, not in the database: one check per account, a new check
// replacing (and cancelling) the previous one, deleted once Sort has used it or the user
// discards it, and lost on a daemon restart (accepted, no time-based expiry). Its rows
// carry display fields (sender, subject) that must never be logged or written to the
// database.
type CheckStore struct {
	mu        sync.Mutex
	byAccount map[int64]*Check
	nextID    atomic.Int64
}

// NewCheckStore returns an empty store.
func NewCheckStore() *CheckStore { return &CheckStore{byAccount: map[int64]*Check{}} }

func (s *CheckStore) newID() string { return strconv.FormatInt(s.nextID.Add(1), 10) }

// put makes chk the account's current check, cancelling the one it replaces.
func (s *CheckStore) put(chk *Check) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev := s.byAccount[chk.AccountID]; prev != nil && prev.cancel != nil {
		prev.cancel()
	}
	s.byAccount[chk.AccountID] = chk
}

// Get returns the account's current check, or nil.
func (s *CheckStore) Get(accountID int64) *Check {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byAccount[accountID]
}

// All returns every account's current check, by account id.
func (s *CheckStore) All() []*Check {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Check, 0, len(s.byAccount))
	for _, id := range slices.Sorted(maps.Keys(s.byAccount)) {
		out = append(out, s.byAccount[id])
	}
	return out
}

// Discard removes and cancels the account's current check and returns it, or nil.
func (s *CheckStore) Discard(accountID int64) *Check {
	s.mu.Lock()
	defer s.mu.Unlock()
	chk := s.byAccount[accountID]
	if chk != nil {
		if chk.cancel != nil {
			chk.cancel()
		}
		delete(s.byAccount, accountID)
	}
	return chk
}

// Take removes the account's current check, but only when it is the one with id, so a
// newer check that replaced it since is left alone. It returns the removed check, or nil.
// Sort calls it once it has used a check's rows.
func (s *CheckStore) Take(accountID int64, id string) *Check {
	s.mu.Lock()
	defer s.mu.Unlock()
	chk := s.byAccount[accountID]
	if chk == nil || chk.ID != id {
		return nil
	}
	delete(s.byAccount, accountID)
	return chk
}

// Check is one account's current cleanup check and its progress. Read it with State.
type Check struct {
	ID          string
	AccountID   int64
	Folder      string
	Since       int64   // 0 = all mail
	Limit       int     // 0 = no limit
	RuleIDs     []int64 // the rules the check was limited to; nil = every enabled rule (MAI-43)
	Fingerprint string
	StartedAt   int64

	mu         sync.Mutex
	status     string
	done       int
	total      int
	matched    int
	modelCalls int
	tokens     int
	costUSD    float64
	errMsg     string
	rows       []composer.CheckRow
	exclude    []int // the user's unticked selectable row indices, ascending; nil = every selectable row ticked
	cancel     context.CancelFunc
}

// CheckState is a check read back: a copy safe to hand around and render. Rows are present
// only when the check is ready or stale.
type CheckState struct {
	ID          string
	AccountID   int64
	Folder      string
	Since       int64
	Limit       int
	RuleIDs     []int64 // nil = every enabled rule
	Fingerprint string
	Status      string
	Done        int
	Total       int
	Matched     int // emails in the chosen range before the limit; more than Total when it cut the range
	ModelCalls  int
	Tokens      int
	CostUSD     float64
	Error       string
	Rows        []composer.CheckRow
	// Exclude is the saved selection: the unticked selectable row indices, ascending. It
	// lives only as long as the check (MAI-44) and is never written anywhere else.
	Exclude []int
}

func (c *Check) setProgress(p composer.CheckProgress) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.done, c.total, c.matched, c.modelCalls, c.tokens, c.costUSD = p.Done, p.Total, p.Matched, p.ModelCalls, p.Tokens, p.CostUSD
}

// finish records the check's end. It returns the state to publish, or a zero state (empty
// Status) when the check is no longer the account's current one (a newer check replaced it,
// or the daemon is stopping), so a stale goroutine never speaks for the account.
func (c *Check) finish(rows []composer.CheckRow, err error, ctx context.Context) CheckState {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctx.Err() != nil {
		return CheckState{}
	}
	if err != nil {
		c.status, c.errMsg = CheckFailed, "The check stopped: the mail server or the AI model failed. Try again."
		return c.state(false)
	}
	c.rows, c.status = rows, CheckReady
	return c.state(false)
}

// MarkStale moves a ready check to stale: the rules changed since it ran, so Sort must wait
// for a new check. It leaves a running or failed check as it is.
func (c *Check) MarkStale() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status == CheckReady {
		c.status = CheckStale
	}
}

// State returns the whole check, rows included.
func (c *Check) State() CheckState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state(true)
}

func (c *Check) state(withRows bool) CheckState {
	st := CheckState{ID: c.ID, AccountID: c.AccountID, Folder: c.Folder, Since: c.Since, Limit: c.Limit, RuleIDs: slices.Clone(c.RuleIDs),
		Fingerprint: c.Fingerprint, Status: c.status, Done: c.done, Total: c.total, Matched: c.matched, ModelCalls: c.modelCalls,
		Tokens: c.tokens, CostUSD: c.costUSD, Error: c.errMsg}
	if withRows {
		st.Rows = c.rows
		st.Exclude = slices.Clone(c.exclude)
	}
	return st
}

// SetExclude saves the user's selection: the unticked row indices, stored sorted and
// without repeats. It reports false when the check is not ready, since a running, failed or
// stale check has no table to tick. Whether the indices name selectable rows is the
// caller's to check.
func (c *Check) SetExclude(ids []int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status != CheckReady {
		return false
	}
	c.exclude = slices.Compact(slices.Sorted(slices.Values(ids)))
	return true
}

// CheckFunc runs the read-only check and returns its rows. The daemon calls it on the
// account's background context, so a check outlives the browser request that began it.
type CheckFunc func(ctx context.Context, report func(composer.CheckProgress)) ([]composer.CheckRow, error)

// StartCheck begins a cleanup check of an account in the background, keyed in the store as
// the account's current check (cancelling any previous one). It returns the check at once,
// running; progress and the finished rows arrive as check.progress events and through
// CheckStore.Get. fingerprint is the signature of the rules the check was run against,
// which Sort compares with the rules in force to refuse a stale check.
func (m *Manager) StartCheck(c Cleanup, fingerprint string, run CheckFunc) (*Check, error) {
	m.mu.Lock()
	r := m.sups[c.AccountID]
	m.mu.Unlock()
	if r == nil {
		return nil, ErrNotConnected
	}
	ctx, cancel := context.WithCancel(r.ctx)
	now := time.Now
	if r.sup.Pipeline.Now != nil {
		now = r.sup.Pipeline.Now
	}
	checks := m.Checks()
	chk := &Check{ID: checks.newID(), AccountID: c.AccountID, Folder: c.Folder, Limit: c.Limit, RuleIDs: slices.Clone(c.RuleIDs),
		Fingerprint: fingerprint, StartedAt: now().Unix(), status: CheckRunning, cancel: cancel}
	if !c.Since.IsZero() {
		chk.Since = c.Since.Unix()
	}
	checks.put(chk)
	hub := r.sup.Hub
	began := time.Now()
	slog.InfoContext(ctx, "cleanup check started", "account", c.AccountID, "folder", c.Folder, "limit", c.Limit)
	m.wg.Go(func() {
		defer cancel()
		rows, err := run(ctx, func(p composer.CheckProgress) {
			chk.setProgress(p)
			hub.Publish(events.CheckProgress, chk.progressState())
		})
		st := chk.finish(rows, err, ctx)
		if st.Status == "" { // replaced, or the daemon is stopping: it is not the account's any more
			return
		}
		hub.Publish(events.CheckProgress, st)
		if st.ModelCalls > 0 {
			hub.Publish(events.UsageUpdated, nil) // the stats screens may show the calls the check booked
		}
		if st.Status == CheckFailed {
			slog.WarnContext(ctx, "cleanup check failed", "account", c.AccountID, "folder", c.Folder,
				"done", st.Done, "total", st.Total, "duration_ms", time.Since(began).Milliseconds())
			return
		}
		slog.InfoContext(ctx, "cleanup check finished", "account", c.AccountID, "folder", c.Folder, "messages", st.Total,
			"model_calls", st.ModelCalls, "cost_usd", st.CostUSD, "duration_ms", time.Since(began).Milliseconds())
	})
	return chk, nil
}

func (c *Check) progressState() CheckState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state(false)
}
