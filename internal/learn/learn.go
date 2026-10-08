// Package learn holds what the daemon learns from its own decisions and the user's
// corrections: learned sender rules, and the few-shot examples the fallback model is shown.
// It also defines the example type the model adapters accept.
package learn

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"

	"github.com/TurtleByte-IN/mailrules/internal/message"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// Example is one past correction shown to the fallback model as a few-shot example.
type Example struct {
	Email       message.Summary // the corrected email; its body is at most the stored snippet
	RightRuleID int64           // the rule the user said was right; 0 = keep in inbox
}

// A sender is learned after After model decisions in a row to the same rule, each at
// Confidence or above and none corrected. Bulk mail that passed DMARC (a newsletter-type
// sender, PRD R15) needs only AfterBulk, as long as every recent decision for the sender
// agrees with it.
const (
	After      = 3
	AfterBulk  = 1
	Confidence = 0.9
)

// Observe runs after a model decided for mail from sender. When the sender's recent
// decisions qualify, it creates a learned sender rule that routes the address to that rule
// without a model call, and reports true. bulk says the email just decided was bulk mail
// that passed DMARC. Corrections delete the rule again (store.AddCorrection). v is who the
// email's mailbox belongs to: the decisions read are of the mailboxes they see, and the
// sender rule goes to their tenant.
func Observe(ctx context.Context, st *store.Store, v store.Viewer, sender string, bulk bool, now int64) (bool, error) {
	need := After
	if bulk {
		need = AfterBulk
	}
	recent, err := st.RecentSenderDecisions(ctx, v, sender, After)
	if err != nil || len(recent) < need {
		return false, err
	}
	for _, d := range recent {
		byModel := d.Stage == "decider" || d.Stage == "fallback"
		if !byModel || d.RuleID == 0 || d.RuleID != recent[0].RuleID || d.Confidence < Confidence || d.Corrected {
			return false, nil
		}
	}
	return st.AddLearnedSenderRule(ctx, v.TenantID, v.UserID, sender, recent[0].RuleID, now)
}

// MaxExamples is how many corrections the fallback model is shown for one email.
const MaxExamples = 5

// recent is how many of the newest corrections retrieval looks at.
// ponytail: ranks the newest 500 corrections in memory on every escalation; one user makes
// a handful a week. Add from_addr and from_domain columns to corrections and rank in SQL
// if that stops being true.
const recent = 500

// Past is one stored correction as retrieval ranks it.
type Past struct {
	Example
	At int64 // when the user made it
}

// Rank picks the corrections to show for an email: at most MaxExamples, most similar
// first. Same sender address beats same sender domain, which beats same List-Id, which
// beats no likeness at all; within each, the most recent comes first. Only corrections
// the model can still answer with are kept: towards one of candidates, or towards "keep in
// the inbox" (rule 0).
func Rank(email message.Summary, past []Past, candidates []int64) []Example {
	likeness := func(p Past) int {
		switch {
		case email.From != "" && p.Email.From == email.From:
			return 0
		case email.FromDomain != "" && p.Email.FromDomain == email.FromDomain:
			return 1
		case email.ListID != "" && p.Email.ListID == email.ListID:
			return 2
		}
		return 3
	}
	past = slices.DeleteFunc(slices.Clone(past), func(p Past) bool {
		return p.RightRuleID != 0 && !slices.Contains(candidates, p.RightRuleID)
	})
	slices.SortStableFunc(past, func(a, b Past) int {
		return cmp.Or(cmp.Compare(likeness(a), likeness(b)), cmp.Compare(b.At, a.At))
	})
	out := make([]Example, 0, MaxExamples)
	for _, p := range past[:min(len(past), MaxExamples)] {
		out = append(out, p.Example)
	}
	return out
}

// Examples retrieves the few-shot examples for an email from the corrections made to mail
// of the mailboxes v sees.
func Examples(ctx context.Context, st *store.Store, v store.Viewer, email message.Summary, candidates []int64) ([]Example, error) {
	rows, err := st.Corrections(ctx, v, recent)
	if err != nil {
		return nil, err
	}
	past := make([]Past, 0, len(rows))
	for _, c := range rows {
		p := Past{Example: Example{RightRuleID: c.RightRuleID}, At: c.CreatedAt}
		if json.Unmarshal([]byte(c.Example), &p.Email) != nil {
			continue // written by a version that summarised differently: not worth failing a decision
		}
		past = append(past, p)
	}
	return Rank(email, past, candidates), nil
}
