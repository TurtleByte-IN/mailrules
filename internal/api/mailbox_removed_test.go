package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// removedRules is three rules over two mailboxes: one for work@acme.example only, one whose
// condition can match elsewhere too, and one whose condition can only match there.
const removedRules = `rules:
  - id: Work only
    when: Invoices
    actions: ["move:Money"]
    applies_to: work@acme.example
  - id: Either
    match:
      any:
        - {field: account, op: eq, value: work@acme.example}
        - {field: from_domain, op: eq, value: a.com}
    actions: ["move:A"]
  - id: Only work
    when: Anything
    match:
      all:
        - {field: account, op: eq, value: work@acme.example}
        - {field: subject, op: contains, value: y}
    actions: ["move:Y"]
`

// ruleNamed returns the rule called name, as GET /api/rules has it.
func (e *env) ruleNamed(name string) map[string]any {
	e.t.Helper()
	for _, r := range e.call(http.MethodGet, "/api/rules", "", http.StatusOK)["items"].([]any) {
		if m := r.(map[string]any); m["name"] == name {
			conform(e.t, e.doc, "Rule", m)
			return m
		}
	}
	e.t.Fatalf("no rule %q", name)
	return nil
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// removedEnv is an install with removedRules imported and work@acme.example (account 1) removed.
func removedEnv(t *testing.T) *env {
	e := newEnv(t)
	e.signIn()
	e.addMailboxes("work@acme.example", "me@icloud.com")
	e.call(http.MethodPost, "/api/rules/import", removedRules, http.StatusOK)
	e.call(http.MethodDelete, "/api/accounts/1", "", http.StatusNoContent)
	return e
}

// Removing a mailbox keeps the rules made for it, off and marked, and rewrites the rules
// whose conditions name it; the rules file still exports and imports (MAI-129, MAI-132).
func TestRulesOfARemovedMailbox(t *testing.T) {
	e := removedEnv(t)
	tests := []struct {
		name       string
		on, marked bool
		conditions string
	}{
		{"Work only", false, true, `{}`},
		{"Either", true, false, `{"any":[{"field":"from_domain","op":"eq","value":"a.com"}]}`},
		{"Only work", false, true, `{"all":[{"field":"subject","op":"contains","value":"y"}]}`},
	}
	for _, tt := range tests {
		r := e.ruleNamed(tt.name)
		if r["enabled"] != tt.on || r["mailbox_removed"] != tt.marked || r["account_id"] != nil || jsonOf(t, r["conditions"]) != tt.conditions {
			t.Errorf("%s = %v", tt.name, r)
		}
	}

	exp := e.do(http.MethodGet, "/api/rules/export", "")
	if exp.status != http.StatusOK {
		t.Fatalf("export = %d %s", exp.status, exp.raw)
	}
	exported := string(exp.raw)
	if strings.Count(exported, "mailbox_removed: true") != 2 || strings.Count(exported, "enabled: false") != 2 ||
		strings.Contains(exported, "applies_to") || strings.Contains(exported, "work@acme.example") {
		t.Errorf("export:\n%s", exported)
	}

	// The file goes back into the install it came from, and into another, as it was.
	for _, into := range []*env{e, func() *env { b := newEnv(t); b.signIn(); b.addMailboxes("me@icloud.com"); return b }()} {
		into.call(http.MethodPost, "/api/rules/import", exported, http.StatusOK)
		for _, tt := range tests {
			if r := into.ruleNamed(tt.name); r["enabled"] != tt.on || r["mailbox_removed"] != tt.marked {
				t.Errorf("after import %s = %v", tt.name, r)
			}
		}
		if again := into.do(http.MethodGet, "/api/rules/export", ""); string(again.raw) != exported {
			t.Errorf("export after import differs:\n%s\nwas\n%s", again.raw, exported)
		}
	}
}

// A rule whose mailbox was removed is not switched on until it is given a mailbox, or All
// mailboxes, or new conditions; that may come in the same request.
func TestEnableARuleOfARemovedMailbox(t *testing.T) {
	const msg = "Its mailbox was removed. Choose a mailbox for it, or All mailboxes, or edit its condition, before turning it on."
	tests := []struct {
		name, first, enable string // first: an edit before switching it on, or none
		account             any
	}{
		{"a mailbox in the same request", "", `{"enabled":true,"account_id":2}`, float64(2)},
		{"All mailboxes, then on", `{"account_id":null}`, `{"enabled":true}`, nil},
		{"new conditions in the same request", "", `{"enabled":true,"conditions":{"field":"subject","op":"contains","value":"z"}}`, nil},
		{"new exceptions, then on", `{"exceptions":{"field":"is_bulk","op":"eq","value":true}}`, `{"enabled":true}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := removedEnv(t)
			path := "/api/rules/" + strconv.FormatInt(id(e.ruleNamed("Work only")["id"]), 10)
			r := e.do(http.MethodPatch, path, `{"enabled":true}`)
			if r.status != http.StatusBadRequest || r.body.Error.Code != "invalid_input" || r.body.Error.Path != "enabled" || r.body.Error.Message != msg {
				t.Fatalf("enable = %d %q %q %q", r.status, r.body.Error.Code, r.body.Error.Path, r.body.Error.Message)
			}
			// Other edits that choose nothing leave it marked, and off.
			got := e.call(http.MethodPatch, path, `{"name":"Work only","actions":[{"type":"move","folder":"Money"}]}`, http.StatusOK)["rule"].(map[string]any)
			if got["mailbox_removed"] != true || got["enabled"] != false {
				t.Fatalf("after an edit that chose nothing: %v", got)
			}
			if tt.first != "" {
				got = e.call(http.MethodPatch, path, tt.first, http.StatusOK)["rule"].(map[string]any)
				if got["mailbox_removed"] != false || got["enabled"] != false {
					t.Fatalf("after %s: %v", tt.first, got)
				}
			}
			got = e.call(http.MethodPatch, path, tt.enable, http.StatusOK)["rule"].(map[string]any)
			if got["mailbox_removed"] != false || got["enabled"] != true || got["account_id"] != tt.account {
				t.Errorf("after %s: %v", tt.enable, got)
			}
			conform(t, e.doc, "Rule", got)
		})
	}
}

// What an import of a rule whose mailbox was removed refuses or keeps.
func TestImportARuleOfARemovedMailbox(t *testing.T) {
	t.Run("applies_to with mailbox_removed contradicts itself", func(t *testing.T) {
		e := newEnv(t)
		e.signIn()
		e.addMailboxes("me@icloud.com")
		body := "rules:\n  - {id: A, when: x, actions: [keep], applies_to: me@icloud.com, mailbox_removed: true}\n"
		r := e.do(http.MethodPost, "/api/rules/import", body)
		if r.status != http.StatusBadRequest || r.body.Error.Path != "rules[0].mailbox_removed" || !strings.Contains(r.body.Error.Message, `Rule 1 ("A"): mailbox_removed says`) {
			t.Fatalf("import = %d %q %s", r.status, r.body.Error.Path, r.body.Error.Message)
		}
		if items := e.call(http.MethodGet, "/api/rules", "", http.StatusOK)["items"].([]any); len(items) != 0 {
			t.Errorf("a refused file stored %d rules", len(items))
		}
	})
	t.Run("enabled: true does not switch a marked rule on", func(t *testing.T) {
		e := newEnv(t)
		e.signIn()
		e.call(http.MethodPost, "/api/rules/import", "rules:\n  - {id: A, when: x, actions: [keep], enabled: true, mailbox_removed: true}\n", http.StatusOK)
		if r := e.ruleNamed("A"); r["enabled"] != false || r["mailbox_removed"] != true {
			t.Errorf("A = %v", r)
		}
	})
	t.Run("a file that chooses neither a mailbox nor new conditions leaves the mark", func(t *testing.T) {
		e := removedEnv(t)
		e.call(http.MethodPost, "/api/rules/import", "rules:\n  - {id: Work only, when: Invoices, actions: [\"move:Money\"]}\n", http.StatusOK)
		if r := e.ruleNamed("Work only"); r["enabled"] != false || r["mailbox_removed"] != true {
			t.Errorf("Work only = %v", r)
		}
		e.call(http.MethodPost, "/api/rules/import", "rules:\n  - {id: Work only, when: Invoices, actions: [\"move:Money\"], applies_to: me@icloud.com}\n", http.StatusOK)
		if r := e.ruleNamed("Work only"); r["enabled"] != true || r["mailbox_removed"] != false || r["account_id"] != float64(2) {
			t.Errorf("after applies_to, Work only = %v", r)
		}
	})
}
