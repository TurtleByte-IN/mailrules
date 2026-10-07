package rules

import (
	"cmp"
	"slices"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/message"
)

// Stage says which step produced a result; it is stored in decisions.stage.
// The pipeline rewrites StageDecider to "fallback" when the fallback model
// answered, which only it knows.
type Stage string

const (
	StageSender    Stage = "sender"
	StageCondition Stage = "condition"
	StageDecider   Stage = "decider"
	StageNone      Stage = "none"
)

// Sender rule match types and verdicts.
const (
	MatchAddress = "address"
	MatchDomain  = "domain"

	VerdictRoute = "route"
	VerdictKeep  = "keep"
	VerdictBlock = "block"
)

// SenderRule mirrors one row of the sender_rules table.
type SenderRule struct {
	ID        int64
	UserID    int64
	MatchType string // address | domain
	Value     string // lower-case
	RuleID    int64  // for route: the rule whose actions apply; 0 otherwise
	Verdict   string // route | keep | block
	Source    string // user | learned
	Hits      int
	CreatedAt int64
}

// Result is the final outcome for one email.
type Result struct {
	Stage       Stage
	RuleID      int64 // 0 = no rule; with Review, the rule the decider suggested
	RuleVersion int
	Confidence  float64
	Actions     []Action // the primary rule's actions, then each stacking rule's
	Stacked     []int64  // stacking rules whose actions were appended
	Review      bool     // below threshold: take no action, show in Needs review
	// SenderRuleID is the sender rule that settled the email (Stage is StageSender); 0 otherwise.
	SenderRuleID int64
}

// Options are the per-run settings Evaluate needs.
type Options struct {
	MinConfidence float64   // threshold for rules that set none
	Now           time.Time // for age_days
}

// Evaluation is what Evaluate found. When Final is set the email is settled
// without a model. Otherwise the caller asks the decider to pick among
// Candidates and passes its answer to Resolve.
type Evaluation struct {
	Final      *Result
	Candidates []Rule // passing intent rules above the cut-off, in priority order
	CutOff     *Rule  // the default when the decider picks none; may be nil

	stacking      []Rule
	minConfidence float64
}

// Evaluate runs the evaluation order for one email: sender rules, then the
// priority walk down to the cut-off rule. It makes no model call.
func Evaluate(e message.Summary, rs []Rule, senders []SenderRule, opt Options) Evaluation {
	rs = slices.Clone(rs)
	slices.SortStableFunc(rs, func(a, b Rule) int {
		return cmp.Or(cmp.Compare(a.Priority, b.Priority), cmp.Compare(a.ID, b.ID))
	})
	passes := func(r Rule) bool {
		return r.Enabled && (r.AccountID == 0 || r.AccountID == e.AccountID) &&
			r.Conditions.Match(&e, opt.Now) &&
			(r.Exceptions.IsEmpty() || !r.Exceptions.Match(&e, opt.Now)) // an empty "unless" excludes nothing
	}

	ev := Evaluation{minConfidence: opt.MinConfidence}
	for _, r := range rs {
		if r.Stack && r.Intent == "" && passes(r) {
			ev.stacking = append(ev.stacking, r)
		}
	}

	// Step 1: sender rules.
	if sr := matchSender(&e, senders); sr != nil {
		switch sr.Verdict {
		case VerdictKeep:
			ev.Final = &Result{Stage: StageSender, Confidence: 1, Actions: []Action{{Type: ActKeep}}, SenderRuleID: sr.ID}
			return ev
		case VerdictBlock:
			ev.Final = &Result{Stage: StageSender, Confidence: 1, Actions: []Action{{Type: ActTrash}}, SenderRuleID: sr.ID}
			return ev
		case VerdictRoute:
			// A route to a rule that is gone or switched off falls through to the walk.
			if i := slices.IndexFunc(rs, func(r Rule) bool { return r.ID == sr.RuleID && r.Enabled }); i >= 0 {
				res := ev.result(&rs[i], StageSender, 1)
				res.SenderRuleID = sr.ID
				ev.Final = &res
				return ev
			}
		}
	}

	// Steps 2 and 3: walk by priority; the first passing condition-only rule is the cut-off.
	for i, r := range rs {
		if r.Stack || !passes(r) {
			continue
		}
		if r.Intent == "" {
			ev.CutOff = &rs[i]
			break
		}
		ev.Candidates = append(ev.Candidates, r)
	}

	// Step 4: nothing for the decider to choose between.
	if len(ev.Candidates) == 0 {
		res := ev.result(ev.CutOff, StageCondition, 1)
		ev.Final = &res
	}
	return ev
}

// Resolve turns the decider's answer into the final result (step 5). A pick
// outside the candidates counts as "none of these", for which the cut-off rule
// applies; when none does, the result keeps the decider's confidence that no rule
// applies. A pick below its threshold goes to Needs review with no action at all,
// even when there is a cut-off rule.
func (ev Evaluation) Resolve(ruleID int64, confidence float64) Result {
	i := slices.IndexFunc(ev.Candidates, func(r Rule) bool { return r.ID == ruleID })
	if i < 0 {
		res := ev.result(ev.CutOff, StageCondition, 1)
		if res.Stage == StageNone {
			res.Confidence = confidence // how sure the decider was that no rule applies
		}
		return res
	}
	pick := &ev.Candidates[i]
	threshold := ev.minConfidence
	if pick.MinConfidence != nil {
		threshold = *pick.MinConfidence
	}
	if confidence >= threshold {
		return ev.result(pick, StageDecider, confidence)
	}
	return Result{Stage: StageDecider, RuleID: pick.ID, RuleVersion: pick.Version, Confidence: confidence, Review: true}
}

// result builds the outcome for a primary rule and appends the stacking
// rules' actions (step 6). With no primary rule the first stacking rule that
// passed takes its place; with neither, nothing happens.
func (ev Evaluation) result(primary *Rule, stage Stage, confidence float64) Result {
	stacking := ev.stacking
	if primary == nil {
		if len(stacking) == 0 {
			return Result{Stage: StageNone}
		}
		primary, stacking, stage, confidence = &stacking[0], stacking[1:], StageCondition, 1
	}
	res := Result{Stage: stage, RuleID: primary.ID, RuleVersion: primary.Version, Confidence: confidence,
		Actions: slices.Clone(primary.Actions)}
	for _, s := range stacking {
		if s.ID == primary.ID {
			continue
		}
		res.Actions = append(res.Actions, s.Actions...)
		res.Stacked = append(res.Stacked, s.ID)
	}
	return res
}

// matchSender finds the sender rule for an email: an address rule beats a
// domain rule, and the most specific domain beats its parents.
func matchSender(e *message.Summary, senders []SenderRule) *SenderRule {
	var best *SenderRule
	for i, sr := range senders {
		switch {
		case sr.MatchType == MatchAddress && sr.Value == e.From:
			return &senders[i]
		case sr.MatchType == MatchDomain && e.FromDomain != "" && domainMatches(e.FromDomain, sr.Value) &&
			(best == nil || len(sr.Value) > len(best.Value)):
			best = &senders[i]
		}
	}
	return best
}
