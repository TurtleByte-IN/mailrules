package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
}

// Corrections is the Examples of a Decider that learns from the user's corrections. A
// failure to read them is logged and costs the examples, never the decision.
func Corrections(st *store.Store, userID int64) func(ctx context.Context, email message.Summary, candidates []int64) []learn.Example {
	return func(ctx context.Context, email message.Summary, candidates []int64) []learn.Example {
		ex, err := learn.Examples(ctx, st, userID, email, candidates)
		if err != nil {
			slog.WarnContext(ctx, "could not read corrections for the fallback model", "error", err.Error())
		}
		return ex
	}
}

// Outcome is what was settled for one email.
type Outcome struct {
	rules.Result
	RuleName string       // the name of Result.RuleID; empty when there is none
	Reason   string       // one sentence saying why
	Asked    bool         // a model was asked
	Calls    int          // how many model calls that took: 2 when the fallback answered too
	Usage    models.Usage // what asking cost, over both models when the fallback answered
	// Probabilities is what the decision model gave each candidate, by rule id (0 = none
	// of them); nil when it gives none.
	Probabilities map[int64]float64
}

// Settle runs the evaluation order for one email and asks the model when it has to. The
// email is decided by the model of the highest-priority candidate rule that names one
// (and whose model can be used), otherwise by the default router.
func (d Decider) Settle(ctx context.Context, sum message.Summary, rs []rules.Rule, senders []rules.SenderRule) (Outcome, error) {
	names := map[int64]string{}
	for _, r := range rs {
		names[r.ID] = r.Name
	}
	ev := rules.Evaluate(sum, rs, senders, rules.Options{MinConfidence: d.MinConfidence, Now: d.Now})
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
		return Outcome{Result: rules.Result{Stage: rules.StageNone, Review: true}, Reason: ReasonNoModel}, nil
	}
	if router.Fallback != nil && d.Examples != nil { // only the fallback is shown examples
		req.Examples = d.Examples(ctx, sum, ids)
	}
	routed, err := router.Route(ctx, req)
	if err != nil {
		return Outcome{}, fmt.Errorf("decide: %w: %w", ErrModel, err)
	}
	out := Outcome{Result: ev.Resolve(routed.RuleID, routed.Confidence), Reason: routed.Reason, Asked: true, Calls: 1, Usage: routed.Primary,
		Probabilities: routed.Probabilities}
	if out.Stage == rules.StageCondition {
		out.Reason += fmt.Sprintf("; the default rule %q applied", names[out.RuleID])
	}
	if routed.Fallback != nil {
		out.Calls, out.Usage = 2, *routed.Fallback
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
	case res.Stage == rules.StageSender && res.RuleID == 0:
		return "Sender rule: " + res.Actions[0].Type // keep, or trash for a blocked sender
	case res.Stage == rules.StageSender:
		return fmt.Sprintf("Sender rule: %q", names[res.RuleID])
	}
	return fmt.Sprintf("Matched %q by its conditions", names[res.RuleID])
}
