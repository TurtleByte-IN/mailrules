package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/learn"
	"github.com/TurtleByte-IN/mailrules/internal/message"
	"github.com/TurtleByte-IN/mailrules/internal/models"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// ErrModel marks a failure of the decision model, as opposed to the mailbox or the database.
var ErrModel = errors.New("the decision model failed")

// Decider settles which rule applies to one email: the rules' evaluation order first and,
// when rules with an intent are in play, the decision model. It changes nothing, so the
// live pipeline, cleanup and the rule tester all decide through it and cannot disagree.
type Decider struct {
	Router *models.Router // nil = no decision model is set
	// Override returns the router for a rule's own model, or nil when it cannot be used.
	Override      func(ctx context.Context, spec string) *models.Router
	MinConfidence float64   // act threshold for rules that set none
	Now           time.Time // for age_days
	// Examples returns the user's corrections to show the fallback model for this email,
	// most similar first; candidates are the rule ids on offer. nil = show none.
	Examples func(ctx context.Context, email message.Summary, candidates []int64) []learn.Example
	// Own are the mailbox's own addresses, whose mail is left alone: no sender rule, rule or
	// model is consulted for it. Empty = none (the leave_own_mail setting is off). See OwnMail.
	Own []string
	// RouteOnly are rules a sender rule may route to although they are not in the rules
	// given to Settle (see rules.Options.RouteOnly). nil = none.
	RouteOnly []rules.Rule
}

// OwnMail returns the Decider.Own for an account: its own addresses while the
// leave_own_mail setting is on, nil while it is off. The live pipeline reads it for every
// email; a cleanup check and a test run once, as they start. The setting is the account's
// tenant's.
func OwnMail(ctx context.Context, st *store.Store, a store.Account) ([]string, error) {
	on, err := st.LeaveOwnMail(ctx, a.TenantID)
	if err != nil || !on {
		return nil, err
	}
	return a.OwnAddresses(), nil
}

// Corrections is the Examples of a Decider that learns from the corrections made to mail
// of the mailboxes v sees. A failure to read them is logged and costs the examples, never
// the decision.
func Corrections(st *store.Store, v store.Viewer) func(ctx context.Context, email message.Summary, candidates []int64) []learn.Example {
	return func(ctx context.Context, email message.Summary, candidates []int64) []learn.Example {
		ex, err := learn.Examples(ctx, st, v, email, candidates)
		if err != nil {
			slog.WarnContext(ctx, "could not read corrections for the fallback model", "error", err.Error())
		}
		return ex
	}
}

// Outcome is what was settled for one email.
type Outcome struct {
	rules.Result
	RuleName string // the name of Result.RuleID; empty when there is none
	Reason   string // one sentence saying why
	Asked    bool   // a model was asked
	// NoModel: rules with an intent were in play and no model could be asked, so they were
	// passed over. With a model set, this is an email the model would have decided.
	NoModel bool
	Calls   int          // how many model calls that took, counting retries and the fallback
	Usage   models.Usage // what asking cost, over every call made
	// Probabilities is what the decision model gave each candidate, by rule id (0 = none
	// of them); nil when it gives none.
	Probabilities map[int64]float64
}

// Settle runs the evaluation order for one email and asks the model when it has to. The
// email is decided by the model of the highest-priority candidate rule that names one
// (and whose model can be used), otherwise by the default router.
func (d Decider) Settle(ctx context.Context, sum message.Summary, rs []rules.Rule, senders []rules.SenderRule) (Outcome, error) {
	if slices.ContainsFunc(d.Own, func(addr string) bool { return strings.EqualFold(addr, sum.From) }) {
		return Outcome{Result: rules.Result{Stage: rules.StageNone}, Reason: ReasonOwnMail}, nil
	}
	names := map[int64]string{}
	for _, r := range rs {
		names[r.ID] = r.Name
	}
	for _, r := range d.RouteOnly {
		names[r.ID] = r.Name
	}
	ev := rules.Evaluate(sum, rs, senders, rules.Options{MinConfidence: d.MinConfidence, Now: d.Now, RouteOnly: d.RouteOnly})
	if ev.Final != nil {
		return Outcome{Result: *ev.Final, RuleName: names[ev.Final.RuleID], Reason: localReason(*ev.Final, names)}, nil
	}
	router := d.Router
	req := models.DecideRequest{Email: sum}
	ids := make([]int64, 0, len(ev.Candidates))
	overridden := false
	for _, r := range ev.Candidates {
		req.Candidates = append(req.Candidates, models.Candidate{RuleID: r.ID, Name: r.Name, Intent: r.Intent, Exceptions: r.Exceptions.Text()})
		ids = append(ids, r.ID)
		if r.Model != "" && !overridden && d.Override != nil {
			if own := d.Override(ctx, r.Model); own != nil {
				router, overridden = own, true
			}
		}
	}
	if router == nil {
		// No model to ask: the rules with an intent are passed over, as if the model had
		// picked none of them, so the condition-only rule below them (or a stacking rule)
		// still applies. Only an email that nothing else takes waits in Needs review.
		res := ev.Resolve(0, 0)
		if res.Stage == rules.StageNone {
			return Outcome{Result: rules.Result{Stage: rules.StageNone, Review: true}, Reason: ReasonNoModel, NoModel: true}, nil
		}
		return Outcome{Result: res, RuleName: names[res.RuleID], Reason: localReason(res, names), NoModel: true}, nil
	}
	if router.Fallback != nil && d.Examples != nil { // only the fallback is shown examples
		req.Examples = d.Examples(ctx, sum, ids)
	}
	routed, err := router.Route(ctx, req)
	if errors.Is(err, models.ErrBadOutput) {
		// The model answered, ReadTries times, with nothing that could be read: the email
		// is left where it is (PRD R5), not retried later nor parked in Needs review.
		slog.WarnContext(ctx, "model answers could not be read; keeping the email", "error", err.Error())
		return Outcome{Result: rules.Result{Stage: rules.StageNone}, Reason: ReasonUnreadable, Asked: true,
			Calls: routed.Calls, Usage: routed.Primary}, nil
	}
	if err != nil {
		return Outcome{}, fmt.Errorf("decide: %w: %w", ErrModel, err)
	}
	out := Outcome{Result: ev.Resolve(routed.RuleID, routed.Confidence), Reason: routed.Reason, Asked: true, Calls: routed.Calls, Usage: routed.Primary,
		Probabilities: routed.Probabilities}
	if out.Stage == rules.StageCondition {
		out.Reason += fmt.Sprintf("; the default rule %q applied", names[out.RuleID])
	}
	if routed.Fallback != nil {
		out.Usage = *routed.Fallback
		out.Usage.TokensIn += routed.Primary.TokensIn
		out.Usage.TokensOut += routed.Primary.TokensOut
		out.Usage.CostUSD += routed.Primary.CostUSD
		out.Usage.Latency += routed.Primary.Latency
		if out.Stage == rules.StageDecider {
			out.Stage = "fallback" // the stage only this step knows (see rules.Stage)
		}
	}
	out.RuleName = names[out.RuleID]
	return out, nil
}

// localReason words a result no model was asked about.
func localReason(res rules.Result, names map[int64]string) string {
	switch {
	case res.Stage == rules.StageNone:
		return "No rule matched"
	case res.Stage == rules.StageSender && res.RuleID == 0 && res.Actions[0].Type == rules.ActMove:
		return fmt.Sprintf("Sender rule: move to %q", res.Actions[0].Folder)
	case res.Stage == rules.StageSender && res.RuleID == 0:
		return "Sender rule: " + res.Actions[0].Type // keep, or trash for a blocked sender
	case res.Stage == rules.StageSender:
		return fmt.Sprintf("Sender rule: %q", names[res.RuleID])
	}
	return fmt.Sprintf("Matched %q by its conditions", names[res.RuleID])
}
