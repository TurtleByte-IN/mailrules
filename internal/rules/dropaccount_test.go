package rules

import (
	"slices"
	"testing"
)

func TestDropAccount(t *testing.T) {
	const x = 7 // the mailbox being removed; 8 stays
	eqX, neX := leaf("account", OpEq, float64(x)), leaf("account", OpNe, float64(x))
	eqY := leaf("account", OpEq, float64(8))
	subj := leaf("subject", OpContains, "y")
	dom := leaf("from_domain", OpEq, "a.com")
	all := func(cs ...Cond) Cond { return Cond{All: cs} }
	anyOf := func(cs ...Cond) Cond { return Cond{Any: cs} }

	tests := []struct {
		name          string
		in            Rule // Enabled, Intent "x" and an action are added unless set
		noIntent, off bool
		want          Rule // AccountID, Conditions, Exceptions, Enabled, MailboxRemoved
		changed       bool
	}{
		{name: "a rule that names another mailbox is left alone",
			in: Rule{Conditions: all(dom, eqY)}, want: Rule{Conditions: all(dom, eqY), Enabled: true}},
		{name: "a rule that names no mailbox is left alone",
			in: Rule{Conditions: subj, Exceptions: dom}, want: Rule{Conditions: subj, Exceptions: dom, Enabled: true}},
		{name: "a value that is not the mailbox's whole number does not name it",
			in: Rule{Conditions: leaf("account", OpEq, 7.5)}, want: Rule{Conditions: leaf("account", OpEq, 7.5), Enabled: true}},

		{name: "a rule for the mailbox is kept off, marked and for no mailbox",
			in: Rule{AccountID: x, Conditions: subj}, want: Rule{Conditions: subj, MailboxRemoved: true}, changed: true},
		{name: "a rule for the mailbox loses its account leaves too",
			in:   Rule{AccountID: x, Conditions: all(eqX, subj), Exceptions: neX},
			want: Rule{Conditions: all(subj), MailboxRemoved: true}, changed: true},
		{name: "a rule for another mailbox is not touched by a condition on another",
			in: Rule{AccountID: 8, Conditions: subj}, want: Rule{AccountID: 8, Conditions: subj, Enabled: true}},

		// Match, at the top.
		{name: "match eq X can never match: off and marked, leaf dropped",
			in: Rule{Conditions: eqX}, want: Rule{MailboxRemoved: true}, changed: true},
		{name: "match ne X always matches: dropped, the intent rule stays on",
			in: Rule{Conditions: neX}, want: Rule{Enabled: true}, changed: true},
		{name: "match ne X on a condition-only rule leaves no condition: off and marked",
			in: Rule{Conditions: neX}, noIntent: true, want: Rule{MailboxRemoved: true}, changed: true},
		{name: "match in [X, Y] becomes in [Y]",
			in:   Rule{Conditions: leaf("account", OpIn, []any{float64(x), float64(8)})},
			want: Rule{Conditions: leaf("account", OpIn, []any{float64(8)}), Enabled: true}, changed: true},
		{name: "match in [X] can never match: off and marked",
			in: Rule{Conditions: leaf("account", OpIn, []any{float64(x)})}, want: Rule{MailboxRemoved: true}, changed: true},

		// Match, inside groups.
		{name: "any: eq X drops away, the rest stays on",
			in: Rule{Conditions: anyOf(eqX, dom)}, want: Rule{Conditions: anyOf(dom), Enabled: true}, changed: true},
		{name: "all: eq X makes it false: off and marked, the rest kept as written",
			in: Rule{Conditions: all(eqX, subj)}, want: Rule{Conditions: all(subj), MailboxRemoved: true}, changed: true},
		{name: "all: ne X drops away",
			in: Rule{Conditions: all(neX, subj)}, want: Rule{Conditions: all(subj), Enabled: true}, changed: true},
		{name: "any: ne X makes it true: no condition left",
			in: Rule{Conditions: anyOf(neX, subj)}, want: Rule{Enabled: true}, changed: true},
		{name: "any of only X: off and marked",
			in: Rule{Conditions: anyOf(eqX, leaf("account", OpIn, []any{float64(x)}))}, want: Rule{MailboxRemoved: true}, changed: true},
		{name: "nested: in a group in a group",
			in:   Rule{Conditions: all(subj, anyOf(eqX, eqY))},
			want: Rule{Conditions: all(subj, anyOf(eqY)), Enabled: true}, changed: true},
		{name: "nested: a false all drops out of its any",
			in: Rule{Conditions: anyOf(all(eqX, subj), dom)}, want: Rule{Conditions: anyOf(dom), Enabled: true}, changed: true},
		{name: "nested: a false any makes its all false, and the trees keep the rest",
			in:   Rule{Conditions: all(dom, anyOf(eqX, all(eqX, subj)))},
			want: Rule{Conditions: all(dom, anyOf(all(subj))), MailboxRemoved: true}, changed: true},

		// Exceptions.
		{name: "exception eq X can never apply: the exceptions go",
			in: Rule{Conditions: subj, Exceptions: eqX}, want: Rule{Conditions: subj, Enabled: true}, changed: true},
		{name: "exception ne X always applies: off and marked",
			in: Rule{Conditions: subj, Exceptions: neX}, want: Rule{Conditions: subj, MailboxRemoved: true}, changed: true},
		{name: "exception any: eq X drops away",
			in:   Rule{Conditions: subj, Exceptions: anyOf(eqX, dom)},
			want: Rule{Conditions: subj, Exceptions: anyOf(dom), Enabled: true}, changed: true},
		{name: "exception all: ne X drops away",
			in:   Rule{Conditions: subj, Exceptions: all(neX, dom)},
			want: Rule{Conditions: subj, Exceptions: all(dom), Enabled: true}, changed: true},
		{name: "exception all with eq X can never apply: the exceptions go",
			in: Rule{Conditions: subj, Exceptions: all(eqX, dom)}, want: Rule{Conditions: subj, Enabled: true}, changed: true},
		{name: "exception in [X, Y] becomes in [Y]",
			in:   Rule{Conditions: subj, Exceptions: leaf("account", OpIn, []any{float64(x), float64(8)})},
			want: Rule{Conditions: subj, Exceptions: leaf("account", OpIn, []any{float64(8)}), Enabled: true}, changed: true},

		{name: "a rule that was off stays off, unmarked, when it still works",
			in: Rule{Conditions: all(neX, subj)}, off: true, want: Rule{Conditions: all(subj)}, changed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := tt.in
			in.Name, in.Enabled, in.Actions = "r", !tt.off, []Action{{Type: ActKeep}}
			if !tt.noIntent {
				in.Intent = "x"
			}
			before := mustJSON(t, in)
			got, changed := in.DropAccount(x)
			if changed != tt.changed {
				t.Errorf("changed = %v, want %v", changed, tt.changed)
			}
			if mustJSON(t, in) != before {
				t.Error("DropAccount changed the rule it was given")
			}
			want := in
			want.AccountID, want.Conditions, want.Exceptions = tt.want.AccountID, tt.want.Conditions, tt.want.Exceptions
			want.Enabled, want.MailboxRemoved = tt.want.Enabled, tt.want.MailboxRemoved
			if g, w := mustJSON(t, got), mustJSON(t, want); g != w {
				t.Errorf("got  %s\nwant %s", g, w)
			}
			if !got.MailboxRemoved && got.Validate() != nil {
				t.Errorf("a rule left on does not validate: %v", got.Validate())
			}
			if slices.Contains(got.AccountIDs(), x) {
				t.Errorf("the rule still names mailbox %d: %v", x, got.AccountIDs())
			}
			if again, ch := got.DropAccount(x); ch || mustJSON(t, again) != mustJSON(t, got) {
				t.Errorf("a second drop changed it again: %s", mustJSON(t, again))
			}
		})
	}
}

func TestAccountIDs(t *testing.T) {
	r := Rule{AccountID: 3, Conditions: Cond{Any: []Cond{leaf("account", OpEq, float64(4)), leaf("subject", OpEq, "5")}},
		Exceptions: leaf("account", OpIn, []any{float64(3), float64(6)})}
	if got := r.AccountIDs(); !slices.Equal(got, []int64{3, 4, 6}) {
		t.Errorf("AccountIDs = %v", got)
	}
	if got := (Rule{}).AccountIDs(); len(got) != 0 {
		t.Errorf("AccountIDs of a rule naming none = %v", got)
	}
}
