package store

import (
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
)

// MAI-244: the summary counts an email as Activity and the Overview do. An email whose
// latest decision's actions were all undone is back in the inbox, so it is neither sorted
// nor listed as trashed, even when an earlier decision on it (a run in dry-run, before the
// rule was sorted again live) left actions recorded.
func TestSummaryLeavesOutUndone(t *testing.T) {
	type step struct {
		status string // the status its action ends in
		at     int64
	}
	for _, tc := range []struct {
		name       string
		kind       string
		decisions  []step
		wantSorted int // trashed ones included, as the summary counts them
	}{
		{"done", rules.ActFlag, []step{{ActionDone, 1000}}, 1},
		{"done, then undone", rules.ActFlag, []step{{ActionUndone, 1000}}, 0},
		{"dry-run", rules.ActFlag, []step{{ActionDryRun, 1000}}, 1},
		{"dry-run, then sorted live", rules.ActFlag, []step{{ActionDryRun, 1000}, {ActionDone, 1100}}, 1},
		{"dry-run, then sorted live and undone", rules.ActFlag, []step{{ActionDryRun, 1000}, {ActionUndone, 1100}}, 0},
		{"done, then sorted again and undone", rules.ActFlag, []step{{ActionDone, 1000}, {ActionUndone, 1100}}, 0},
		{"trashed", rules.ActTrash, []step{{ActionDone, 1000}}, 1},
		{"trashed in dry-run, then live and undone", rules.ActTrash, []step{{ActionDryRun, 1000}, {ActionUndone, 1100}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := open(t)
			ctx := t.Context()
			a := newAccount(t, s, "me@icloud.com", "pw")
			u, err := s.FirstUser(ctx)
			if err != nil {
				t.Fatal(err)
			}
			r, err := s.CreateRule(ctx, u.TenantID, rules.Rule{UserID: u.ID, Name: "Login codes", Enabled: true,
				Actions: []rules.Action{{Type: tc.kind}}}, 1)
			if err != nil {
				t.Fatal(err)
			}
			m, _, err := s.IngestMessage(ctx, mail.MsgRef{AccountID: a.ID, Folder: "INBOX", UIDValidity: 1, UID: 1}, 1000)
			if err != nil {
				t.Fatal(err)
			}
			for _, st := range tc.decisions {
				d, err := s.AddDecision(ctx, Decision{MessageID: m.ID, Stage: "condition", RuleID: r.ID, RuleName: r.Name, CreatedAt: st.at}, StateActed)
				if err != nil {
					t.Fatal(err)
				}
				before := Snapshot{Folder: "INBOX", UIDValidity: 1, UID: 1}
				act := Action{DecisionID: d, MessageID: m.ID, AccountID: a.ID, Kind: tc.kind, Before: before, CreatedAt: st.at,
					Status: ActionDryRun}
				if st.status != ActionDryRun {
					act.Status = ActionFailed
				}
				if act.ID, err = s.InsertAction(ctx, act); err != nil {
					t.Fatal(err)
				}
				if st.status == ActionDryRun {
					continue
				}
				after := before
				if tc.kind == rules.ActTrash {
					after = Snapshot{Folder: "Trash", UIDValidity: 2, UID: 7}
				} else {
					after.Flags = []string{`\Flagged`}
				}
				act.Status, act.After = ActionDone, &after
				if err := s.FinishAction(ctx, act, FollowUps{}); err != nil {
					t.Fatal(err)
				}
				if st.status == ActionUndone {
					if err := s.MarkActionUndone(ctx, act, before.Ref(a.ID), st.at+1); err != nil {
						t.Fatal(err)
					}
				}
			}

			uses, err := s.SummaryRules(ctx, u.Viewer(), 0, 2000)
			if err != nil {
				t.Fatal(err)
			}
			sorted := 0
			for _, use := range uses {
				sorted += use.Emails
			}
			if sorted != tc.wantSorted {
				t.Errorf("summary sorted %d, want %d (%+v)", sorted, tc.wantSorted, uses)
			}
			tot, err := s.StatsTotals(ctx, u.Viewer(), 0)
			if err != nil {
				t.Fatal(err)
			}
			if tot.Sorted != tc.wantSorted || tot.WentNowhere != 1-tc.wantSorted {
				t.Errorf("overview sorted %d, inbox %d; want %d, %d", tot.Sorted, tot.WentNowhere, tc.wantSorted, 1-tc.wantSorted)
			}
			inbox, err := s.Activity(ctx, u.Viewer(), ActivityFilter{Outcome: "inbox"})
			if err != nil {
				t.Fatal(err)
			}
			if len(inbox) != 1-tc.wantSorted {
				t.Errorf("activity's inbox filter lists %d, want %d", len(inbox), 1-tc.wantSorted)
			}
			trashed, total, err := s.SummaryTrashed(ctx, u.Viewer(), 0, 2000, 10)
			if err != nil {
				t.Fatal(err)
			}
			if want := map[bool]int{true: tc.wantSorted}[tc.kind == rules.ActTrash]; total != want || len(trashed) != want {
				t.Errorf("summary trashed %d of %d, want %d", len(trashed), total, want)
			}
		})
	}
}
