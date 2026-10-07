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
	// ReasonUnreadable is the decision's reason when the model's answer could not be read
	// after every try, and the email was kept.
	ReasonUnreadable = "The AI's answer could not be read after 3 tries, so the email was left where it is."
	// ReasonOwnMail is the decision's reason for an email sent from the mailbox's own
	// address while the leave_own_mail setting is on (Decider.Own).
	ReasonOwnMail = "Sent from this mailbox's own address, so MailRules left it alone."

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
	// Live, when set, supplies Router and MinConfidence afresh for every message, so a
	// change made in Settings needs no restart (settings.Settings.Live).
	Live func(ctx context.Context) (router *models.Router, minConfidence float64)
	// Override returns the router for a rule's own model (rules.Rule.Model), or nil when
	// that model cannot be used (settings.Settings.RouterFor). nil = rules' models are ignored.
	Override func(ctx context.Context, spec string) *models.Router

	// Batch is the batch the actions belong to. 0 = the day's live batch; a cleanup run
	// sets its own, so the whole run can be undone as one.
	Batch int64
	// Spent, when set, is told what each model decision cost: a cleanup run shows the total.
	Spent func(tokens int, costUSD float64)

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

// Sort runs one message of existing mail through the same steps as new mail: cleanup's
// way in. Unlike Process it does not look at whether the message was seen before (the
// rules may have changed since, or dry-run been switched off), and it leaves the folder's
// watch position alone.
func (p *Pipeline) Sort(ctx context.Context, ref mail.MsgRef) error {
	m, _, err := p.Store.IngestMessage(ctx, ref, p.now().Unix())
	if err != nil {
		return err
	}
	return p.run(ctx, m)
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
	d := Decider{Router: p.Router, Override: p.Override, MinConfidence: p.MinConfidence, Now: now,
		Examples: Corrections(p.Store, p.Account.UserID)}
	if p.Live != nil {
		d.Router, d.MinConfidence = p.Live(ctx)
	}
	if d.Own, err = OwnMail(ctx, p.Store, p.Account); err != nil {
		return err
	}
	out, err := d.Settle(ctx, *sum, rs, senders)
	if err != nil {
		return err
	}
	res := out.Result
	if res.SenderRuleID != 0 {
		if err := p.Store.HitSenderRule(ctx, res.SenderRuleID); err != nil {
			return err
		}
	}
	if out.Asked {
		p.Hub.Publish(events.UsageUpdated, nil)
		u := out.Usage
		if p.Spent != nil {
			p.Spent(u.TokensIn+u.TokensOut, u.CostUSD)
		}
	}
	dec = decisionFrom(out, m.ID, now)
	if err := p.finish(ctx, m, dec, res); err != nil {
		return err
	}
	p.learnFrom(ctx, dec, res, m)
	return nil
}

// decisionFrom shapes the decision row an Outcome implies for a message. It is the one
// place that turns a settled Outcome into a store.Decision, so a live email and a cleanup
// Sort that replay the same Outcome record the same decision. It writes nothing.
func decisionFrom(out Outcome, messageID int64, now time.Time) store.Decision {
	res := out.Result
	d := store.Decision{MessageID: messageID, CreatedAt: now.Unix(), Stage: string(res.Stage), RuleID: res.RuleID,
		RuleVersion: res.RuleVersion, Confidence: res.Confidence, Reason: out.Reason, RuleName: out.RuleName, Probabilities: out.Probabilities}
	if out.Asked {
		u := out.Usage
		d.Model, d.TokensIn, d.TokensOut, d.CostUSD, d.LatencyMS = u.Model, u.TokensIn, u.TokensOut, u.CostUSD, u.Latency.Milliseconds()
	}
	return d
}

// learnFrom teaches the sender index from a model's own confident pick, as the only thing
// that tells us anything new about a sender. Bulk mail that passed DMARC is learned from
// sooner (learn.AfterBulk). A failure is logged and costs the lesson, not the decision.
func (p *Pipeline) learnFrom(ctx context.Context, dec store.Decision, res rules.Result, m *store.Message) {
	if (dec.Stage == string(rules.StageDecider) || dec.Stage == "fallback") && !res.Review && m.FromAddr != "" {
		bulk := m.Signals.Bulk && m.Signals.DMARC == "pass"
		if _, err := learn.Observe(ctx, p.Store, p.Account.UserID, m.FromAddr, bulk, p.now().Unix()); err != nil {
			slog.WarnContext(ctx, "could not update learned sender rules", "account", p.Account.ID, "error", err.Error())
		}
	}
}

// SortSaved applies a decision a cleanup check already settled to one existing email,
// without asking a model: it verifies the email is still where the check found it
// (BODY.PEEK, never marking it read), ingests its row, records the decision carrying the
// check's stage, rule, model, confidence and reason — but no fresh cost, which is on the
// ledger once from the check — and hands the actions to the executor in the run's batch.
// applied is false when the email has moved or gone since the check: it is passed over.
func (p *Pipeline) SortSaved(ctx context.Context, ref mail.MsgRef, out Outcome) (applied bool, err error) {
	raw, err := p.Mailbox.Fetch(ctx, ref, 0)
	if errors.Is(err, mail.ErrNotFound) {
		return false, nil // moved or deleted since the check: skip it, and say so
	}
	if err != nil {
		return false, fmt.Errorf("fetch: %w", err)
	}
	now := p.now()
	m, _, err := p.Store.IngestMessage(ctx, ref, now.Unix())
	if err != nil {
		return false, err
	}
	if sum, perr := message.Parse(raw, m.AccountID, p.BodyChars); perr == nil {
		if err := contacts.Fill(ctx, p.Store, sum); err != nil {
			return false, err
		}
		fill(&m, sum, raw)
		if err := p.Store.SaveMessageSummary(ctx, m); err != nil {
			return false, err
		}
	}
	if out.SenderRuleID != 0 {
		if err := p.Store.HitSenderRule(ctx, out.SenderRuleID); err != nil {
			return false, err
		}
	}
	dec := decisionFrom(out, m.ID, now)
	dec.TokensIn, dec.TokensOut, dec.CostUSD, dec.LatencyMS = 0, 0, 0, 0 // paid once, on the check's ledger
	if err := p.finish(ctx, &m, dec, out.Result); err != nil {
		return false, err
	}
	p.learnFrom(ctx, dec, out.Result, &m)
	return true, nil
}

// finish records the decision, acts on it or parks the message in Needs review, sets the
// final state and publishes the event. A message parked in Needs review is not touched in
// the mailbox at all.
func (p *Pipeline) finish(ctx context.Context, m *store.Message, dec store.Decision, res rules.Result) error {
	var err error
	if dec.ID, err = p.Store.AddDecision(ctx, dec, store.StateDecided); err != nil {
		return err
	}
	rec := actions.DecisionRecord{DecisionID: dec.ID, MessageID: m.ID, Ref: m.Location()}
	state, event := store.StateSkipped, events.MessageProcessed
	var acts []rules.Action
	switch {
	case res.Review:
		state, event = store.StateReview, events.MessageReview
	case len(res.Actions) > 0:
		state, acts = store.StateActed, res.Actions
	}
	if p.Exec != nil && len(acts) > 0 {
		// Live processing shares one batch per day, so "undo today" is one batch undo.
		batch := p.Batch
		if batch == 0 {
			var err error
			if batch, err = p.Store.LiveBatch(ctx, p.now()); err != nil {
				return err
			}
		}
		if _, err := p.Exec.Apply(ctx, rec, acts, batch); err != nil {
			return fmt.Errorf("apply: %w", err)
		}
	}
	if err := p.Store.SetMessageState(ctx, m.ID, state, m.Attempts, 0); err != nil {
		return err
	}
	m.State, m.NextAttemptAt = state, 0
	p.Hub.Publish(event, store.ActivityRow{Message: *m, Decision: &dec})
	return nil
}

// fill copies what is kept of a parsed email onto its row. The body is not: only its
// first snippetChars characters.
func fill(m *store.Message, s *message.Summary, raw *message.Raw) {
	if ids := s.Headers["Message-Id"]; len(ids) > 0 {
		m.MessageID = trimAngles(ids[0])
	}
	m.FromAddr, m.FromName, m.FromDomain, m.ToAddrs, m.Subject, m.ListID = s.From, s.FromName, s.FromDomain, s.To, s.Subject, s.ListID
	m.Snippet = s.Body
	if r := []rune(s.Body); len(r) > snippetChars {
		m.Snippet = string(r[:snippetChars])
	}
	m.ReceivedAt, m.HasAttachment, m.Size = s.ReceivedAt.Unix(), s.HasAttachment, raw.Size
	m.Signals = store.Signals{Bulk: s.IsBulk, Noreply: s.IsNoreply, Contact: s.IsContact, RepliedBefore: s.RepliedBefore, DMARC: s.DMARC,
		ListUnsubscribe: len(s.Headers["List-Unsubscribe"]) > 0}
}

func trimAngles(id string) string {
	if len(id) >= 2 && id[0] == '<' && id[len(id)-1] == '>' {
		return id[1 : len(id)-1]
	}
	return id
}
