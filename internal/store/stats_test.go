package store

import (
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
)

// MAI-31: only an email a sender rule or a condition rule acted on counts as decided
// without a model. An email nobody decided (no model to ask, or no rule matched) does not,
// and the count never passes the processed emails (MAI-19).
func TestStatsTotalsWithoutModel(t *testing.T) {
	type seed struct{ state, stage, model string }
	senderRule := seed{StateActed, "sender", ""}
	condition := seed{StateActed, "condition", ""}
	aiDecision := seed{StateActed, "decider", "jev-1"}
	fallback := seed{StateActed, "fallback", "claude-haiku-4-5"}
	defaultAfterModel := seed{StateActed, "condition", "jev-1"} // a model was asked, then the default rule applied
	noModelReview := seed{StateReview, "none", ""}              // "No decision model is set"
	noRuleMatched := seed{StateSkipped, "none", ""}

	for _, tc := range []struct {
		name                    string
		seeds                   []seed
		wantProcessed, wantFree int
	}{
		{"nothing processed", nil, 0, 0},
		{"only an email waiting for want of a model", []seed{noModelReview}, 1, 0},
		{"sender rule", []seed{senderRule}, 1, 1},
		{"condition rule", []seed{condition}, 1, 1},
		{"model decision", []seed{aiDecision, fallback}, 2, 0},
		{"default rule after a model was asked", []seed{defaultAfterModel}, 1, 0},
		{"no rule matched, left in the inbox", []seed{noRuleMatched}, 1, 0},
		{"mixed day", []seed{senderRule, condition, aiDecision, noModelReview, noRuleMatched}, 5, 2},
		{"everything free", []seed{senderRule, condition, condition}, 3, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := open(t)
			ctx := t.Context()
			a := newAccount(t, s, "me@icloud.com", "pw")
			u, err := s.FirstUser(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for i, sd := range tc.seeds {
				m, _, err := s.IngestMessage(ctx, mail.MsgRef{AccountID: a.ID, Folder: "INBOX", UIDValidity: 1, UID: uint32(i + 1)}, 1000)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.AddDecision(ctx, Decision{MessageID: m.ID, Stage: sd.stage, Model: sd.model, CreatedAt: 1000}, sd.state); err != nil {
					t.Fatal(err)
				}
			}
			got, err := s.StatsTotals(ctx, u.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got.Processed != tc.wantProcessed || got.WithoutModel != tc.wantFree {
				t.Errorf("totals = processed %d, without model %d; want %d, %d", got.Processed, got.WithoutModel, tc.wantProcessed, tc.wantFree)
			}
			if got.WithoutModel > got.Processed {
				t.Errorf("without model %d is more than processed %d", got.WithoutModel, got.Processed)
			}
		})
	}
}
