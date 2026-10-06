// Package composer holds the two ways a user tries rules out before they act: the rule
// composer, which turns free text into validated draft rules, and the rule tester, which
// runs saved or draft rules over recent mail. Neither saves anything, and neither can
// change a mailbox: all they are given of one is Reader.
package composer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/contacts"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/message"
	"github.com/TurtleByte-IN/mailrules/internal/pipeline"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// Reader is the read-only part of mail.Mailbox. Fetch reads with BODY.PEEK, so a tested
// message stays unread; there is no method here that could move or flag one.
type Reader interface {
	Fetch(ctx context.Context, ref mail.MsgRef, maxBody int) (*message.Raw, error)
	FetchSince(ctx context.Context, folder string, since time.Time, limit int) ([]mail.MsgRef, error)
}

// Limits of one test run.
const (
	DefaultLimit = 200
	MaxLimit     = 2000
	// workers is how many messages are in flight at once; the model calls among them
	// are capped again, daemon-wide, by models.Caller.
	workers = 8
)

// Tester runs rules over the newest mail of one folder and reports what each email would
// get. Bodies are held in memory for the length of one message's evaluation only.
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

// Run evaluates rs (and the sender rules) against the newest limit messages of folder.
// Rules with an id below 1 are drafts: they take part like saved rules and are reported
// by name only. progress, when set, is called after every message, never concurrently.
// A message that is gone or unreadable by the time it is fetched is left out.
func (t Tester) Run(ctx context.Context, rs []rules.Rule, senders []rules.SenderRule, folder string, limit int, progress func(done, total int)) (Result, error) {
	refs, err := t.Mailbox.FetchSince(ctx, folder, time.Time{}, limit)
	if err != nil {
		return Result{}, fmt.Errorf("list %s: %w", folder, err)
	}
	slices.Reverse(refs) // newest first

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var (
		res  Result
		mu   sync.Mutex
		wg   sync.WaitGroup
		done int
		rows = make([]*Row, len(refs))
		next = make(chan int)
	)
	for range min(workers, len(refs)) {
		wg.Go(func() {
			for i := range next {
				row, out, err := t.one(ctx, refs[i], rs, senders)
				mu.Lock()
				if err != nil {
					cancel(err) // the first failure ends the run
				}
				rows[i] = row
				res.ModelCalls += out.Calls
				res.CostUSD += out.Usage.CostUSD
				done++
				if progress != nil && err == nil {
					progress(done, len(refs))
				}
				mu.Unlock()
			}
		})
	}
feed:
	for i := range refs {
		select {
		case next <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(next)
	wg.Wait()
	if err := context.Cause(ctx); err != nil {
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

// one settles a single message. A nil row means it could not be read and is left out.
func (t Tester) one(ctx context.Context, ref mail.MsgRef, rs []rules.Rule, senders []rules.SenderRule) (*Row, pipeline.Outcome, error) {
	raw, err := t.Mailbox.Fetch(ctx, ref, 0)
	if errors.Is(err, mail.ErrNotFound) {
		return nil, pipeline.Outcome{}, nil
	}
	if err != nil {
		return nil, pipeline.Outcome{}, fmt.Errorf("fetch: %w", err)
	}
	sum, err := message.Parse(raw, t.AccountID, t.BodyChars)
	if err != nil {
		return nil, pipeline.Outcome{}, nil
	}
	if err := contacts.Fill(ctx, t.Store, sum); err != nil {
		return nil, pipeline.Outcome{}, err
	}
	out, err := t.Decider.Settle(ctx, *sum, rs, senders)
	if err != nil {
		return nil, out, err
	}
	row := &Row{From: sum.From, Subject: sum.Subject, Stage: string(out.Stage), RuleName: out.RuleName, Confidence: out.Confidence,
		Reason: out.Reason, Review: out.Review, Actions: append([]rules.Action{}, out.Actions...), rule: out.RuleID}
	if !sum.ReceivedAt.IsZero() {
		at := sum.ReceivedAt.Unix()
		row.ReceivedAt = &at
	}
	if out.RuleID > 0 {
		row.RuleID = &out.RuleID
	}
	return row, out, nil
}
