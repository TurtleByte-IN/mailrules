package store

import (
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
)

// MAI-31: only an email a sender rule or a condition rule acted on counts as decided
// without a model. An email nobody decided (no model to ask, or no rule matched) does not,
// and the count never passes the processed emails (MAI-19). An email whose actions were
// only recorded in dry-run counts as it does among the sorted ones: as what would have
// been done.
func TestStatsTotalsWithoutModel(t *testing.T) {
	type seed struct{ state, stage, model, act string } // act: the status of its one move action, "" for none
	senderRule := seed{StateActed, "sender", "", ""}
	condition := seed{StateActed, "condition", "", ""}
	aiDecision := seed{StateActed, "decider", "jev-1", ""}
	fallback := seed{StateActed, "fallback", "claude-haiku-4-5", ""}
	defaultAfterModel := seed{StateActed, "condition", "jev-1", ""} // a model was asked, then the default rule applied
	noModelReview := seed{StateReview, "none", "", ""}              // "No decision model is set"
	noRuleMatched := seed{StateSkipped, "none", "", ""}
	senderRuleLive := seed{StateActed, "sender", "", ActionDone}
	senderRuleDry := seed{StateActed, "sender", "", ActionDryRun}
	conditionDry := seed{StateActed, "condition", "", ActionDryRun}
	aiDecisionDry := seed{StateActed, "decider", "jev-1", ActionDryRun}

	for _, tc := range []struct {
		name                    string
		seeds                   []seed
		wantProcessed, wantFree int
		wantSorted              int // checked when the seeds carry actions
	}{
		{"nothing processed", nil, 0, 0, 0},
		{"only an email waiting for want of a model", []seed{noModelReview}, 1, 0, 0},
		{"sender rule", []seed{senderRule}, 1, 1, 0},
		{"condition rule", []seed{condition}, 1, 1, 0},
		{"model decision", []seed{aiDecision, fallback}, 2, 0, 0},
		{"default rule after a model was asked", []seed{defaultAfterModel}, 1, 0, 0},
		{"no rule matched, left in the inbox", []seed{noRuleMatched}, 1, 0, 0},
		{"mixed day", []seed{senderRule, condition, aiDecision, noModelReview, noRuleMatched}, 5, 2, 0},
		{"everything free", []seed{senderRule, condition, condition}, 3, 3, 0},
		{"sender rule, live", []seed{senderRuleLive}, 1, 1, 1},
		{"sender rule and condition rule in dry-run, as sorted counts them", []seed{senderRuleDry, conditionDry}, 2, 2, 2},
		{"model decision in dry-run", []seed{aiDecisionDry, senderRuleDry}, 2, 1, 2},
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
				d, err := s.AddDecision(ctx, Decision{MessageID: m.ID, Stage: sd.stage, Model: sd.model, CreatedAt: 1000}, sd.state)
				if err != nil {
					t.Fatal(err)
				}
				if sd.act != "" {
					rec := Action{DecisionID: d, MessageID: m.ID, AccountID: a.ID, Kind: "move", Folder: "Food", Status: sd.act, CreatedAt: 1000,
						Before: Snapshot{Folder: "INBOX", UIDValidity: 1, UID: uint32(i + 1)}}
					if _, err := s.InsertAction(ctx, rec); err != nil {
						t.Fatal(err)
					}
				}
			}
			got, err := s.StatsTotals(ctx, u.Viewer(), 0)
			if err != nil {
				t.Fatal(err)
			}
			if got.Processed != tc.wantProcessed || got.WithoutModel != tc.wantFree {
				t.Errorf("totals = processed %d, without model %d; want %d, %d", got.Processed, got.WithoutModel, tc.wantProcessed, tc.wantFree)
			}
			if tc.wantSorted > 0 && got.Sorted != tc.wantSorted {
				t.Errorf("sorted %d, want %d", got.Sorted, tc.wantSorted)
			}
			if got.WithoutModel > got.Processed {
				t.Errorf("without model %d is more than processed %d", got.WithoutModel, got.Processed)
			}
		})
	}
}
