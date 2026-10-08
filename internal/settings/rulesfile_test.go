package settings

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// withMailboxes is Settings with a user, two connected mailboxes (ids 1 and 2) and two rules
// that exist already: "Work" limited to the first mailbox, "Home" to every mailbox.
func withMailboxes(t *testing.T, env map[string]string) (s *Settings, user store.User) {
	t.Helper()
	ctx := t.Context()
	s = newSettings(t, env)
	user, err := s.Store.CreateFirstUser(ctx, "me@example.test", "hash", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"work@acme.example", "me@icloud.com"} {
		if _, err := s.Store.CreateAccount(ctx, s.Master, store.Account{UserID: user.ID, Label: "label of " + name, Preset: "generic", Host: "h", Port: 993, TLSMode: "implicit", Username: name}, "pw"); err != nil {
			t.Fatal(err)
		}
	}
	for i, r := range []rules.Rule{
		{Name: "Work", AccountID: 1, Conditions: rules.Cond{All: []rules.Cond{{Field: "from_domain", Op: rules.OpEq, Value: "acme.example"}}}},
		{Name: "Home", Conditions: rules.Cond{All: []rules.Cond{{Field: "from_domain", Op: rules.OpEq, Value: "home.example"}}}},
	} {
		r.UserID, r.Priority, r.Enabled, r.Actions = user.ID, i+1, true, []rules.Action{{Type: rules.ActKeep}}
		if _, err := s.Store.CreateRule(ctx, r, 1); err != nil {
			t.Fatal(err)
		}
	}
	return s, user
}

func TestExportRules(t *testing.T) {
	ctx := t.Context()
	s, user := withMailboxes(t, map[string]string{"MAILRULES_DECIDER": "clef"})
	// A stored setting wins over the environment: the file says what is in force.
	model := "clef-flash"
	if err := s.Apply(ctx, Patch{DeciderModel: &model}); err != nil {
		t.Fatal(err)
	}
	accountRule := rules.Rule{UserID: user.ID, Name: "Not work", Priority: 3, Enabled: true, Actions: []rules.Action{{Type: rules.ActKeep}},
		Conditions: rules.Cond{All: []rules.Cond{{Field: "account", Op: rules.OpEq, Value: 2}}},
		Exceptions: rules.Cond{Any: []rules.Cond{{Field: "is_bulk", Op: rules.OpEq, Value: true}, {All: []rules.Cond{{Field: "account", Op: rules.OpIn, Value: []any{1, 2}}}}}}}
	if _, err := s.Store.CreateRule(ctx, accountRule, 1); err != nil {
		t.Fatal(err)
	}
	f, err := s.ExportRules(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if f.Defaults != (rules.Defaults{DecisionModel: "clef:clef-flash", FallbackModel: "claude-haiku-4-5"}) {
		t.Errorf("defaults = %+v", f.Defaults)
	}
	if len(f.Rules) != 3 || len(f.Mailboxes) != 1 || f.Mailboxes["Work"] != "work@acme.example" {
		t.Errorf("rules %d, mailboxes %v: want Work scoped to its address and Home not scoped", len(f.Rules), f.Mailboxes)
	}
	got := f.Rules[2]
	if c, e := jsonOf(t, got.Conditions), jsonOf(t, got.Exceptions); c != `{"all":[{"field":"account","op":"eq","value":"me@icloud.com"}]}` ||
		e != `{"any":[{"field":"is_bulk","op":"eq","value":true},{"all":[{"field":"account","op":"in","value":["work@acme.example","me@icloud.com"]}]}]}` {
		t.Errorf("account conditions are not written as addresses:\n%s\n%s", c, e)
	}
	if _, err := rules.MarshalYAML(f); err != nil {
		t.Errorf("the file cannot be written: %v", err)
	}

	// A fallback turned off is not written.
	off := ""
	if err := s.Apply(ctx, Patch{FallbackModel: &off}); err != nil {
		t.Fatal(err)
	}
	if f, err = s.ExportRules(ctx, user.ID); err != nil || f.Defaults.FallbackModel != "" {
		t.Errorf("defaults = %+v, err %v", f.Defaults, err)
	}
}

// An export never writes a mailbox number, so a condition on a mailbox that is gone stops it,
// as applies_to does.
func TestExportRulesMailboxGone(t *testing.T) {
	ctx := t.Context()
	s, user := withMailboxes(t, nil)
	gone := rules.Rule{UserID: user.ID, Name: "Gone", Priority: 3, Enabled: true, Intent: "x", Actions: []rules.Action{{Type: rules.ActKeep}},
		Exceptions: rules.Cond{All: []rules.Cond{{Field: "account", Op: rules.OpIn, Value: []any{1, 9}}}}}
	if _, err := s.Store.CreateRule(ctx, gone, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExportRules(ctx, user.ID); err == nil || !strings.Contains(err.Error(), `rule "Gone" has a condition on mailbox 9, which no longer exists`) {
		t.Errorf("err = %v", err)
	}
}

func TestPrepareImport(t *testing.T) {
	const rule = `
  - {id: %s, match: {from_domain: x.example}, actions: [keep]%s}`
	tests := []struct {
		name        string
		env         map[string]string
		fallbackOff bool   // the install has saved an empty fallback model
		twin        bool   // a third mailbox is connected, as WORK@acme.example
		defaults    string // the file's defaults block, indented
		rules       string // "name|extra" per line
		account     map[string]int64
		unless      map[string]string // the rule's exceptions as stored, in JSON
		problems    []string          // each a part of a message, in order; empty = accepted
		paths       []string          // each problem's path, when given
	}{
		{name: "no defaults and no mailboxes", rules: "New|", account: map[string]int64{"New": 0}},
		{name: "the models in force", defaults: "decision_model: jev\n  fallback_model: claude-haiku-4-5", rules: "New|"},
		{name: "a decider with its model", env: map[string]string{"MAILRULES_DECIDER": "clef", "MAILRULES_DECIDER_MODEL": "clef-flash"},
			defaults: "decision_model: clef:clef-flash", rules: "New|"},
		{name: "a decider named without its model", env: map[string]string{"MAILRULES_DECIDER": "clef", "MAILRULES_DECIDER_MODEL": "clef-flash"},
			defaults: "decision_model: clef", rules: "New|"},
		{name: "another decision model", defaults: "decision_model: clef", rules: "New|",
			problems: []string{`written for the decision model "clef", but this install uses "jev"`}},
		{name: "another model of the same decider", env: map[string]string{"MAILRULES_DECIDER": "clef", "MAILRULES_DECIDER_MODEL": "clef-flash"},
			defaults: "decision_model: clef:clef-pro", rules: "New|",
			problems: []string{`"clef:clef-pro", but this install uses "clef:clef-flash"`}},
		{name: "another fallback model", defaults: "fallback_model: claude-sonnet-4-5", rules: "New|",
			problems: []string{`fallback model "claude-sonnet-4-5", but this install uses "claude-haiku-4-5". An import does not change the models`}},
		{name: "a fallback where this install has none", fallbackOff: true, defaults: "fallback_model: claude-haiku-4-5", rules: "New|",
			problems: []string{`but this install uses none (the fallback is off)`}},
		{name: "both models wrong, each said", defaults: "decision_model: clef\n  fallback_model: x", rules: "New|",
			problems: []string{"decision model", "fallback model"}},

		{name: "a mailbox", rules: "New|, applies_to: me@icloud.com", account: map[string]int64{"New": 2}},
		{name: "a mailbox in other letters", rules: "New|, applies_to: Work@ACME.example", account: map[string]int64{"New": 1}},
		{name: "two rules, two mailboxes", rules: "A|, applies_to: work@acme.example\nB|, applies_to: me@icloud.com\nC|", account: map[string]int64{"A": 1, "B": 2, "C": 0}},
		{name: "a mailbox that is not connected", rules: "New|, applies_to: nobody@else.example",
			problems: []string{`Rule 1 ("New"): no mailbox for nobody@else.example is connected here`}},
		{name: "every unknown mailbox is named", rules: "A|, applies_to: a@x.example\nB|, applies_to: me@icloud.com\nC|, applies_to: c@x.example",
			problems: []string{`Rule 1 ("A"): no mailbox for a@x.example`, `Rule 3 ("C"): no mailbox for c@x.example`}},
		{name: "a rule the file says nothing about keeps its mailbox", rules: "Work|\nHome|", account: map[string]int64{"Work": 1, "Home": 0}},
		{name: "a mailbox replaces the one it had", rules: "Work|, applies_to: me@icloud.com\nHome|, applies_to: work@acme.example", account: map[string]int64{"Work": 2, "Home": 1}},
		{name: "all widens a rule", rules: "Work|, applies_to: all\nHome|, applies_to: ALL", account: map[string]int64{"Work": 0, "Home": 0}},
		{name: "mailbox and model problems together", defaults: "decision_model: clef", rules: "New|, applies_to: nobody@else.example",
			problems: []string{"decision model", "no mailbox for nobody@else.example"}},
		{name: "two mailboxes by one address", twin: true, rules: "New|, applies_to: work@acme.example",
			problems: []string{`Rule 1 ("New"): more than one connected mailbox is called work@acme.example`}, paths: []string{"rules[0].applies_to"}},

		{name: "an account condition", rules: "New|, unless: {account: me@icloud.com}",
			unless: map[string]string{"New": `{"all":[{"field":"account","op":"eq","value":2}]}`}},
		{name: "nested, in a list, in other letters", rules: "New|, unless: {any: [{field: is_bulk, op: eq, value: true}, {all: [{field: account, op: in, value: [ME@iCloud.com, work@acme.example]}]}]}",
			unless: map[string]string{"New": `{"any":[{"field":"is_bulk","op":"eq","value":true},{"all":[{"field":"account","op":"in","value":[2,1]}]}]}`}},
		{name: "a condition on a mailbox that is not connected", rules: "A|\nB|, unless: {any: [{field: account, op: ne, value: ghost@x.example}]}",
			problems: []string{`Rule 2 ("B"): no mailbox for ghost@x.example is connected here, so its account condition cannot name it`}, paths: []string{"rules[1].unless"}},
		{name: "a condition on two mailboxes by one address", twin: true, rules: "New|, unless: {account: [me@icloud.com, Work@Acme.example]}",
			problems: []string{`Rule 1 ("New"): more than one connected mailbox is called Work@Acme.example, so it is not clear which its account condition means`}, paths: []string{"rules[0].unless"}},
		{name: "every problem in a rule is named", rules: "New|, applies_to: a@x.example, unless: {account: [b@x.example, me@icloud.com, c@x.example]}",
			problems: []string{"no mailbox for a@x.example", "no mailbox for b@x.example", "no mailbox for c@x.example"},
			paths:    []string{"rules[0].applies_to", "rules[0].unless", "rules[0].unless"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			s, user := withMailboxes(t, tt.env)
			if tt.twin {
				if _, err := s.Store.CreateAccount(ctx, s.Master, store.Account{UserID: user.ID, Label: "twin", Preset: "generic", Host: "other", Port: 993, TLSMode: "implicit", Username: "WORK@acme.example"}, "pw"); err != nil {
					t.Fatal(err)
				}
			}
			if tt.fallbackOff {
				off := ""
				if err := s.Apply(ctx, Patch{FallbackModel: &off}); err != nil {
					t.Fatal(err)
				}
			}
			var b strings.Builder
			if tt.defaults != "" {
				b.WriteString("defaults:\n  " + tt.defaults + "\n")
			}
			b.WriteString("rules:")
			for line := range strings.SplitSeq(tt.rules, "\n") {
				name, extra, _ := strings.Cut(line, "|")
				b.WriteString(strings.Replace(strings.Replace(rule, "%s", name, 1), "%s", extra, 1))
			}
			f, err := rules.ParseYAML([]byte(b.String()))
			if err != nil {
				t.Fatalf("the test file is wrong: %v\n%s", err, b.String())
			}
			parsed := jsonOf(t, f.Rules)
			got, err := s.PrepareImport(ctx, user.ID, f)
			if len(tt.problems) > 0 {
				if err == nil {
					t.Fatalf("accepted, want problems %q", tt.problems)
				}
				var all, paths []string
				for _, e := range joinedErrors(err) {
					var p *ImportProblem
					if !errors.As(e, &p) || p.Path == "" {
						t.Fatalf("problem %v is not an ImportProblem with a path", e)
					}
					all, paths = append(all, p.Message), append(paths, p.Path)
				}
				if tt.paths != nil && !slices.Equal(paths, tt.paths) {
					t.Errorf("paths = %q, want %q", paths, tt.paths)
				}
				if len(all) != len(tt.problems) {
					t.Fatalf("problems = %q, want %d", all, len(tt.problems))
				}
				for i, want := range tt.problems {
					if !strings.Contains(all[i], want) {
						t.Errorf("problem %d = %q, want it to say %q", i, all[i], want)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(f.Rules) {
				t.Fatalf("%d rules back, want %d", len(got), len(f.Rules))
			}
			for _, r := range got {
				if want, ok := tt.account[r.Name]; ok && r.AccountID != want {
					t.Errorf("rule %q: account %d, want %d", r.Name, r.AccountID, want)
				}
				if want, ok := tt.unless[r.Name]; ok && jsonOf(t, r.Exceptions) != want {
					t.Errorf("rule %q: unless %s, want %s", r.Name, jsonOf(t, r.Exceptions), want)
				}
			}
			if after := jsonOf(t, f.Rules); after != parsed {
				t.Errorf("the parsed file was changed: %s became %s", parsed, after)
			}
		})
	}
}

// A rules file names a mailbox by address. ParseYAML refuses a number already; a File
// built some other way gets the same answer from the import.
func TestPrepareImportMailboxNumber(t *testing.T) {
	s, user := withMailboxes(t, nil)
	f := rules.File{Rules: []rules.Rule{{Name: "New", Enabled: true, Actions: []rules.Action{{Type: rules.ActKeep}},
		Conditions: rules.Cond{Any: []rules.Cond{{Field: "account", Op: rules.OpIn, Value: []any{"me@icloud.com", 1.0}}}}}}}
	_, err := s.PrepareImport(t.Context(), user.ID, f)
	var p *ImportProblem
	if !errors.As(err, &p) || p.Path != "rules[0].match" || p.Message != `Rule 1 ("New"): `+rules.AccountByAddress {
		t.Errorf("err = %v", err)
	}
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// joinedErrors is what an errors.Join holds.
func joinedErrors(err error) []error {
	if j, ok := err.(interface{ Unwrap() []error }); ok { //nolint:errorlint // the join itself
		return j.Unwrap()
	}
	return []error{err}
}
