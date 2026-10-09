package rules

import (
	"reflect"
	"testing"
)

func TestEvaluate(t *testing.T) {
	zomato := leaf("from_domain", OpEq, "zomato.com")
	never := leaf("from_domain", OpEq, "nowhere.example")
	moveTo := func(f string) []Action { return []Action{{Type: ActMove, Folder: f}} }

	// Shared rule set, deliberately out of priority order.
	food := Rule{ID: 3, Name: "food", Priority: 30, Enabled: true, Version: 2, Conditions: zomato, Actions: moveTo("Food")}
	receipts := Rule{ID: 1, Name: "receipts", Priority: 10, Enabled: true, Intent: "Receipts and invoices", Actions: moveTo("Receipts")}
	promos := Rule{ID: 2, Name: "promos", Priority: 20, Enabled: true, Intent: "Promotions", Actions: moveTo("Promos"), MinConfidence: conf(0.9)}
	below := Rule{ID: 4, Name: "below the cut-off", Priority: 40, Enabled: true, Intent: "Anything", Actions: moveTo("Misc")}
	flagBulk := Rule{ID: 5, Name: "flag bulk", Priority: 5, Enabled: true, Stack: true, Conditions: leaf("is_bulk", OpEq, true), Actions: []Action{{Type: ActFlag}}}
	readPDF := Rule{ID: 6, Name: "read pdfs", Priority: 50, Enabled: true, Stack: true, Conditions: leaf("attachment_ext", OpEq, "pdf"), Actions: []Action{{Type: ActRead}}}

	type pick struct {
		ruleID     int64
		confidence float64
	}
	tests := []struct {
		name       string
		rules      []Rule
		senders    []SenderRule
		candidates []int64 // rule ids offered to the decider; nil = settled without a model
		routeOnly  []Rule  // rules only a sender rule may route to
		cutOff     int64   // 0 = none
		pick       pick
		want       Result
	}{
		// Step 1: sender rules.
		{name: "sender keep stops everything, stacking included", rules: []Rule{food, flagBulk},
			senders: []SenderRule{{MatchType: MatchAddress, Value: "noreply@mail.zomato.com", Verdict: VerdictKeep}},
			want:    Result{Stage: StageSender, Confidence: 1, Actions: []Action{{Type: ActKeep}}}},
		{name: "sender block trashes", rules: []Rule{food},
			senders: []SenderRule{{MatchType: MatchDomain, Value: "zomato.com", Verdict: VerdictBlock}},
			want:    Result{Stage: StageSender, Confidence: 1, Actions: []Action{{Type: ActTrash}}}},
		{name: "sender route applies that rule without a model", rules: []Rule{food, receipts, promos},
			senders: []SenderRule{{MatchType: MatchDomain, Value: "mail.zomato.com", Verdict: VerdictRoute, RuleID: 2}},
			want:    Result{Stage: StageSender, RuleID: 2, Confidence: 1, Actions: moveTo("Promos")}},
		// MAI-162: a sender's own folder wins over the rules, as keep and block do, stacking included.
		{name: "sender move files into its folder ahead of a matching rule", rules: []Rule{food, flagBulk},
			senders: []SenderRule{{MatchType: MatchDomain, Value: "zomato.com", Verdict: VerdictMove, Folder: "Takeaway"}},
			want:    Result{Stage: StageSender, Confidence: 1, Actions: moveTo("Takeaway")}},
		{name: "address move beats domain block", rules: []Rule{food},
			senders: []SenderRule{
				{MatchType: MatchDomain, Value: "zomato.com", Verdict: VerdictBlock},
				{MatchType: MatchAddress, Value: "noreply@mail.zomato.com", Verdict: VerdictMove, Folder: "Takeaway"}},
			want: Result{Stage: StageSender, Confidence: 1, Actions: moveTo("Takeaway")}},
		{name: "address keep beats domain move", rules: []Rule{food},
			senders: []SenderRule{
				{MatchType: MatchDomain, Value: "zomato.com", Verdict: VerdictMove, Folder: "Takeaway"},
				{MatchType: MatchAddress, Value: "noreply@mail.zomato.com", Verdict: VerdictKeep}},
			want: Result{Stage: StageSender, Confidence: 1, Actions: []Action{{Type: ActKeep}}}},
		{name: "a move for another sender is ignored", rules: []Rule{food},
			senders: []SenderRule{{MatchType: MatchDomain, Value: "swiggy.in", Verdict: VerdictMove, Folder: "Takeaway"}},
			want:    Result{Stage: StageCondition, RuleID: 3, RuleVersion: 2, Confidence: 1, Actions: moveTo("Food")}},
		{name: "address rule beats domain rule", rules: []Rule{food},
			senders: []SenderRule{
				{MatchType: MatchDomain, Value: "zomato.com", Verdict: VerdictBlock},
				{MatchType: MatchAddress, Value: "noreply@mail.zomato.com", Verdict: VerdictKeep}},
			want: Result{Stage: StageSender, Confidence: 1, Actions: []Action{{Type: ActKeep}}}},
		{name: "most specific domain wins", rules: []Rule{food},
			senders: []SenderRule{
				{MatchType: MatchDomain, Value: "mail.zomato.com", Verdict: VerdictKeep},
				{MatchType: MatchDomain, Value: "zomato.com", Verdict: VerdictBlock}},
			want: Result{Stage: StageSender, Confidence: 1, Actions: []Action{{Type: ActKeep}}}},
		{name: "sender rule for someone else is ignored", rules: []Rule{food},
			senders: []SenderRule{{MatchType: MatchDomain, Value: "swiggy.in", Verdict: VerdictBlock}},
			want:    Result{Stage: StageCondition, RuleID: 3, RuleVersion: 2, Confidence: 1, Actions: moveTo("Food")}},
		{name: "route to a missing rule falls through to the walk", rules: []Rule{food},
			senders: []SenderRule{{MatchType: MatchDomain, Value: "zomato.com", Verdict: VerdictRoute, RuleID: 99}},
			want:    Result{Stage: StageCondition, RuleID: 3, RuleVersion: 2, Confidence: 1, Actions: moveTo("Food")}},
		// A run of only some rules leaves the others out of the walk, but a sender rule still routes to one (MAI-43).
		{name: "sender route to a rule only it can reach still applies", rules: []Rule{food}, routeOnly: []Rule{promos},
			senders: []SenderRule{{MatchType: MatchDomain, Value: "zomato.com", Verdict: VerdictRoute, RuleID: 2}},
			want:    Result{Stage: StageSender, RuleID: 2, Confidence: 1, Actions: moveTo("Promos")}},
		{name: "a rule only a sender rule can reach is not walked", rules: []Rule{food}, routeOnly: []Rule{receipts},
			want: Result{Stage: StageCondition, RuleID: 3, RuleVersion: 2, Confidence: 1, Actions: moveTo("Food")}},
		{name: "route to a rule that is switched off falls through even if only it can reach it", rules: []Rule{food},
			routeOnly: []Rule{{ID: 2, Name: "promos", Priority: 20, Intent: "Promotions", Actions: moveTo("Promos")}},
			senders:   []SenderRule{{MatchType: MatchDomain, Value: "zomato.com", Verdict: VerdictRoute, RuleID: 2}},
			want:      Result{Stage: StageCondition, RuleID: 3, RuleVersion: 2, Confidence: 1, Actions: moveTo("Food")}},

		// Step 2: the walk skips rules that are off, fail, or are excepted.
		{name: "disabled rule is skipped", rules: []Rule{{ID: 9, Priority: 1, Conditions: zomato, Actions: moveTo("Off")}, food},
			want: Result{Stage: StageCondition, RuleID: 3, RuleVersion: 2, Confidence: 1, Actions: moveTo("Food")}},
		{name: "failing conditions are skipped", rules: []Rule{{ID: 9, Priority: 1, Enabled: true, Conditions: never, Actions: moveTo("No")}, food},
			want: Result{Stage: StageCondition, RuleID: 3, RuleVersion: 2, Confidence: 1, Actions: moveTo("Food")}},
		{name: "matching exception skips the rule", rules: []Rule{
			{ID: 9, Priority: 1, Enabled: true, Conditions: zomato, Exceptions: leaf("has_attachment", OpEq, true), Actions: moveTo("No")}, food},
			want: Result{Stage: StageCondition, RuleID: 3, RuleVersion: 2, Confidence: 1, Actions: moveTo("Food")}},
		{name: "rule for another account is skipped", rules: []Rule{{ID: 9, Priority: 1, Enabled: true, AccountID: 8, Conditions: zomato, Actions: moveTo("No")}, food},
			want: Result{Stage: StageCondition, RuleID: 3, RuleVersion: 2, Confidence: 1, Actions: moveTo("Food")}},
		{name: "rule for this account applies", rules: []Rule{{ID: 9, Priority: 1, Enabled: true, AccountID: 7, Conditions: zomato, Actions: moveTo("Mine")}, food},
			want: Result{Stage: StageCondition, RuleID: 9, Confidence: 1, Actions: moveTo("Mine")}},
		{name: "intent rule with failing conditions is not a candidate", rules: []Rule{
			{ID: 9, Priority: 1, Enabled: true, Intent: "Anything", Conditions: never, Actions: moveTo("No")}, food},
			want: Result{Stage: StageCondition, RuleID: 3, RuleVersion: 2, Confidence: 1, Actions: moveTo("Food")}},

		// Steps 3 and 4: cut-off and candidates.
		{name: "no candidates: the cut-off applies", rules: []Rule{food, below},
			want: Result{Stage: StageCondition, RuleID: 3, RuleVersion: 2, Confidence: 1, Actions: moveTo("Food")}},
		{name: "nothing passes: nothing happens", rules: []Rule{{ID: 9, Priority: 1, Enabled: true, Conditions: never, Actions: moveTo("No")}},
			want: Result{Stage: StageNone}},
		{name: "no rules at all", want: Result{Stage: StageNone}},

		// Step 5: the decider's pick against the threshold.
		{name: "pick above the default threshold applies", rules: []Rule{food, receipts, promos, below},
			candidates: []int64{1, 2}, cutOff: 3, pick: pick{1, 0.8},
			want: Result{Stage: StageDecider, RuleID: 1, Confidence: 0.8, Actions: moveTo("Receipts")}},
		{name: "pick exactly at the threshold applies", rules: []Rule{food, receipts, promos},
			candidates: []int64{1, 2}, cutOff: 3, pick: pick{1, 0.75},
			want: Result{Stage: StageDecider, RuleID: 1, Confidence: 0.75, Actions: moveTo("Receipts")}},
		{name: "pick below the rule's own threshold goes to review, not to the cut-off", rules: []Rule{food, receipts, promos},
			candidates: []int64{1, 2}, cutOff: 3, pick: pick{2, 0.8},
			want: Result{Stage: StageDecider, RuleID: 2, Confidence: 0.8, Review: true}},
		{name: "decider picks none: the cut-off is the default", rules: []Rule{food, receipts, promos},
			candidates: []int64{1, 2}, cutOff: 3, pick: pick{0, 0.99},
			want: Result{Stage: StageCondition, RuleID: 3, RuleVersion: 2, Confidence: 1, Actions: moveTo("Food")}},
		{name: "pick outside the candidates counts as none", rules: []Rule{food, receipts, promos, below},
			candidates: []int64{1, 2}, cutOff: 3, pick: pick{4, 0.99},
			want: Result{Stage: StageCondition, RuleID: 3, RuleVersion: 2, Confidence: 1, Actions: moveTo("Food")}},
		{name: "below threshold with no cut-off goes to review", rules: []Rule{receipts, promos},
			candidates: []int64{1, 2}, pick: pick{2, 0.8},
			want: Result{Stage: StageDecider, RuleID: 2, Confidence: 0.8, Review: true}},
		{name: "decider picks none with no cut-off: nothing happens, with how sure it was", rules: []Rule{receipts, promos},
			candidates: []int64{1, 2}, pick: pick{0, 0.6},
			want: Result{Stage: StageNone, Confidence: 0.6}},

		// Step 6: stacking.
		{name: "stacking rules append after the cut-off's actions", rules: []Rule{food, flagBulk, readPDF},
			want: Result{Stage: StageCondition, RuleID: 3, RuleVersion: 2, Confidence: 1, Stacked: []int64{5, 6},
				Actions: []Action{{Type: ActMove, Folder: "Food"}, {Type: ActFlag}, {Type: ActRead}}}},
		{name: "stacking rule is never the cut-off", rules: []Rule{flagBulk, receipts, food},
			candidates: []int64{1}, cutOff: 3, pick: pick{1, 0.9},
			want: Result{Stage: StageDecider, RuleID: 1, Confidence: 0.9, Stacked: []int64{5},
				Actions: []Action{{Type: ActMove, Folder: "Receipts"}, {Type: ActFlag}}}},
		{name: "stacking after a sender route", rules: []Rule{food, flagBulk},
			senders: []SenderRule{{MatchType: MatchDomain, Value: "zomato.com", Verdict: VerdictRoute, RuleID: 3}},
			want: Result{Stage: StageSender, RuleID: 3, RuleVersion: 2, Confidence: 1, Stacked: []int64{5},
				Actions: []Action{{Type: ActMove, Folder: "Food"}, {Type: ActFlag}}}},
		{name: "stacking rule alone becomes the result", rules: []Rule{flagBulk, readPDF},
			want: Result{Stage: StageCondition, RuleID: 5, Confidence: 1, Stacked: []int64{6},
				Actions: []Action{{Type: ActFlag}, {Type: ActRead}}}},
		{name: "stacking rule whose conditions fail adds nothing", rules: []Rule{food,
			{ID: 7, Priority: 1, Enabled: true, Stack: true, Conditions: never, Actions: []Action{{Type: ActFlag}}}},
			want: Result{Stage: StageCondition, RuleID: 3, RuleVersion: 2, Confidence: 1, Actions: moveTo("Food")}},
		{name: "review takes no stacking actions", rules: []Rule{receipts, flagBulk},
			candidates: []int64{1}, pick: pick{1, 0.5},
			want: Result{Stage: StageDecider, RuleID: 1, Confidence: 0.5, Review: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := Evaluate(testEmail(), tt.rules, tt.senders, Options{MinConfidence: 0.75, Now: testNow, RouteOnly: tt.routeOnly})

			var candidates []int64
			for _, c := range ev.Candidates {
				candidates = append(candidates, c.ID)
			}
			if !reflect.DeepEqual(candidates, tt.candidates) {
				t.Fatalf("candidates = %v, want %v", candidates, tt.candidates)
			}
			var cutOff int64
			if ev.CutOff != nil {
				cutOff = ev.CutOff.ID
			}
			if cutOff != tt.cutOff && tt.candidates != nil {
				t.Errorf("cut-off = %d, want %d", cutOff, tt.cutOff)
			}
			if (ev.Final == nil) != (tt.candidates != nil) {
				t.Fatalf("Final = %+v with candidates %v", ev.Final, candidates)
			}

			got := ev.Resolve(tt.pick.ruleID, tt.pick.confidence)
			if ev.Final != nil {
				got = *ev.Final
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("result = %+v\n          want %+v", got, tt.want)
			}
		})
	}
}
