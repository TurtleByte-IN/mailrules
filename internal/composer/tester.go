// Package composer holds the two ways a user tries rules out before they act: the rule
// composer, which turns free text into validated draft rules, and the rule tester, which
// runs saved or draft rules over recent mail. Neither saves anything, and neither can
// change a mailbox: all they are given of one is Reader.
package composer

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/contacts"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/message"
	"github.com/TurtleByte-IN/mailrules/internal/pipeline"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// Reader is the read-only part of mail.Mailbox. FetchMany reads with BODY.PEEK, so a tested
// message stays unread; there is no method here that could move or flag one.
type Reader interface {
	FetchMany(ctx context.Context, refs []mail.MsgRef, maxBody int) ([]*message.Raw, error)
	FetchSince(ctx context.Context, folder string, since time.Time, limit int) ([]mail.MsgRef, int, error)
}

// Limits of one test run.
const (
	DefaultLimit = 200
	MaxLimit     = 2000
	// workers is how many messages are in flight at once; the model calls among them
	// are capped again, daemon-wide, by models.Caller.
	workers = 8
	// fetchBatch is how many messages one request to the mail server reads (MAI-59). The
	// connector runs one command at a time, so one request per message would make a scan
	// as many round trips as it has emails.
	fetchBatch = 100
)

// slowFetch is how long one request to the mail server may take before it is logged on its own.
var slowFetch = 2 * time.Second

// Tester runs rules over the newest mail of one folder and reports what each email would
// get. Bodies are held in memory for one batch of fetched messages only.
type Tester struct {
	Store     *store.Store // the contacts index, for the is_contact and replied_before signals
	Mailbox   Reader
	AccountID int64
	// Decider is the same step the live pipeline decides with. Give it routers made with
	// models.Router.For("test"), so the calls are booked as tests.
	Decider   pipeline.Decider
	BodyChars int
}

// Row is what one email would get. Its JSON is the contract's TestRow.
type Row struct {
	From       string         `json:"from"`
	Subject    string         `json:"subject"`
	ReceivedAt *int64         `json:"received_at"`
	Stage      string         `json:"stage"`
	RuleID     *int64         `json:"rule_id"` // null = no rule, or a draft (see rule_name)
	RuleName   string         `json:"rule_name"`
	Confidence float64        `json:"confidence"`
	Reason     string         `json:"reason"`
	Review     bool           `json:"review"` // below the threshold: it would go to Needs review
	Actions    []rules.Action `json:"actions"`

	rule int64 // the rule's id as given to Run, which for a draft is not a saved rule's
}

// Result is a finished test run. Its JSON is the contract's TestResult.
type Result struct {
	Rows       []Row   `json:"results"` // newest first
	Tested     int     `json:"tested"`
	Matched    int     `json:"matched"` // a rule or sender rule would have applied
	ModelCalls int     `json:"model_calls"`
	CostUSD    float64 `json:"cost_usd"`
}

// Progress is how far a run has got. A run that finds no mail reports one 0 of 0.
type Progress struct {
	Done       int // messages tested or passed over
	Total      int // messages the run will go through, known once the mail is listed
	ModelCalls int // model calls made so far
}

// list reads the newest limit messages of folder, since a time, newest last, and
// logs how long the mail server took. matched is how many the folder holds in that range
// before the limit; the same search that finds them counts them, so it costs nothing extra.
func (t Tester) list(ctx context.Context, folder string, since time.Time, limit int) (refs []mail.MsgRef, matched int, err error) {
	start := time.Now()
	refs, matched, err = t.Mailbox.FetchSince(ctx, folder, since, limit)
	slog.DebugContext(ctx, "mail listed", "account", t.AccountID, "folder", folder, "limit", limit, "matched", matched, "messages", len(refs),
		"duration_ms", time.Since(start).Milliseconds(), "ok", err == nil)
	if err != nil {
		return nil, 0, fmt.Errorf("list %s: %w", folder, err)
	}
	return refs, matched, nil
}

// Run evaluates rs (and the sender rules) against the newest limit messages of folder.
// Rules with an id below 1 are drafts: they take part like saved rules and are reported
// by name only. progress, when set, is called once the mail is listed (0 done) and after
// every message, never concurrently.
// A message that is gone or unreadable by the time it is fetched is left out.
func (t Tester) Run(ctx context.Context, rs []rules.Rule, senders []rules.SenderRule, folder string, limit int, progress func(Progress)) (Result, error) {
	refs, _, err := t.list(ctx, folder, time.Time{}, limit)
	if err != nil {
		return Result{}, err
	}
	slices.Reverse(refs) // newest first
	if progress != nil {
		progress(Progress{Total: len(refs)})
	}
	var (
		res  Result
		mu   sync.Mutex
		done int
		rows = make([]*Row, len(refs))
	)
	err = t.each(ctx, refs, func(ctx context.Context, i int, sum *message.Summary) error {
		var out pipeline.Outcome
		if sum != nil {
			var err error
			if out, err = t.Decider.Settle(ctx, *sum, rs, senders); err != nil {
				return err
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if sum != nil {
			rows[i] = row(sum, out)
		}
		res.ModelCalls += out.Calls
		res.CostUSD += out.Usage.CostUSD
		done++
		if progress != nil {
			progress(Progress{Done: done, Total: len(refs), ModelCalls: res.ModelCalls})
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	res.Rows = make([]Row, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		res.Rows = append(res.Rows, *row)
		if row.Stage != string(rules.StageNone) && !row.Review {
			res.Matched++
		}
	}
	res.Tested = len(res.Rows)
	return res, nil
}

// each reads every message and hands its parsed summary to work, several messages at a
// time, so work is called concurrently. A message that is gone or cannot be parsed is
// handed over as nil. The first failure ends the run and is returned.
func (t Tester) each(ctx context.Context, refs []mail.MsgRef, work func(ctx context.Context, i int, sum *message.Summary) error) error {
	return t.eachRaw(ctx, refs, 0, func(ctx context.Context, i int, raw *message.Raw) error {
		sum, err := t.parse(ctx, raw, t.BodyChars)
		if err != nil {
			return err
		}
		return work(ctx, i, sum)
	})
}

// eachRaw fetches every message with BODY.PEEK, fetchBatch to a request, at most maxBody
// bytes of its text (0 = as much as the connector reads), and hands it to work, several
// messages at a time, so work is called concurrently. A message that is gone is handed over
// as nil. The first failure ends the run and is returned.
func (t Tester) eachRaw(ctx context.Context, refs []mail.MsgRef, maxBody int, work func(ctx context.Context, i int, raw *message.Raw) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var reads readStats
	defer func() { reads.log(ctx, t.AccountID) }()
	type fetched struct {
		i   int
		raw *message.Raw
	}
	var wg sync.WaitGroup
	next := make(chan fetched)
	for range min(workers, len(refs)) {
		wg.Go(func() {
			for f := range next {
				if err := work(ctx, f.i, f.raw); err != nil {
					cancel(err)
				}
			}
		})
	}
feed:
	for lo := 0; lo < len(refs) && ctx.Err() == nil; lo += fetchBatch {
		raws, err := t.fetch(ctx, refs[lo:min(lo+fetchBatch, len(refs))], maxBody, &reads)
		if err != nil {
			cancel(err)
			break
		}
		for k, raw := range raws {
			select {
			case next <- fetched{lo + k, raw}:
			case <-ctx.Done():
				break feed
			}
		}
	}
	close(next)
	wg.Wait()
	return context.Cause(ctx)
}

// fetch reads a batch of messages with BODY.PEEK, lined up with refs. nil means it is gone.
func (t Tester) fetch(ctx context.Context, refs []mail.MsgRef, maxBody int, stats *readStats) ([]*message.Raw, error) {
	start := time.Now()
	raws, err := t.Mailbox.FetchMany(ctx, refs, maxBody)
	took := time.Since(start)
	stats.add(took)
	if took >= slowFetch {
		slog.DebugContext(ctx, "slow mail fetch", "account", t.AccountID, "folder", refs[0].Folder, "messages", len(refs),
			"first_uid", refs[0].UID, "duration_ms", took.Milliseconds(), "ok", err == nil)
	}
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	return raws, nil
}

// parse reads a fetched message as an email, its text cut to bodyChars characters (0 = all
// of it), and fills its contact signals. nil means there was no message, or it cannot be
// read as an email.
func (t Tester) parse(ctx context.Context, raw *message.Raw, bodyChars int) (*message.Summary, error) {
	if raw == nil {
		return nil, nil
	}
	sum, err := message.Parse(raw, t.AccountID, bodyChars)
	if err != nil {
		return nil, nil
	}
	return sum, contacts.Fill(ctx, t.Store, sum)
}

// readStats adds up how long the mail server took to fetch the messages of one run.
type readStats struct {
	n, total, longest atomic.Int64 // fetches, and their nanoseconds in all and for the slowest
}

// add counts one fetch and keeps the longest.
func (s *readStats) add(d time.Duration) {
	s.n.Add(1)
	s.total.Add(int64(d))
	for cur := s.longest.Load(); int64(d) > cur && !s.longest.CompareAndSwap(cur, int64(d)); cur = s.longest.Load() {
	}
}

// log writes the run's fetches as one debug line.
func (s *readStats) log(ctx context.Context, account int64) {
	n := s.n.Load()
	if n == 0 {
		return
	}
	slog.DebugContext(ctx, "mail fetched", "account", account, "fetches", n,
		"total_ms", time.Duration(s.total.Load()).Milliseconds(), "slowest_ms", time.Duration(s.longest.Load()).Milliseconds())
}

// row shapes what was settled for one email.
func row(sum *message.Summary, out pipeline.Outcome) *Row {
	r := &Row{From: sum.From, Subject: sum.Subject, Stage: string(out.Stage), RuleName: out.RuleName, Confidence: out.Confidence,
		Reason: out.Reason, Review: out.Review, Actions: append([]rules.Action{}, out.Actions...), rule: out.RuleID}
	if !sum.ReceivedAt.IsZero() {
		at := sum.ReceivedAt.Unix()
		r.ReceivedAt = &at
	}
	if out.RuleID > 0 {
		r.RuleID = &out.RuleID
	}
	return r
}

// CheckRow is one checked email: how the real flow settled it, kept so a cleanup Sort can
// apply the same answer without asking the model again. Ref says where the email was when
// it was checked; Sort verifies it is still there. The display fields live only in the
// daemon's memory and are never logged or written to the database (MAI-44).
type CheckRow struct {
	Ref        mail.MsgRef
	From       string
	Subject    string
	ReceivedAt int64 // 0 = unknown
	Outcome    pipeline.Outcome
}

// CheckProgress reports how far a check has got. The daemon turns it into a check.progress
// event and into the "N of M checked", model-call and cost totals the screen shows.
type CheckProgress struct {
	Done  int
	Total int // emails this check goes through: at most the newest limit of the range
	// Matched is how many emails the folder holds in the chosen range before the limit,
	// known once the mail is listed. It is more than Total when the limit cut the range.
	Matched    int
	ModelCalls int
	Tokens     int
	CostUSD    float64
}

// Check runs every selected email of folder (the mail received since, newest limit kept)
// through the real flow — sender rules, conditions, then the decision model for rules with
// an intent, exactly as live processing decides — and returns one row per email with what
// was settled, newest first. It moves nothing and records nothing: the model is really
// asked and really paid (the caller books the calls under the "cleanup" purpose), but no
// mailbox is changed and no activity is written. progress, when set, is called once the
// mail is listed (0 of N) and after every email, never concurrently.
func (t Tester) Check(ctx context.Context, rs []rules.Rule, senders []rules.SenderRule, folder string, since time.Time, limit int, progress func(CheckProgress)) ([]CheckRow, error) {
	refs, matched, err := t.list(ctx, folder, since, limit)
	if err != nil {
		return nil, err
	}
	slices.Reverse(refs) // newest first
	if progress != nil {
		progress(CheckProgress{Total: len(refs), Matched: matched})
	}
	var (
		mu    sync.Mutex
		done  int
		calls int
		toks  int
		cost  float64
		rows  = make([]*CheckRow, len(refs))
	)
	err = t.each(ctx, refs, func(ctx context.Context, i int, sum *message.Summary) error {
		var out pipeline.Outcome
		if sum != nil {
			var err error
			if out, err = t.Decider.Settle(ctx, *sum, rs, senders); err != nil {
				return err
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if sum != nil {
			r := CheckRow{Ref: refs[i], From: sum.From, Subject: sum.Subject, Outcome: out}
			if !sum.ReceivedAt.IsZero() {
				r.ReceivedAt = sum.ReceivedAt.Unix()
			}
			rows[i] = &r
		}
		calls += out.Calls
		toks += out.Usage.TokensIn + out.Usage.TokensOut
		cost += out.Usage.CostUSD
		done++
		if progress != nil {
			progress(CheckProgress{Done: done, Total: len(refs), Matched: matched, ModelCalls: calls, Tokens: toks, CostUSD: cost})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]CheckRow, 0, len(rows))
	for _, r := range rows {
		if r != nil {
			out = append(out, *r)
		}
	}
	return out, nil
}
