package api

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// fieldPath is how a message used to begin: "rules[0].conditions.all[0].op: ...".
var fieldPath = regexp.MustCompile(`^[a-z_]+(\[\d+\])?([.:]|$)`)

// forAPerson says what stops a refusal message from being a sentence for a person, or "".
func forAPerson(msg, path string) string {
	first, _ := utf8.DecodeRuneInString(msg)
	switch {
	case msg == "":
		return "it is empty"
	case !unicode.IsUpper(first):
		return "it does not start with a capital"
	case !strings.HasSuffix(msg, "."):
		return "it does not end with a full stop"
	case fieldPath.MatchString(msg):
		return "it starts with a field path"
	case path != "" && strings.ContainsAny(path, ".[") && strings.Contains(msg, path):
		return "it repeats the field path"
	}
	return ""
}

// MAI-9: every refusal's message is a sentence for a person (capitalised, ending with a
// full stop, no field path in it), and the location is in path.
func TestRefusalMessagesAreSentences(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.call(http.MethodPost, "/api/rules/batch", `{"rules":[`+foodRule+`]}`, http.StatusCreated)
	rule := func(fields string) string { return `{"rules":[{"name":"x",` + fields + `}]}` }
	keep := `"actions":[{"type":"keep"}]`
	for _, tc := range []struct {
		name, method, url, body string
		code, path              string
		says                    string // part of the message; "" = any sentence
	}{
		// One rule edited.
		{"edit: no action", "PATCH", "/api/rules/1", `{"actions":[]}`, "rule_invalid", "actions", "A rule needs at least one action."},
		{"edit: no name", "PATCH", "/api/rules/1", `{"name":" "}`, "rule_invalid", "name", "A rule needs a name."},
		{"edit: unknown operator", "PATCH", "/api/rules/1", `{"conditions":{"all":[{"field":"subject","op":"sounds_like","value":"x"}]}}`, "rule_invalid", "conditions.all[0].op", `Unknown operator "sounds_like".`},
		{"edit: unknown field", "PATCH", "/api/rules/1", `{"conditions":{"field":"sender","op":"eq","value":"x"}}`, "rule_invalid", "conditions.field", `Unknown field "sender".`},
		{"edit: wrong value", "PATCH", "/api/rules/1", `{"conditions":{"field":"is_bulk","op":"eq","value":"yes"}}`, "rule_invalid", "conditions.value", "true or false"},
		{"edit: bad pattern", "PATCH", "/api/rules/1", `{"conditions":{"field":"subject","op":"matches","value":"("}}`, "rule_invalid", "conditions.value", "The pattern cannot be used"},
		{"edit: threshold", "PATCH", "/api/rules/1", `{"min_confidence":2}`, "rule_invalid", "min_confidence", "The confidence threshold must be between 0 and 1."},
		{"edit: unknown model", "PATCH", "/api/rules/1", `{"model":"gpt"}`, "rule_invalid", "model", "The model must be empty, or one of"},
		{"edit: folder", "PATCH", "/api/rules/1", `{"actions":[{"type":"move","folder":""}]}`, "rule_invalid", "actions[0].folder", "A folder name must be 1 to 200 characters."},
		{"edit: no such account", "PATCH", "/api/rules/1", `{"account_id":9}`, "invalid_input", "account_id", "No such account."},
		// A batch.
		{"batch: no action", "POST", "/api/rules/batch", rule(`"intent":"y","actions":[]`), "rule_invalid", "rules[0].actions", "A rule needs at least one action."},
		{"batch: nothing to match", "POST", "/api/rules/batch", rule(keep), "rule_invalid", "rules[0].conditions", "A rule needs conditions, an intent, or both."},
		{"batch: trash on intent", "POST", "/api/rules/batch", rule(`"intent":"y","actions":[{"type":"trash"}]`), "rule_invalid", "rules[0].min_confidence", "confidence threshold of at least 0.85"},
		{"batch: stacking intent", "POST", "/api/rules/batch", rule(`"intent":"y","stack":true,` + keep), "rule_invalid", "rules[0].stack", "A stacking rule is condition-only"},
		{"batch: unknown action", "POST", "/api/rules/batch", rule(`"intent":"y","actions":[{"type":"shred"}]`), "rule_invalid", "rules[0].actions[0].type", `Unknown action "shred".`},
		{"batch: model", "POST", "/api/rules/batch", rule(`"intent":"y","model":"ollama",` + keep), "rule_invalid", "rules[0].model", "The ollama decider has no default model."},
		{"batch: new folder", "POST", "/api/rules/batch", rule(`"intent":"y","new_folders":["a*"],` + keep), "rule_invalid", "rules[0].new_folders[0]", "A folder name must not contain wildcards"},
		{"batch: wrong shape", "POST", "/api/rules/batch", `{"rules":[{"nam":"x"}]}`, "invalid_input", "rules[0]", ""},
		// An import names the rule in the sentence, never the field path.
		{"import: one bad rule", "POST", "/api/rules/import", "rules:\n  - {id: Fine, match: {is_bulk: true}, actions: [keep]}\n  - {id: ok2, match: {is_bulk: true}, actions: [keep]}\n  - {id: Scams, match: {any: [{field: subject, op: like, value: x}]}, actions: [keep]}\n",
			"rule_invalid", "conditions.any[0].op", `Rule 3 ("Scams"): Unknown operator "like".`},
		{"import: model", "POST", "/api/rules/import", "rules:\n  - {id: Scams, when: scams, model: gpt, actions: [keep]}\n", "rule_invalid", "model", `Rule 1 ("Scams"): The model must be empty`},
		{"import: not yaml", "POST", "/api/rules/import", "rules: [", "rule_invalid", "", "could not be read as a rules file"},
		// Settings.
		{"settings: ollama url", "PATCH", "/api/settings", `{"ollama_url":"localhost"}`, "invalid_input", "ollama_url", "Enter an http or https URL"},
		{"settings: openai url", "PATCH", "/api/settings", `{"openai_base_url":"ftp://x"}`, "invalid_input", "openai_base_url", "Enter an http or https URL"},
		{"settings: decider", "PATCH", "/api/settings", `{"decider":"nope"}`, "invalid_input", "decider", "The decision model must be one of"},
		{"settings: threshold", "PATCH", "/api/settings", `{"min_confidence":2}`, "invalid_input", "min_confidence", "between 0 and 1"},
		{"settings: retention", "PATCH", "/api/settings", `{"retention_days":0}`, "invalid_input", "retention_days", "Retention must be between"},
		{"settings: unknown key", "PATCH", "/api/settings", `{"keys":{"nope":"x"}}`, "invalid_input", "keys.nope", "Unknown key."},
		// The tester.
		{"test: no account", "POST", "/api/rules/test", `{}`, "invalid_input", "account_id", "An account is required"},
		{"test: limit", "POST", "/api/rules/test", `{"account_id":1,"limit":0}`, "invalid_input", "limit", "The limit must be between"},
		{"test: draft", "POST", "/api/rules/test", `{"account_id":1,"rules":[{"name":"x","intent":"y","actions":[]}]}`, "rule_invalid", "rules[0].actions", "A rule needs at least one action."},
		// Lists.
		{"activity: limit", "GET", "/api/activity?limit=0", "", "invalid_input", "limit", ""},
		{"activity: account", "GET", "/api/activity?account=x", "", "invalid_input", "account", ""},
		{"senders: sort", "GET", "/api/senders?sort=x", "", "invalid_input", "sort", ""},
		{"senders: limit", "GET", "/api/senders?limit=0", "", "invalid_input", "limit", ""},
		{"batches: cursor", "GET", "/api/batches?cursor=x", "", "invalid_input", "cursor", ""},
		{"cleanup: limit", "POST", "/api/cleanup/preview", `{"account_id":1,"limit":0}`, "invalid_input", "limit", ""},
		{"sender: verdict", "PUT", "/api/senders/domain/a.example", `{"verdict":"x"}`, "invalid_input", "verdict", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := e.do(tc.method, tc.url, tc.body)
			got := r.body.Error
			if r.status != http.StatusBadRequest || got.Code != tc.code || got.Path != tc.path {
				t.Fatalf("= %d %q path %q (%s), want 400 %q path %q", r.status, got.Code, got.Path, got.Message, tc.code, tc.path)
			}
			if !strings.Contains(got.Message, tc.says) {
				t.Errorf("message %q does not say %q", got.Message, tc.says)
			}
			for _, line := range strings.Split(got.Message, "\n") {
				if why := forAPerson(line, got.Path); why != "" {
					t.Errorf("message %q: %s", line, why)
				}
			}
		})
	}
}
