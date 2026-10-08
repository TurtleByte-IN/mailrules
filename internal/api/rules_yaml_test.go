package api

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// addMailboxes connects accounts for these addresses, in this order, so their ids are 1, 2, ...
func (e *env) addMailboxes(addresses ...string) {
	e.t.Helper()
	for _, a := range addresses {
		acct := store.Account{UserID: 1, Label: "Mail of " + a, Preset: "generic", Host: "imap.example.test", Port: 993, TLSMode: "implicit", Username: a}
		if _, err := e.st.CreateAccount(e.t.Context(), make([]byte, 32), acct, "app-password-1234"); err != nil {
			e.t.Fatal(err)
		}
	}
}

// accountOf says, per rule name, the address of the mailbox the rule applies to ("" = all).
func (e *env) accountOf() map[string]string {
	e.t.Helper()
	addr := map[float64]string{}
	for _, a := range e.call(http.MethodGet, "/api/accounts", "", http.StatusOK)["items"].([]any) {
		m := a.(map[string]any)
		addr[m["id"].(float64)] = m["username"].(string)
	}
	out := map[string]string{}
	for _, r := range e.call(http.MethodGet, "/api/rules", "", http.StatusOK)["items"].([]any) {
		m := r.(map[string]any)
		id, _ := m["account_id"].(float64)
		out[m["name"].(string)] = addr[id]
	}
	return out
}

// accountConds says what the account conditions of the rule "Everywhere" (in scopedRules)
// name, as addresses: the one in match, then those in unless.
func (e *env) accountConds() (match string, unless []string) {
	e.t.Helper()
	addr := map[float64]string{}
	for _, a := range e.call(http.MethodGet, "/api/accounts", "", http.StatusOK)["items"].([]any) {
		m := a.(map[string]any)
		addr[m["id"].(float64)] = m["username"].(string)
	}
	for _, r := range e.call(http.MethodGet, "/api/rules", "", http.StatusOK)["items"].([]any) {
		m := r.(map[string]any)
		if m["name"] != "Everywhere" {
			continue
		}
		leaf := func(c any, path ...string) any {
			for _, p := range path {
				if i, err := strconv.Atoi(p); err == nil {
					c = c.([]any)[i]
				} else {
					c = c.(map[string]any)[p]
				}
			}
			return c.(map[string]any)["value"]
		}
		match = addr[leaf(m["conditions"], "any", "0").(float64)]
		for _, id := range leaf(m["exceptions"], "all", "1", "any", "0").([]any) {
			unless = append(unless, addr[id.(float64)])
		}
	}
	return match, unless
}

const scopedRules = `defaults:
  decision_model: jev
  fallback_model: claude-haiku-4-5
  min_confidence: 0.8
rules:
  - id: Work invoices
    match: {from_domain: acme.example}
    actions: ["move:Money"]
    applies_to: work@acme.example
  - id: Everywhere
    when: Newsletters
    match:
      any:
        - {field: account, op: eq, value: me@icloud.com}
        - {field: subject, op: contains, value: news}
    unless:
      all:
        - {field: is_bulk, op: eq, value: false}
        - any:
            - {field: account, op: in, value: [Work@Acme.example, me@icloud.com]}
    actions: ["move:Reading"]
  - id: Home bills
    match: {subject: bill}
    actions: ["move:Bills"]
    applies_to: Me@iCloud.com
    min_confidence: 0.9
`

// Exporting from one install and importing into another gives the same rules, including the
// mailbox each applies to and the mailboxes its account conditions name, even where the
// mailboxes have other ids; and the models in the defaults are the install's.
func TestRulesYAMLRoundTrip(t *testing.T) {
	a := newEnv(t)
	a.signIn()
	a.addMailboxes("work@acme.example", "me@icloud.com")
	a.call(http.MethodPost, "/api/rules/import", scopedRules, http.StatusOK)
	want := map[string]string{"Work invoices": "work@acme.example", "Everywhere": "", "Home bills": "me@icloud.com"}
	if got := a.accountOf(); !mapsEqual(got, want) {
		t.Fatalf("after import, rules apply to %v, want %v", got, want)
	}
	wantMatch, wantUnless := "me@icloud.com", []string{"work@acme.example", "me@icloud.com"}
	if m, u := a.accountConds(); m != wantMatch || !slices.Equal(u, wantUnless) {
		t.Fatalf("after import, the account conditions name %q and %q, want %q and %q", m, u, wantMatch, wantUnless)
	}

	r := a.do(http.MethodGet, "/api/rules/export", "")
	if r.status != http.StatusOK {
		t.Fatalf("export = %d %s", r.status, r.raw)
	}
	exported := string(r.raw)
	for _, line := range []string{"defaults:", "decision_model: jev", "fallback_model: claude-haiku-4-5", "applies_to: work@acme.example", "applies_to: me@icloud.com",
		"value: me@icloud.com", "- work@acme.example"} {
		if !strings.Contains(exported, line) {
			t.Errorf("export lacks %q:\n%s", line, exported)
		}
	}
	if strings.Count(exported, "applies_to:") != 2 {
		t.Errorf("want applies_to on the two scoped rules only:\n%s", exported)
	}

	// Another install with the same mailboxes added in the other order: ids 1 and 2 are swapped.
	b := newEnv(t)
	b.signIn()
	b.addMailboxes("me@icloud.com", "work@acme.example")
	got := b.call(http.MethodPost, "/api/rules/import", exported, http.StatusOK)
	if got["created"] != float64(3) {
		t.Fatalf("import = %v", got)
	}
	if got := b.accountOf(); !mapsEqual(got, want) {
		t.Errorf("after the round trip rules apply to %v, want %v", got, want)
	}
	if m, u := b.accountConds(); m != wantMatch || !slices.Equal(u, wantUnless) {
		t.Errorf("after the round trip the account conditions name %q and %q, want %q and %q", m, u, wantMatch, wantUnless)
	}
	if again := b.do(http.MethodGet, "/api/rules/export", ""); string(again.raw) != exported {
		t.Errorf("export of the import differs:\n%s\nwas\n%s", again.raw, exported)
	}

	// Importing the file back over the install it came from changes nothing.
	a.call(http.MethodPost, "/api/rules/import", exported, http.StatusOK)
	if got := a.accountOf(); !mapsEqual(got, want) {
		t.Errorf("after importing over itself, rules apply to %v, want %v", got, want)
	}
	if again := a.do(http.MethodGet, "/api/rules/export", ""); string(again.raw) != exported {
		t.Errorf("export after re-import differs:\n%s\nwas\n%s", again.raw, exported)
	}
}

// What an import refuses, and that it then stores nothing; and how an older file, with no
// applies_to, leaves a rule that is limited to a mailbox.
func TestRulesYAMLImportRefusals(t *testing.T) {
	tests := []struct {
		name, body, path string
		wantMsg          string
	}{
		{"a mailbox that is not connected", strings.Replace(scopedRules, "work@acme.example", "ghost@nowhere.example", 1),
			"rules[0].applies_to", `Rule 1 ("Work invoices"): no mailbox for ghost@nowhere.example is connected here`},
		{"a decision model this install does not use", strings.Replace(scopedRules, "decision_model: jev", "decision_model: clef", 1),
			"defaults.decision_model", `written for the decision model "clef", but this install uses "jev"`},
		{"a fallback model this install does not use", strings.Replace(scopedRules, "claude-haiku-4-5", "claude-sonnet-4-5", 1),
			"defaults.fallback_model", `written for the fallback model "claude-sonnet-4-5", but this install uses "claude-haiku-4-5"`},
		{"an account condition on a mailbox that is not connected", strings.Replace(scopedRules, "value: me@icloud.com}", "value: ghost@nowhere.example}", 1),
			"rules[1].match", `Rule 2 ("Everywhere"): no mailbox for ghost@nowhere.example is connected here, so its account condition cannot name it`},
		{"a mailbox number in an account condition", strings.Replace(scopedRules, "[Work@Acme.example, me@icloud.com]", "[1, me@icloud.com]", 1),
			"exceptions.all[1].any[0].value", `Rule 2 ("Everywhere"): In a rules file, an account condition names a mailbox by its email address`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.signIn()
			e.addMailboxes("work@acme.example", "me@icloud.com")
			r := e.do(http.MethodPost, "/api/rules/import", tt.body)
			if r.status != http.StatusBadRequest || r.body.Error.Code != "rule_invalid" || r.body.Error.Path != tt.path || !strings.Contains(r.body.Error.Message, tt.wantMsg) {
				t.Fatalf("import = %d %q path %q: %s; want 400 rule_invalid on %s saying %q", r.status, r.body.Error.Code, r.body.Error.Path, r.body.Error.Message, tt.path, tt.wantMsg)
			}
			if items := e.call(http.MethodGet, "/api/rules", "", http.StatusOK)["items"].([]any); len(items) != 0 {
				t.Errorf("a refused file stored %d rules", len(items))
			}
		})
	}

	t.Run("an older file keeps the mailbox a rule has", func(t *testing.T) {
		e := newEnv(t)
		e.signIn()
		e.addMailboxes("work@acme.example", "me@icloud.com")
		e.call(http.MethodPost, "/api/rules/import", scopedRules[strings.Index(scopedRules, "rules:"):strings.Index(scopedRules, "  - id: Home")], http.StatusOK)
		old := "rules:\n  - {id: Work invoices, match: {from_domain: acme.example}, actions: [\"move:Archive\"]}\n"
		e.call(http.MethodPost, "/api/rules/import", old, http.StatusOK)
		if got := e.accountOf(); got["Work invoices"] != "work@acme.example" {
			t.Errorf("an import without applies_to moved the rule to %q", got["Work invoices"])
		}
		e.call(http.MethodPost, "/api/rules/import", strings.TrimSuffix(old, "}\n")+", applies_to: all}\n", http.StatusOK)
		if got := e.accountOf(); got["Work invoices"] != "" {
			t.Errorf("applies_to: all left the rule on %q", got["Work invoices"])
		}
	})
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || v != w {
			return false
		}
	}
	return true
}
