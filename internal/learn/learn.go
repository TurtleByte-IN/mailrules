// Package learn holds what the daemon learns from its own decisions: learned sender
// rules now, few-shot retrieval of corrections later (milestone M9). It also defines the
// example type the model adapters accept.
package learn

import (
	"context"

	"github.com/TurtleByte-IN/mailrules/internal/message"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// Example is one past correction shown to the fallback model as a few-shot example.
type Example struct {
	Email       message.Summary // the corrected email; its body is at most the stored snippet
	RightRuleID int64           // the rule the user said was right; 0 = keep in inbox
}

// A sender is learned after After model decisions in a row to the same rule, each at
// Confidence or above and none corrected.
const (
	After      = 3
	Confidence = 0.9
)

// Observe runs after a model decided for mail from sender. When the sender's last After
// decisions qualify, it creates a learned sender rule that routes the address to that rule
// without a model call, and reports true. Corrections delete the rule again
// (store.AddCorrection).
func Observe(ctx context.Context, st *store.Store, userID int64, sender string, now int64) (bool, error) {
	recent, err := st.RecentSenderDecisions(ctx, userID, sender, After)
	if err != nil || len(recent) < After {
		return false, err
	}
	for _, d := range recent {
		byModel := d.Stage == "decider" || d.Stage == "fallback"
		if !byModel || d.RuleID == 0 || d.RuleID != recent[0].RuleID || d.Confidence < Confidence || d.Corrected {
			return false, nil
		}
	}
	return st.AddLearnedSenderRule(ctx, userID, sender, recent[0].RuleID, now)
}
