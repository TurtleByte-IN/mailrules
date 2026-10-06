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
			progress(done, len(refs))
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
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var wg sync.WaitGroup
	next := make(chan int)
	for range min(workers, len(refs)) {
		wg.Go(func() {
			for i := range next {
				sum, err := t.read(ctx, refs[i])
				if err == nil {
					err = work(ctx, i, sum)
				}
				if err != nil {
					cancel(err)
				}
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
	return context.Cause(ctx)
}

// read fetches one message with BODY.PEEK and parses it. nil means it is gone or cannot
// be read as an email.
func (t Tester) read(ctx context.Context, ref mail.MsgRef) (*message.Summary, error) {
	raw, err := t.Mailbox.Fetch(ctx, ref, 0)
	if errors.Is(err, mail.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	sum, err := message.Parse(raw, t.AccountID, t.BodyChars)
	if err != nil {
		return nil, nil
	}
	return sum, contacts.Fill(ctx, t.Store, sum)
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

// Preview outcomes: what a cleanup run would do with a group of emails.
const (
	OutcomeRule   = "rule"   // a rule or sender rule applies, settled without a model
	OutcomeNone   = "none"   // no rule matches: the email stays where it is
	OutcomeModel  = "model"  // rules with an intent are in play: the decision model decides during the run
	OutcomeReview = "review" // it needs the decision model and none is set: it would wait in Needs review
)

// Group is the emails of a cleanup preview that share an outcome. Its JSON is one entry
// of the contract's CleanupPreview.groups.
type Group struct {
	Outcome  string `json:"outcome"`
	RuleID   *int64 `json:"rule_id"`
	RuleName string `json:"rule_name"` // for a sender rule without a rule: "Sender rule: keep" or "Sender rule: trash"
	Count    int    `json:"count"`
	Samples  []Row  `json:"samples"` // the newest few
}

// Preview is what a cleanup run would do, as far as it can be told without asking a model.
type Preview struct {
	Total      int     `json:"total"`
	Groups     []Group `json:"groups"` // biggest first
	ModelCalls int     `json:"estimated_model_calls"`
	CostUSD    float64 `json:"estimated_cost_usd"` // filled in by the caller, who knows what a call costs
}

// Preview counts, per outcome, what a cleanup of folder would do with the mail received
// since (zero = all of it; a positive limit keeps the newest limit). It asks no model:
// what sender rules and conditions settle is counted under its rule, and the emails that
// rules with an intent compete for are counted as the model calls the run will make.
func (t Tester) Preview(ctx context.Context, rs []rules.Rule, senders []rules.SenderRule, folder string, since time.Time, limit int) (Preview, error) {
	refs, err := t.Mailbox.FetchSince(ctx, folder, since, limit)
	if err != nil {
		return Preview{}, fmt.Errorf("list %s: %w", folder, err)
	}
	slices.Reverse(refs) // newest first, so the samples are the newest
	names := map[int64]string{}
	for _, r := range rs {
		names[r.ID] = r.Name
	}
	type key struct {
		outcome string
		rule    int64
		name    string
	}
	rows := make([]*Row, len(refs))
	keys := make([]key, len(refs))
	var mu sync.Mutex
	err = t.each(ctx, refs, func(_ context.Context, i int, sum *message.Summary) error {
		if sum == nil {
			return nil
		}
		// Settle with no model at all: it answers "review" exactly for the mail that needs one.
		out, err := pipeline.Decider{MinConfidence: t.Decider.MinConfidence, Now: t.Decider.Now}.Settle(ctx, *sum, rs, senders)
		if err != nil {
			return err
		}
		k := key{OutcomeRule, out.RuleID, out.RuleName}
		switch {
		case out.Review && t.Decider.Router != nil:
			k = key{outcome: OutcomeModel}
			out.Result, out.Reason = rules.Result{Stage: rules.StageDecider}, "The decision model decides this during the run"
		case out.Review:
			k = key{outcome: OutcomeReview}
		case out.Stage == rules.StageNone:
			k = key{outcome: OutcomeNone}
		case out.RuleID == 0: // a sender rule that keeps or blocks
			k.name = out.Reason
		}
		mu.Lock()
		defer mu.Unlock()
		rows[i], keys[i] = row(sum, out), k
		return nil
	})
	if err != nil {
		return Preview{}, err
	}
	var p Preview
	at := map[key]int{}
	for i, r := range rows {
		if r == nil {
			continue
		}
		k := keys[i]
		j, ok := at[k]
		if !ok {
			j, at[k] = len(p.Groups), len(p.Groups)
			g := Group{Outcome: k.outcome, RuleName: k.name, Samples: []Row{}}
			if k.rule > 0 {
				g.RuleID = &k.rule
			}
			p.Groups = append(p.Groups, g)
		}
		g := &p.Groups[j]
		g.Count++
		if len(g.Samples) < maxSamples {
			g.Samples = append(g.Samples, *r)
		}
		p.Total++
		if k.outcome == OutcomeModel {
			p.ModelCalls++
		}
	}
	slices.SortStableFunc(p.Groups, func(a, b Group) int { return b.Count - a.Count })
	if p.Groups == nil {
		p.Groups = []Group{}
	}
	return p, nil
}
