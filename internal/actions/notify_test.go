package actions

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// A rule that names destinations queues one notice per destination for every email it
// acts on live, in the transaction of its last action, and none for anything else.
func TestApplyQueuesNotices(t *testing.T) {
	tests := []struct {
		name        string
		stage       string // the decision's; "" = no decision (a correction)
		batch       string // live | cleanup
		dryRun      bool
		notify      bool // the rule names both destinations
		acts        []string
		failOnFlags bool // the server refuses flag changes
		plainHTTP   bool // the destinations were added over http, before only https was taken
		wantActions []string
		wantFolder  string
	}{
		{name: "a live run of a rule", stage: "condition", batch: store.BatchLive, notify: true, acts: []string{"move:Food", "read"},
			wantActions: []string{"move", "read"}, wantFolder: "Food"},
		{name: "a model's pick", stage: "decider", batch: store.BatchLive, notify: true, acts: []string{"archive"},
			wantActions: []string{"archive"}, wantFolder: "Archive"},
		{name: "a run a failure cut short tells what was done", stage: "condition", batch: store.BatchLive, notify: true,
			acts: []string{"move:Food", "flag"}, failOnFlags: true, wantActions: []string{"move"}, wantFolder: "Food"},
		{name: "a run that did nothing", stage: "condition", batch: store.BatchLive, notify: true, acts: []string{"flag"}, failOnFlags: true},
		{name: "dry-run", stage: "condition", batch: store.BatchLive, dryRun: true, notify: true, acts: []string{"move:Food"}},
		{name: "a cleanup", stage: "condition", batch: store.BatchCleanup, notify: true, acts: []string{"move:Food"}},
		{name: "a sender rule", stage: "sender", batch: store.BatchLive, notify: true, acts: []string{"move:Food"}},
		{name: "a correction", batch: store.BatchLive, notify: true, acts: []string{"move:Food"}},
		{name: "destinations over plain http", stage: "condition", batch: store.BatchLive, notify: true, plainHTTP: true, acts: []string{"move:Food"}},
		{name: "a rule with no destinations", stage: "condition", batch: store.BatchLive, acts: []string{"move:Food"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			ctx := t.Context()
			master := make([]byte, 32)
			var ids []int64
			scheme := "https"
			if tt.plainHTTP {
				scheme = "http"
			}
			for _, name := range []string{"Ops", "Me"} {
				d, err := e.st.CreateDestination(ctx, master, store.Destination{TenantID: 1, Name: name, Kind: store.DestinationWebhook},
					store.DestinationSecret{URL: scheme + "://hooks.example/" + name}, 1)
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, d.ID)
			}
			r := rules.Rule{UserID: e.user.ID, Name: "Food", Intent: "food", Actions: act(tt.acts...), Enabled: true}
			if tt.notify {
				r.Notify = ids
			}
			rule, err := e.st.CreateRule(ctx, 1, r, 1)
			if err != nil {
				t.Fatal(err)
			}
			if err := e.st.SetDryRun(ctx, 1, tt.dryRun); err != nil {
				t.Fatal(err)
			}
			if tt.failOnFlags {
				e.mb.storeErr = errors.New("NO [CANNOT] refused")
			}
			d := e.deliver("1")
			if tt.stage != "" {
				d.DecisionID, err = e.st.AddDecision(ctx, store.Decision{MessageID: d.MessageID, Stage: tt.stage, RuleID: rule.ID,
					RuleName: rule.Name, CreatedAt: 1}, store.StateDecided)
				if err != nil {
					t.Fatal(err)
				}
			}
			batch, err := e.st.LiveBatch(ctx, 1, time.Unix(1_800_000_000, 0))
			if tt.batch == store.BatchCleanup {
				var b store.Batch
				b, err = e.st.CreateCleanupBatch(ctx, e.acct.ID, "INBOX", 0, 0, 0, 1, 1)
				batch = b.ID
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.x.Apply(ctx, d, rule.Actions, batch); (err != nil) != tt.failOnFlags {
				t.Fatalf("Apply = %v", err)
			}

			got, err := e.st.WaitingNotices(ctx, 2_000_000_000, 100)
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantActions == nil {
				if len(got) != 0 {
					t.Fatalf("notices = %+v, want none", got)
				}
				return
			}
			if len(got) != len(ids) {
				t.Fatalf("notices = %+v, want one per destination", got)
			}
			for i, n := range got {
				if n.DestinationID != ids[i] || n.TenantID != 1 || n.AccountID != e.acct.ID || n.MessageID != d.MessageID ||
					n.RuleID != rule.ID || n.RuleName != "Food" || n.CreatedAt != 1_800_000_000 ||
					!slices.Equal(n.Actions, tt.wantActions) || n.Folder != tt.wantFolder ||
					n.From != "orders@shop.example" || n.Subject != "order 1" || n.Mailbox != "x" {
					t.Errorf("notice %d = %+v", i, n)
				}
			}
		})
	}
}

// An undo tells no one.
func TestUndoQueuesNoNotice(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	dest, err := e.st.CreateDestination(ctx, make([]byte, 32), store.Destination{TenantID: 1, Name: "Ops", Kind: store.DestinationSlack},
		store.DestinationSecret{URL: "https://hooks.slack.com/services/T/B/x"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	rule, err := e.st.CreateRule(ctx, 1, rules.Rule{UserID: e.user.ID, Name: "Food", Intent: "food", Actions: act("move:Food"),
		Enabled: true, Notify: []int64{dest.ID}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	d := e.deliver("1")
	d.DecisionID, _ = e.st.AddDecision(ctx, store.Decision{MessageID: d.MessageID, Stage: "condition", RuleID: rule.ID, RuleName: rule.Name, CreatedAt: 1}, store.StateDecided)
	batch, _ := e.st.LiveBatch(ctx, 1, time.Unix(1_800_000_000, 0))
	recs, err := e.x.Apply(ctx, d, rule.Actions, batch)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.x.Undo(ctx, recs[0].ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.st.WaitingNotices(ctx, 2_000_000_000, 100); len(got) != 1 {
		t.Errorf("notices after the undo = %+v, want only the one of the move", got)
	}
}
