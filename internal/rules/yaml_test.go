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
            - {field: account, op: in, value: [work@acme.example, me@icloud.com]}
    unless: {is_contact: true, from_domain: [bank.example], account: work@acme.example}
    actions: [{type: move, folder: "Money: 2026"}, flag]
    model: clef
  - id: flag lists
    match: {field: list_id, op: exists}
    actions: [flag, read]
    stack: true
    enabled: false
  - id: scams
    template: Cold sales
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
	for _, want := range []string{"is_contact: true", "- 'move:Money: 2026'", "- flag", "op: not_contains", "stack: true", "enabled: false", "model: clef", "template: Cold sales",
		"account: work@acme.example", "- me@icloud.com"} {
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
		{"default threshold too low for intent trash", "defaults: {min_confidence: 0.75}\nrules:\n  - {id: a, when: scams, actions: [trash]}", "min_confidence", ""},
		{"a mailbox number in the shorthand", "rules:\n  - {id: a, match: {account: 3}, actions: [keep]}", "conditions.all[0].value", "by its email address"},
		{"a mailbox number in a list", "rules:\n  - {id: a, match: {account: [me@icloud.com, 3]}, actions: [keep]}", "conditions.all[0].value", "by its email address"},
		{"a mailbox number deep in unless", "rules:\n  - {id: a, match: {is_bulk: true}, unless: {all: [{any: [{field: account, op: ne, value: 3}]}]}, actions: [keep]}",
			"exceptions.all[0].any[0].value", "not by its number"},
		{"a list for eq", "rules:\n  - {id: a, match: {field: account, op: eq, value: [me@icloud.com]}, actions: [keep]}", "conditions.value", "one mailbox address"},
		{"one address for in", "rules:\n  - {id: a, match: {field: account, op: in, value: me@icloud.com}, actions: [keep]}", "conditions.value", "a list of mailbox addresses"},
		{"a blank address", "rules:\n  - {id: a, match: {account: \"\"}, actions: [keep]}", "conditions.all[0].value", "one mailbox address"},
		{"duplicate id", "rules:\n  - {id: a, match: {is_bulk: true}, actions: [keep]}\n  - {id: a, match: {is_bulk: true}, actions: [keep]}", "id", "rule 2"},
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

// A file keeps the defaults block and the mailbox each rule applies to, so that an export
// read back is the same file: two rules limited to different mailboxes stay limited, the
// rule that applies everywhere stays so, and the models in the defaults are still there.
func TestYAMLKeepsDefaultsAndMailboxes(t *testing.T) {
	in := File{
		Defaults: Defaults{DecisionModel: "clef:clef-flash", FallbackModel: "claude-haiku-4-5"},
		Rules: []Rule{
			{Name: "Work invoices", Priority: 1, Enabled: true, AccountID: 7, Conditions: Cond{All: []Cond{leaf("from_domain", OpIn, []any{"acme.example"})}},
				Actions: []Action{{Type: ActMove, Folder: "Money"}}},
			{Name: "Everywhere", Priority: 2, Enabled: true, Intent: "Newsletters", Actions: []Action{{Type: ActArchive}}},
			{Name: "Home bills", Priority: 3, Enabled: true, AccountID: 8, Conditions: Cond{All: []Cond{leaf("subject", OpContains, "bill")}},
				Actions: []Action{{Type: ActMove, Folder: "Bills"}}},
		},
		Mailboxes: map[string]string{"Work invoices": "work@acme.example", "Home bills": "me@icloud.com"},
	}
	out, err := MarshalYAML(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"defaults:", "decision_model: clef:clef-flash", "fallback_model: claude-haiku-4-5", "applies_to: work@acme.example", "applies_to: me@icloud.com"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("export lacks %q:\n%s", want, out)
		}
	}
	if n := strings.Count(string(out), "applies_to:"); n != 2 {
		t.Errorf("%d applies_to lines, want 2 (the rule for every mailbox says nothing):\n%s", n, out)
	}
	back, err := ParseYAML(out)
	if err != nil {
		t.Fatalf("re-import: %v\n%s", err, out)
	}
	if back.Defaults != in.Defaults {
		t.Errorf("defaults = %+v, want %+v", back.Defaults, in.Defaults)
	}
	if got := back.Mailboxes; len(got) != 2 || got["Work invoices"] != "work@acme.example" || got["Home bills"] != "me@icloud.com" {
		t.Errorf("mailboxes = %v", got)
	}
	// The account ids are the importer's to fill in from the addresses.
	for i := range back.Rules {
		back.Rules[i].AccountID = in.Rules[i].AccountID
	}
	if a, b := mustJSON(t, in.Rules), mustJSON(t, back.Rules); a != b {
		t.Errorf("rules changed:\n%s\nbecame\n%s", a, b)
	}
}

func TestYAMLMailboxes(t *testing.T) {
	const rule = "rules:\n  - {id: a, match: {from_domain: x.example}, actions: [keep]%s}\n"
	t.Run("what applies_to may say", func(t *testing.T) {
		tests := []struct {
			name, extra string
			want        map[string]string
		}{
			{"nothing", "", map[string]string{}},
			{"a mailbox", ", applies_to: me@icloud.com", map[string]string{"a": "me@icloud.com"}},
			{"spaces are dropped", `, applies_to: "  me@icloud.com "`, map[string]string{"a": "me@icloud.com"}},
			{"all", ", applies_to: all", map[string]string{"a": "all"}},
			{"blank says nothing", `, applies_to: ""`, map[string]string{}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				f, err := ParseYAML([]byte(strings.Replace(rule, "%s", tt.extra, 1)))
				if err != nil {
					t.Fatal(err)
				}
				if len(f.Mailboxes) != len(tt.want) || f.Mailboxes["a"] != tt.want["a"] {
					t.Errorf("mailboxes = %v, want %v", f.Mailboxes, tt.want)
				}
			})
		}
	})
	// An export that dropped the scope would turn the rule into one for every mailbox.
	t.Run("a scoped rule without an address is not exported", func(t *testing.T) {
		_, err := MarshalYAML(File{Rules: []Rule{{Name: "a", Enabled: true, AccountID: 3, Actions: []Action{{Type: ActKeep}}}}})
		if err == nil || !strings.Contains(err.Error(), `rule "a" applies to mailbox 3`) {
			t.Errorf("err = %v", err)
		}
	})
	// A mailbox number in a file means another mailbox, or none, on the install that reads it.
	t.Run("an account condition without addresses is not exported", func(t *testing.T) {
		for name, c := range map[string]Cond{
			"in match":         {All: []Cond{leaf("account", OpEq, 3.0)}},
			"in a nested list": {Any: []Cond{leaf("is_bulk", OpEq, true), {All: []Cond{leaf("account", OpIn, []any{"me@icloud.com", 3.0})}}}},
		} {
			_, err := MarshalYAML(File{Rules: []Rule{{Name: "a", Enabled: true, Conditions: c, Actions: []Action{{Type: ActKeep}}}}})
			if err == nil || !strings.Contains(err.Error(), `rule "a" has a condition on mailbox 3`) {
				t.Errorf("%s: err = %v", name, err)
			}
			_, err = MarshalYAML(File{Rules: []Rule{{Name: "a", Enabled: true, Intent: "x", Exceptions: c, Actions: []Action{{Type: ActKeep}}}}})
			if err == nil {
				t.Errorf("%s in unless: exported", name)
			}
		}
	})
	t.Run("an older file has no applies_to and no defaults", func(t *testing.T) {
		f, err := ParseYAML([]byte("rules:\n  - {id: a, match: {from_domain: x.example}, actions: [keep]}\n"))
		if err != nil || f.Defaults != (Defaults{}) || len(f.Mailboxes) != 0 {
			t.Errorf("file = %+v, err %v", f, err)
		}
	})
}

// A rule whose mailbox was removed is written off and marked, and read back off and marked
// whatever the file says about enabled; the mark is not written for any other rule.
func TestYAMLMailboxRemoved(t *testing.T) {
	marked := Rule{Name: "a", Priority: 1, Intent: "x", MailboxRemoved: true, Actions: []Action{{Type: ActKeep}}}
	out, err := MarshalYAML(File{Rules: []Rule{marked, {Name: "b", Priority: 2, Enabled: true, Intent: "y", Actions: []Action{{Type: ActKeep}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); strings.Count(got, "mailbox_removed: true") != 1 || strings.Count(got, "enabled: false") != 1 || strings.Contains(got, "applies_to") {
		t.Errorf("export:\n%s", got)
	}
	f, err := ParseYAML(out)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := f.Rules[0], f.Rules[1]; !a.MailboxRemoved || a.Enabled || b.MailboxRemoved || !b.Enabled {
		t.Errorf("read back: a %+v, b %+v", a, b)
	}
	f, err = ParseYAML([]byte("rules:\n  - {id: a, when: x, actions: [keep], enabled: true, mailbox_removed: true}\n"))
	if err != nil || !f.Rules[0].MailboxRemoved || f.Rules[0].Enabled {
		t.Errorf("enabled: true with mailbox_removed: %+v, %v", f.Rules, err)
	}
}
