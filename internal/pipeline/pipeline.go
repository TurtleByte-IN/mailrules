// Package pipeline takes one new email from arrival to outcome: ingest (once), fetch and
// parse, evaluate the rules, ask the decision model when rules with an intent are in
// play, hand the winning rule's actions to the executor or park the email in Needs review,
// record it all, and learn. It never changes a mailbox itself.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/actions"
	"github.com/TurtleByte-IN/mailrules/internal/contacts"
	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/learn"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/message"
	"github.com/TurtleByte-IN/mailrules/internal/models"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

const (
	// RetryEvery is how long a failed message waits before the retry job runs it again.
	RetryEvery = 5 * time.Minute
	// MaxRetries is how many times the retry job tries before the message goes to Needs review.
	MaxRetries = 6
	// ReasonNoModel is the Needs review reason while no decision model is configured.
	ReasonNoModel = "No decision model is set"

	snippetChars = 200
)

// Executor is the part of actions.Executor the pipeline needs.
type Executor interface {
	Apply(ctx context.Context, d actions.DecisionRecord, acts []rules.Action, batchID int64) ([]actions.ActionRecord, error)
}

// Pipeline processes the mail of one account. It is not safe for concurrent use: the
// account's supervisor calls it from one goroutine, which keeps mail in UID order.
type Pipeline struct {
	Store   *store.Store
	Mailbox mail.Mailbox
	Account store.Account
	Router  *models.Router // nil = no decision model is set
	Exec    Executor       // nil = record decisions only
	Hub     *events.Hub    // nil = publish nothing

	MinConfidence float64 // act threshold for rules that set none
	BodyChars     int     // plain-text characters shown to models

	Now func() time.Time // nil = time.Now
	// Rediscover re-runs folder discovery. It is called once when a step fails because a
	// folder is unknown, and the step is then tried again. nil = never.
	Rediscover func(ctx context.Context) error
}

func (p *Pipeline) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Process handles one message the watcher reported. It is idempotent: a message seen
// before is not processed again, unless an earlier run was cut off before it finished.
// The folder's last processed UID advances only once the message's row is committed; on an
// error (the daemon is stopping, or the database failed) it does not, so the watcher sends
// the message again after a restart.
func (p *Pipeline) Process(ctx context.Context, ref mail.MsgRef) error {
	m, fresh, err := p.Store.IngestMessage(ctx, ref, p.now().Unix())
	if err != nil {
		return err
	}
	if fresh || m.State == store.StateNew || m.State == store.StateDecided {
		if err := p.run(ctx, m); err != nil {
			return err
		}
	}
	return p.Store.AdvanceFolder(ctx, ref)
}

// RetryDue is the retry job: it re-runs every failed message of the account whose wait is
// over, and returns how many it ran.
func (p *Pipeline) RetryDue(ctx context.Context) (int, error) {
	due, err := p.Store.DueMessages(ctx, p.Account.ID, p.now().Unix())
	if err != nil {
		return 0, err
	}
	for i, m := range due {
		m.Attempts++
		if err := p.run(ctx, m); err != nil {
			return i, err
		}
	}
	return len(due), nil
}

// run makes one attempt and records a failure. It returns an error only when the outcome
// could not be recorded at all.
func (p *Pipeline) run(ctx context.Context, m store.Message) error {
	err := p.attempt(ctx, &m)
	if errors.Is(err, mail.ErrNoFolder) && p.Rediscover != nil && p.Rediscover(ctx) == nil {
		err = p.attempt(ctx, &m) // unknown folder: discovery ran once, try once more
	}
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return ctx.Err() // stopping is not a failure of this message
	}
	slog.WarnContext(ctx, "could not process a message", "account", m.AccountID, "message", m.ID, "retries", m.Attempts, "error", err.Error())
	switch {
	case errors.Is(err, mail.ErrUnsupported):
		// The server cannot do this at all, so trying again cannot help.
		return p.Store.SetMessageState(ctx, m.ID, store.StateError, m.Attempts, 0)
	case m.Attempts >= MaxRetries:
		d := store.Decision{MessageID: m.ID, Stage: string(rules.StageNone), CreatedAt: p.now().Unix(),
			Reason: fmt.Sprintf("Gave up after %d retries: %v", m.Attempts, err)}
		if d.ID, err = p.Store.AddDecision(ctx, d, store.StateReview); err != nil {
			return err
		}
		if err := p.Store.SetMessageState(ctx, m.ID, store.StateReview, m.Attempts, 0); err != nil {
			return err
		}
		m.State, m.NextAttemptAt = store.StateReview, 0
		p.Hub.Publish(events.MessageReview, store.ActivityRow{Message: m, Decision: &d})
		return nil
	}
	return p.Store.SetMessageState(ctx, m.ID, store.StateError, m.Attempts, p.now().Add(RetryEvery).Unix())
}

// attempt runs the per-email steps once. An error means "try again later".
func (p *Pipeline) attempt(ctx context.Context, m *store.Message) error {
	now := p.now()
	raw, err := p.Mailbox.Fetch(ctx, m.Location(), 0)
	if errors.Is(err, mail.ErrNotFound) {
		// Deleted or filed by the user before we got to it: nothing left to sort.
		return p.Store.SetMessageState(ctx, m.ID, store.StateSkipped, m.Attempts, 0)
	}
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	dec := store.Decision{MessageID: m.ID, Stage: string(rules.StageNone), CreatedAt: now.Unix()}
	sum, err := message.Parse(raw, m.AccountID, p.BodyChars)
	if err != nil {
		dec.Reason = "This message could not be read"
		return p.finish(ctx, m, dec, rules.Result{Review: true})
	}
	if err := contacts.Fill(ctx, p.Store, sum); err != nil {
		return err
	}
	fill(m, sum, raw)
	if err := p.Store.SaveMessageSummary(ctx, *m); err != nil {
		return err
	}

	rs, err := p.Store.Rules(ctx, p.Account.UserID)
	if err != nil {
		return err
	}
	senders, err := p.Store.SenderRules(ctx, p.Account.UserID)
	if err != nil {
		return err
	}
	names := map[int64]string{}
	for _, r := range rs {
		names[r.ID] = r.Name
	}
	ev := rules.Evaluate(*sum, rs, senders, rules.Options{MinConfidence: p.MinConfidence, Now: now})

	var res rules.Result
	switch {
	case ev.Final != nil:
		res = *ev.Final
		dec.Reason = localReason(res, names)
	case p.Router == nil:
		res = rules.Result{Stage: rules.StageNone, Review: true}
		dec.Reason = ReasonNoModel
	default:
		req := models.DecideRequest{Email: *sum}
		for _, r := range ev.Candidates {
			req.Candidates = append(req.Candidates, models.Candidate{RuleID: r.ID, Name: r.Name, Intent: r.Intent, Exceptions: r.Exceptions.Text()})
		}
		routed, err := p.Router.Route(ctx, req)
		if err != nil {
			return fmt.Errorf("decide: %w", err)
		}
		p.Hub.Publish(events.UsageUpdated, nil)
		res = ev.Resolve(routed.RuleID, routed.Confidence)
		dec.Reason = routed.Reason
		if res.Stage == rules.StageCondition {
			dec.Reason += fmt.Sprintf("; the default rule %q applied", names[res.RuleID])
		}
		used := routed.Primary
		if routed.Fallback != nil {
			used = *routed.Fallback
			used.TokensIn += routed.Primary.TokensIn
			used.TokensOut += routed.Primary.TokensOut
			used.CostUSD += routed.Primary.CostUSD
			used.Latency += routed.Primary.Latency
			if res.Stage == rules.StageDecider {
				res.Stage = "fallback" // the stage only the pipeline knows (see rules.Stage)
			}
		}
		dec.Model, dec.TokensIn, dec.TokensOut, dec.CostUSD, dec.LatencyMS = used.Model, used.TokensIn, used.TokensOut, used.CostUSD, used.Latency.Milliseconds()
	}
	dec.Stage, dec.RuleID, dec.RuleVersion, dec.Confidence = string(res.Stage), res.RuleID, res.RuleVersion, res.Confidence
	if err := p.finish(ctx, m, dec, res); err != nil {
		return err
	}

	// Learn: only a model's own confident picks teach anything about a sender.
	if (dec.Stage == string(rules.StageDecider) || dec.Stage == "fallback") && !res.Review && sum.From != "" {
		if _, err := learn.Observe(ctx, p.Store, p.Account.UserID, sum.From, now.Unix()); err != nil {
			slog.WarnContext(ctx, "could not update learned sender rules", "account", m.AccountID, "error", err.Error())
		}
	}
	return nil
}

// finish records the decision, acts on it or parks the message in Needs review, sets the
// final state and publishes the event.
func (p *Pipeline) finish(ctx context.Context, m *store.Message, dec store.Decision, res rules.Result) error {
	var err error
	if dec.ID, err = p.Store.AddDecision(ctx, dec, store.StateDecided); err != nil {
		return err
	}
	rec := actions.DecisionRecord{DecisionID: dec.ID, MessageID: m.ID, Ref: m.Location()}
	state, event := store.StateSkipped, events.MessageProcessed
	switch {
	case res.Review:
		state, event = store.StateReview, events.MessageReview
		if p.Exec != nil {
			// The keyword only helps mail clients show the message; review works without it.
			if _, err := p.Exec.Apply(ctx, rec, []rules.Action{{Type: actions.KindReview}}, 0); err != nil {
				slog.WarnContext(ctx, "could not tag a message for review", "account", m.AccountID, "message", m.ID, "error", err.Error())
			}
		}
	case len(res.Actions) > 0:
		state = store.StateActed
		if p.Exec != nil {
			if _, err := p.Exec.Apply(ctx, rec, res.Actions, 0); err != nil {
				return fmt.Errorf("apply: %w", err)
			}
		}
	}
	if err := p.Store.SetMessageState(ctx, m.ID, state, m.Attempts, 0); err != nil {
		return err
	}
	m.State, m.NextAttemptAt = state, 0
	p.Hub.Publish(event, store.ActivityRow{Message: *m, Decision: &dec})
	return nil
}

// localReason words a result no model was asked about.
func localReason(res rules.Result, names map[int64]string) string {
	switch {
	case res.Stage == rules.StageNone:
		return "No rule matched"
	case res.Stage == rules.StageSender && res.RuleID == 0:
		return "Sender rule: " + res.Actions[0].Type // keep, or trash for a blocked sender
	case res.Stage == rules.StageSender:
		return fmt.Sprintf("Sender rule: %q", names[res.RuleID])
	}
	return fmt.Sprintf("Matched %q by its conditions", names[res.RuleID])
}

// fill copies what is kept of a parsed email onto its row. The body is not: only its
// first snippetChars characters.
func fill(m *store.Message, s *message.Summary, raw *message.Raw) {
	if ids := s.Headers["Message-Id"]; len(ids) > 0 {
		m.MessageID = trimAngles(ids[0])
	}
	m.FromAddr, m.FromDomain, m.ToAddrs, m.Subject, m.ListID = s.From, s.FromDomain, s.To, s.Subject, s.ListID
	m.Snippet = s.Body
	if r := []rune(s.Body); len(r) > snippetChars {
		m.Snippet = string(r[:snippetChars])
	}
	m.ReceivedAt, m.HasAttachment, m.Size = s.ReceivedAt.Unix(), s.HasAttachment, raw.Size
	m.Signals = store.Signals{Bulk: s.IsBulk, Noreply: s.IsNoreply, Contact: s.IsContact, RepliedBefore: s.RepliedBefore, DMARC: s.DMARC}
}

func trimAngles(id string) string {
	if len(id) >= 2 && id[0] == '<' && id[len(id)-1] == '>' {
		return id[1 : len(id)-1]
	}
	return id
}
