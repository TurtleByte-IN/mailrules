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
	"slices"
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
	"github.com/TurtleByte-IN/mailrules/internal/unsubscribe"
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
	// ReasonAllowance is the Needs review reason of an email a model was needed for while
	// the tenant's monthly allowance of AI-sorted emails is used up (Decider.Allowance).
	ReasonAllowance = "This month's AI-sorted emails are used up, so MailRules left this one for you."

	snippetChars = 200
)

// ErrHalted is what the pipeline answers, sorting nothing, while the tenant's plan has
// ended (Pipeline.Halted).
var ErrHalted = errors.New("the plan has ended, so MailRules sorts nothing")

// Executor is the part of actions.Executor the pipeline needs.
type Executor interface {
	Apply(ctx context.Context, d actions.DecisionRecord, acts []rules.Action, batchID int64) ([]actions.ActionRecord, error)
	ApplyMany(ctx context.Context, ms []actions.Many, a rules.Action, batchID int64) (dry bool, errs []error)
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
	// Live, when set, supplies Router and MinConfidence afresh for every message from the
	// settings of the account's tenant, so a change made in Settings needs no restart
	// (settings.Settings.Live).
	Live func(ctx context.Context, tenantID int64) (router *models.Router, minConfidence float64)
	// Override returns the tenant's router for a rule's own model (rules.Rule.Model), or nil
	// when that model cannot be used (settings.Settings.RouterFor). nil = rules' models are ignored.
	Override func(ctx context.Context, tenantID int64, spec string) *models.Router

	// Batch is the batch the actions belong to. 0 = the day's live batch; a cleanup run
	// sets its own, so the whole run can be undone as one.
	Batch int64
	// Spent, when set, is told what each model decision cost: a cleanup run shows the total.
	Spent func(tokens int, costUSD float64)
	// Allowance, when set, says whether a tenant's monthly allowance of emails a model
	// decides on keys the operator holds is used up at now (Decider.Allowance). An email
	// counted already (a message tried again) is decided as before. nil = no allowance.
	Allowance func(ctx context.Context, tenantID int64, now time.Time) (reached bool, err error)
	// Halted, when set, says whether the tenant's mail must not be sorted at now: its plan
	// ended (a build that sells plans, ext.Module.Billing). While it is, Process, Sort,
	// SortSaved and RetryDue leave every message as it is, the folder's watch position
	// included, and answer ErrHalted; the supervisor then stops. Mail that arrived meanwhile
	// is sorted when the mailbox starts again with a plan, as after a pause: the watcher
	// resumes from the stored position. nil = never halted.
	Halted func(ctx context.Context, tenantID int64, now time.Time) (bool, error)

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
// before is not processed again, unless an earlier run was cut off before it finished, and
// neither is a known email that came back into the folder under a new UID (cameBack).
// The folder's last processed UID advances only once the message's row is committed; on an
// error (the daemon is stopping, or the database failed) it does not, so the watcher sends
// the message again after a restart.
func (p *Pipeline) Process(ctx context.Context, ref mail.MsgRef) error {
	if err := p.halted(ctx); err != nil {
		return err
	}
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
	if err := p.halted(ctx); err != nil {
		return err
	}
	m, _, err := p.Store.IngestMessage(ctx, ref, p.now().Unix())
	if err != nil {
		return err
	}
	return p.run(ctx, m)
}

// RetryDue is the retry job: it re-runs every failed message of the account whose wait is
// over, and returns how many it ran.
func (p *Pipeline) RetryDue(ctx context.Context) (int, error) {
	if err := p.halted(ctx); err != nil {
		return 0, err
	}
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

// halted answers ErrHalted while the tenant's mail must not be sorted (Halted), or the
// error that kept it from finding out.
func (p *Pipeline) halted(ctx context.Context) error {
	if p.Halted == nil {
		return nil
	}
	h, err := p.Halted(ctx, p.Account.TenantID, p.now())
	if err != nil {
		return fmt.Errorf("check the tenant's plan: %w", err)
	}
	if h {
		return ErrHalted
	}
	return nil
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
		p.Hub.Publish(p.Account.TenantID, m.AccountID, events.MessageReview, store.ActivityRow{Message: m, Decision: &d})
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
	if back, err := p.cameBack(ctx, m); err != nil || back {
		return err
	}
	if err := p.Store.SaveMessageSummary(ctx, *m); err != nil {
		return err
	}

	rs, err := p.Store.Rules(ctx, p.Account.TenantID)
	if err != nil {
		return err
	}
	senders, err := p.Store.SenderRules(ctx, p.Account.TenantID)
	if err != nil {
		return err
	}
	d := Decider{Router: p.Router, MinConfidence: p.MinConfidence, Now: now, Examples: Corrections(p.Store, p.Account.Owner())}
	if p.Override != nil {
		tenant := p.Account.TenantID
		d.Override = func(ctx context.Context, spec string) *models.Router { return p.Override(ctx, tenant, spec) }
	}
	if p.Live != nil {
		d.Router, d.MinConfidence = p.Live(ctx, p.Account.TenantID)
	}
	if d.Own, err = OwnMail(ctx, p.Store, p.Account); err != nil {
		return err
	}
	if p.Allowance != nil && !m.Metered {
		tenant := p.Account.TenantID
		d.Allowance = func(ctx context.Context) (bool, error) { return p.Allowance(ctx, tenant, now) }
	}
	out, err := d.Settle(ctx, *sum, rs, senders)
	if err != nil {
		return err
	}
	res := out.Result
	if out.Asked {
		p.Hub.Publish(p.Account.TenantID, 0, events.UsageUpdated, nil)
		u := out.Usage
		if p.Spent != nil {
			p.Spent(u.TokensIn+u.TokensOut, u.CostUSD)
		}
	}
	return p.finish(ctx, m, decisionFrom(out, m.ID, now), res)
}

// cameBack reports whether m, a row MailRules has done nothing with yet, is an email it
// already has another row for: the same Message-ID, and no longer where that row last saw
// it. Such an email was moved back into the folder outside MailRules (by the user or
// another mail client), which gave it a new UID. It is not new mail: the known row now
// points here, m goes, and the email is not decided again, so it never waits twice in
// Needs review and mail the user brought back is not sorted away again. A second copy,
// with the first still in place, is new mail; so is m when it already has a decision.
func (p *Pipeline) cameBack(ctx context.Context, m *store.Message) (bool, error) {
	if m.MessageID == "" {
		return false, nil
	}
	known, err := p.Store.MessageByMessageID(ctx, m.AccountID, m.MessageID, m.ID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := p.Mailbox.Flags(ctx, known.Location()); err == nil {
		return false, nil // the first copy is still where it was: this is another one
	} else if !errors.Is(err, mail.ErrNotFound) && !errors.Is(err, mail.ErrNoFolder) {
		return false, fmt.Errorf("look for the earlier copy: %w", err)
	}
	back, err := p.Store.Rebind(ctx, m.ID, known.ID, m.Location())
	if back {
		slog.InfoContext(ctx, "a known email came back under a new UID", "account", m.AccountID, "message", known.ID, "uid", m.UID)
	}
	return back, err
}

// decisionFrom shapes the decision row an Outcome implies for a message. It is the one
// place that turns a settled Outcome into a store.Decision, so a live email and a cleanup
// Sort that replay the same Outcome record the same decision. It writes nothing.
func decisionFrom(out Outcome, messageID int64, now time.Time) store.Decision {
	res := out.Result
	d := store.Decision{MessageID: messageID, CreatedAt: now.Unix(), Stage: string(res.Stage), RuleID: res.RuleID,
		RuleVersion: res.RuleVersion, Confidence: res.Confidence, Reason: out.Reason, RuleName: out.RuleName, Probabilities: out.Probabilities,
		SenderSource: res.SenderSource, Metered: out.Metered}
	if out.Asked {
		u := out.Usage
		d.Model, d.TokensIn, d.TokensOut, d.CostUSD, d.LatencyMS = u.Model, u.TokensIn, u.TokensOut, u.CostUSD, u.Latency.Milliseconds()
	}
	return d
}

// learnFrom teaches the sender index from a model's own confident pick, as the only thing
// that tells us anything new about a sender. Bulk mail that passed DMARC is learned from
// sooner (learn.AfterBulk). A failure is logged and costs the lesson, not the decision.
// It is not called for a decision whose actions were recorded in dry-run: dry-run changes
// nothing that would change what MailRules does later, so no sender is learned from it.
func (p *Pipeline) learnFrom(ctx context.Context, dec store.Decision, res rules.Result, m *store.Message) {
	if (dec.Stage == string(rules.StageDecider) || dec.Stage == "fallback") && !res.Review && m.FromAddr != "" {
		bulk := m.Signals.Bulk && m.Signals.DMARC == "pass"
		if _, err := learn.Observe(ctx, p.Store, p.Account.Owner(), m.FromAddr, bulk, p.now().Unix()); err != nil {
			slog.WarnContext(ctx, "could not update learned sender rules", "account", p.Account.ID, "error", err.Error())
		}
	}
}

// Saved is one email a cleanup check settled: where the check found it, and its answer.
type Saved struct {
	Ref mail.MsgRef
	Out Outcome
}

// SortSaved applies decisions a cleanup check already settled to existing emails of one
// folder (one UIDVALIDITY), without asking a model: it verifies they are still where the
// check found them with one fetch for all of them (BODY.PEEK, never marking them read),
// ingests their rows, records each decision carrying the check's stage, rule, model,
// confidence and reason — but no fresh cost, which is on the ledger once from the check —
// and hands the actions to the executor in the run's batch: the emails whose one action is
// the same move go to the server together (Executor.ApplyMany), any other email on its own.
// applied[i] is false when email i has moved or gone since the check: it is passed over.
// An error is the first email whose actions failed or whose outcome could not be recorded;
// what the others did is on record.
func (p *Pipeline) SortSaved(ctx context.Context, saved []Saved) (applied []bool, err error) {
	if err := p.halted(ctx); err != nil {
		return nil, err
	}
	refs := make([]mail.MsgRef, len(saved))
	for i, s := range saved {
		refs[i] = s.Ref
	}
	raws, err := p.Mailbox.FetchMany(ctx, refs, 0)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	now := p.now()
	type email struct {
		m   store.Message
		dec store.Decision
		res rules.Result
		err error
	}
	applied = make([]bool, len(saved))
	emails := make([]*email, len(saved))
	moves := map[rules.Action][]int{} // emails whose one action is that move
	var order []rules.Action
	for i, sv := range saved {
		raw := raws[i]
		if raw == nil {
			continue // moved or deleted since the check: skip it, and say so
		}
		m, _, err := p.Store.IngestMessage(ctx, refs[i], now.Unix())
		if err != nil {
			return applied, err
		}
		if sum, perr := message.Parse(raw, m.AccountID, p.BodyChars); perr == nil {
			if err := contacts.Fill(ctx, p.Store, sum); err != nil {
				return applied, err
			}
			fill(&m, sum, raw)
			if err := p.Store.SaveMessageSummary(ctx, m); err != nil {
				return applied, err
			}
		}
		e := &email{m: m, res: sv.Out.Result, dec: decisionFrom(sv.Out, m.ID, now)}
		e.dec.TokensIn, e.dec.TokensOut, e.dec.CostUSD, e.dec.LatencyMS = 0, 0, 0, 0 // paid once, on the check's ledger
		e.dec.Metered = false                                                        // counted once, by the check
		emails[i] = e
		acts := actsOf(e.res)
		if p.Exec == nil || !actions.IsMove(acts) {
			e.err = p.finish(ctx, &e.m, e.dec, e.res)
			continue
		}
		if e.dec.ID, err = p.Store.AddDecision(ctx, e.dec, store.StateDecided); err != nil {
			return applied, err
		}
		if moves[acts[0]] == nil {
			order = append(order, acts[0])
		}
		moves[acts[0]] = append(moves[acts[0]], i)
	}
	if len(order) > 0 {
		batch, err := p.batch(ctx)
		if err != nil {
			return applied, err
		}
		for _, a := range order {
			ms := make([]actions.Many, len(moves[a]))
			for k, i := range moves[a] {
				e := emails[i]
				ms[k] = actions.Many{DecisionRecord: actions.DecisionRecord{DecisionID: e.dec.ID, MessageID: e.m.ID, Ref: e.m.Location()}, Flags: raws[i].Flags}
			}
			dry, errs := p.Exec.ApplyMany(ctx, ms, a, batch)
			for k, i := range moves[a] {
				e := emails[i]
				if e.err = errs[k]; e.err != nil {
					e.err = fmt.Errorf("apply: %w", e.err)
					continue
				}
				e.err = p.conclude(ctx, &e.m, e.dec, e.res, dry)
			}
		}
	}
	for i, e := range emails {
		if e == nil {
			continue
		}
		if e.err != nil {
			return applied, e.err
		}
		applied[i] = true
	}
	return applied, nil
}

// finish records the decision, acts on it or parks the message in Needs review, and
// settles the message (conclude). A message parked in Needs review is not touched in the
// mailbox at all.
func (p *Pipeline) finish(ctx context.Context, m *store.Message, dec store.Decision, res rules.Result) (err error) {
	if dec.ID, err = p.Store.AddDecision(ctx, dec, store.StateDecided); err != nil {
		return err
	}
	dry := false
	if acts := actsOf(res); p.Exec != nil && len(acts) > 0 {
		batch, err := p.batch(ctx)
		if err != nil {
			return err
		}
		recs, err := p.Exec.Apply(ctx, actions.DecisionRecord{DecisionID: dec.ID, MessageID: m.ID, Ref: m.Location()}, acts, batch)
		if err != nil {
			return fmt.Errorf("apply: %w", err)
		}
		dry = slices.ContainsFunc(recs, func(a actions.ActionRecord) bool { return a.Status == store.ActionDryRun })
	}
	return p.conclude(ctx, m, dec, res, dry)
}

// actsOf is what a decision's actions are once it is settled: none while it waits in Needs
// review.
func actsOf(res rules.Result) []rules.Action {
	if res.Review {
		return nil
	}
	return res.Actions
}

// batch is the batch the actions belong to: the run's own, or else the day's live batch, so
// "undo today" is one batch undo.
func (p *Pipeline) batch(ctx context.Context) (int64, error) {
	if p.Batch != 0 {
		return p.Batch, nil
	}
	return p.Store.LiveBatch(ctx, p.Account.TenantID, p.now())
}

// conclude settles a decided message once its actions ran, and only then publishes the
// event, so whoever sees the message settled sees all it changed: a model's confident pick
// teaches the sender index first, and the sender rule that settled it counts one more hit in
// the same write that sets the message's final state. Neither happens when the actions
// were only recorded in dry-run (dry): dry-run changes nothing that persists as if the
// actions had been carried out.
func (p *Pipeline) conclude(ctx context.Context, m *store.Message, dec store.Decision, res rules.Result, dry bool) error {
	state, event := store.StateSkipped, events.MessageProcessed
	switch {
	case res.Review:
		state, event = store.StateReview, events.MessageReview
	case len(res.Actions) > 0:
		state = store.StateActed
	}
	var hit int64
	if !dry {
		p.learnFrom(ctx, dec, res, m)
		hit = res.SenderRuleID
	}
	if err := p.Store.SettleMessage(ctx, m.ID, state, m.Attempts, hit); err != nil {
		return err
	}
	m.State, m.NextAttemptAt = state, 0
	p.Hub.Publish(p.Account.TenantID, m.AccountID, event, store.ActivityRow{Message: *m, Decision: &dec})
	return nil
}

// fill copies what is kept of a parsed email onto its row. The body is not: only its
// first snippetChars characters. Nor are the headers, except the one-click unsubscribe link
// they offer, which Senders calls when the person asks.
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
	m.UnsubscribeURL = unsubscribe.OneClick(s.Headers)
}

func trimAngles(id string) string {
	if len(id) >= 2 && id[0] == '<' && id[len(id)-1] == '>' {
		return id[1 : len(id)-1]
	}
	return id
}
