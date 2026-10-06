// Package learn will hold corrections, learned sender rules and few-shot
// retrieval (milestone M9). For now it only defines the example type the model
// adapters accept.
package learn

import "github.com/TurtleByte-IN/mailrules/internal/message"

// Example is one past correction shown to the fallback model as a few-shot example.
type Example struct {
	Email       message.Summary // the corrected email; its body is at most the stored snippet
	RightRuleID int64           // the rule the user said was right; 0 = keep in inbox
}
