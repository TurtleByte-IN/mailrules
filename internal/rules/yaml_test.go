package rules

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// prdExample is the rule file example from the product requirements.
const prdExample = `
defaults:
  decision_model: jev            # or clef
  fallback_model: claude-haiku-4-5
  min_confidence: 0.75
rules:
  - id: food
    said: "Put all Swiggy and Zomato stuff in Food"
    match:
      from_domain: [swiggy.in, zomato.com]   # condition only: zero model calls
    actions: ["move:Food"]
  - id: recruiters
    said: "recruiter emails go to Jobs unless I've talked to them before"
    when: "Recruiter outreach about job openings"
    unless:
      replied_before: true
    actions: ["move:Jobs"]
  - id: scams
    said: "trash anything that looks like a fake bank alert"
    when: "Phishing, scams or fake bank alerts"
    actions: [trash]
    min_confidence: 0.9
`

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseYAML(t *testing.T) {
	f, err := ParseYAML([]byte(prdExample))
	if err != nil {
		t.Fatal(err)
	}
	if f.Defaults.DecisionModel != "jev" || f.Defaults.FallbackModel != "claude-haiku-4-5" || *f.Defaults.MinConfidence != 0.75 {
		t.Errorf("defaults = %+v", f.Defaults)
	}
	want := []Rule{
		{Name: "food", Said: "Put all Swiggy and Zomato stuff in Food", Priority: 1, Enabled: true, MinConfidence: conf(0.75),
			Conditions: Cond{All: []Cond{leaf("from_domain", OpIn, []any{"swiggy.in", "zomato.com"})}},
			Actions:    []Action{{Type: ActMove, Folder: "Food"}}},
		{Name: "recruiters", Said: "recruiter emails go to Jobs unless I've talked to them before", Priority: 2, Enabled: true,
			Intent: "Recruiter outreach about job openings", MinConfidence: conf(0.75),
			Exceptions: Cond{All: []Cond{leaf("replied_before", OpEq, true)}},
			Actions:    []Action{{Type: ActMove, Folder: "Jobs"}}},
		{Name: "scams", Said: "trash anything that looks like a fake bank alert", Priority: 3, Enabled: true,
			Intent: "Phishing, scams or fake bank alerts", MinConfidence: conf(0.9), Actions: []Action{{Type: ActTrash}}},
	}
	if got, want := mustJSON(t, f.Rules), mustJSON(t, want); got != want {
		t.Errorf("rules =\n%s\nwant\n%s", got, want)
	}
}

func TestYAMLRoundTrip(t *testing.T) {
	const full = `
rules:
  - id: Big invoices
    match:
      all:
        - {field: subject, op: contains_any, value: [invoice, receipt]}
        - any:
            - {field: size_kb, op: gt, value: 100}
            - {field: subject, op: matches, value: '^re:'}
            - {field: body, op: not_contains, value: [unsubscribe]}
    unless: {is_contact: true, from_domain: [bank.example], account: 3}
    actions: [{type: move, folder: "Money: 2026"}, flag]
    model: clef
  - id: flag lists
    match: {field: list_id, op: exists}
    actions: [flag, read]
    stack: true
    enabled: false
  - id: scams
    when: Fake bank alerts
    actions: [trash]
    min_confidence: 0.9
`
	for name, src := range map[string]string{"prd example": prdExample, "trees, shorthands and extras": full} {
		t.Run(name, func(t *testing.T) {
			first, err := ParseYAML([]byte(src))
			if err != nil {
				t.Fatal(err)
			}
			out, err := MarshalYAML(first)
			if err != nil {
				t.Fatal(err)
			}
			second, err := ParseYAML(out)
			if err != nil {
				t.Fatalf("re-import: %v\n%s", err, out)
			}
			if a, b := mustJSON(t, first), mustJSON(t, second); a != b {
				t.Errorf("round-trip changed the rules:\n%s\nbecame\n%s\nvia\n%s", a, b, out)
			}
			again, err := MarshalYAML(second)
			if err != nil || string(again) != string(out) {
				t.Errorf("export is not stable (%v):\n%s\nthen\n%s", err, out, again)
			}
		})
	}

	// The export uses both shorthands and keeps the full tree where it must.
	f, _ := ParseYAML([]byte(full))
	f.Rules[1].Priority, f.Rules[2].Priority = 3, 2 // export follows priority, not slice order
	out, _ := MarshalYAML(f)
	for _, want := range []string{"is_contact: true", "- 'move:Money: 2026'", "- flag", "op: not_contains", "stack: true", "enabled: false", "model: clef"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("export lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(string(out), "id: scams") > strings.Index(string(out), "id: flag lists") {
		t.Errorf("export is not in priority order:\n%s", out)
	}
	if f.Rules[0].Conditions.All[1].Any[1].re == nil {
		t.Error("regex from YAML not compiled")
	}
}

func TestParseYAMLErrors(t *testing.T) {
	tests := []struct {
		name, src string
		wantPath  string // "" = a syntax error, not a ValidationError
		wantIn    string
	}{
		{"not yaml", "rules: [", "", "parse rules yaml"},
		{"unknown key", "rules:\n  - {id: a, wen: x, actions: [trash]}", "", "wen"},
		{"no action", "rules:\n  - {id: a, match: {is_bulk: true}}", "actions", `rule 1 ("a")`},
		{"unknown shorthand field", "rules:\n  - {id: a, match: {sender: x}, actions: [keep]}", "conditions.all[0].field", "sender"},
		{"unknown op in tree", "rules:\n  - {id: ok, match: {is_bulk: true}, actions: [keep]}\n  - {id: b, match: {any: [{field: subject, op: like, value: x}]}, actions: [keep]}",
			"conditions.any[0].op", `rule 2 ("b")`},
		{"match is not a map", "rules:\n  - {id: a, match: [is_bulk], actions: [keep]}", "conditions", "must be a map"},
		{"misspelt tree key", "rules:\n  - {id: a, unless: {all: [{feild: subject}]}, actions: [keep]}", "exceptions", "feild"},
		{"move without folder", "rules:\n  - {id: a, match: {is_bulk: true}, actions: [move]}", "actions[0].folder", ""},
		{"action is a number", "rules:\n  - {id: a, match: {is_bulk: true}, actions: [3]}", "actions[0]", ""},
		{"action map with unknown key", "rules:\n  - {id: a, match: {is_bulk: true}, actions: [{type: move, dir: x}]}", "actions[0]", ""},
		{"duplicate id", "rules:\n  - {id: a, match: {is_bulk: true}, actions: [keep]}\n  - {id: a, match: {is_bulk: true}, actions: [keep]}", "id", "rule 2"},
		{"default threshold too low for intent trash", "defaults: {min_confidence: 0.75}\nrules:\n  - {id: a, when: scams, actions: [trash]}", "min_confidence", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseYAML([]byte(tt.src))
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("error %q lacks %q", err, tt.wantIn)
			}
			var ve *ValidationError
			if got := errors.As(err, &ve); got != (tt.wantPath != "") {
				t.Fatalf("ValidationError = %v for %v", got, err)
			}
			if ve != nil && ve.Path != tt.wantPath {
				t.Errorf("path = %s, want %s (%v)", ve.Path, tt.wantPath, err)
			}
		})
	}

	// Every bad rule is reported, not just the first.
	_, err := ParseYAML([]byte("rules:\n  - {id: a, actions: [keep]}\n  - {id: b, match: {is_bulk: true}}"))
	if err == nil || !strings.Contains(err.Error(), `"a"`) || !strings.Contains(err.Error(), `"b"`) {
		t.Errorf("want both rules reported, got %v", err)
	}
}
