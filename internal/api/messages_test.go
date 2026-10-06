package api

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/models"
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

// MAI-10: one call undoes everything done to one email, as one undo batch. An action that
// cannot be undone does not stop the others, and the request is refused only when none
// could be undone.
func TestUndoOneMessage(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.call(http.MethodPost, "/api/rules/batch", `{"rules":[`+foodRule+`,
		{"name":"Boss","conditions":{"field":"from_domain","op":"eq","value":"work.example"},"actions":[{"type":"keep"},{"type":"flag"}]}]}`, http.StatusCreated)
	undo := func(msg any) string { return fmt.Sprintf("/api/messages/%d/undo", id(msg)) }
	statuses := func(item map[string]any) (s string) {
		for _, a := range item["actions"].([]any) {
			s += a.(map[string]any)["status"].(string) + " "
		}
		return s
	}

	// Moved and marked read: both are undone by the one call, newest first.
	e.deliver("noreply@swiggy.in", "order one")
	one := e.item("order one", "acted")
	res := e.call(http.MethodPost, undo(one["id"]), "", http.StatusOK)
	conform(t, e.doc, "MessageUndoResult", res)
	item := res["item"].(map[string]any)
	if res["undone"] != float64(2) || res["failed"] != float64(0) || statuses(item) != "undone undone " || item["undoable"] != false || e.folderOf("order one") != "INBOX" {
		t.Fatalf("undo = %v undone, %v failed, actions %q, in %q", res["undone"], res["failed"], statuses(item), e.folderOf("order one"))
	}
	if b := e.call(http.MethodGet, fmt.Sprintf("/api/batches/%d", id(res["batch_id"])), "", http.StatusOK)["batch"].(map[string]any); b["kind"] != "undo" || b["status"] != "done" || b["done"] != float64(2) {
		t.Errorf("the undo batch = %v", b)
	}
	// Nothing is in effect any more: a second call does nothing and says so.
	if again := e.call(http.MethodPost, undo(one["id"]), "", http.StatusOK); again["undone"] != float64(0) || again["failed"] != float64(0) {
		t.Errorf("a second undo = %v", again)
	}

	// The email was filed by hand since: the flag cannot be taken off, the keep still is
	// undone, and the answer says both.
	e.deliver("boss@work.example", "the plan")
	plan := e.item("the plan", "acted")
	ref, err := e.mb.FindByMessageID(t.Context(), "INBOX", "the-plan@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.mb.Move(t.Context(), ref, "Archive"); err != nil {
		t.Fatal(err)
	}
	half := e.call(http.MethodPost, undo(plan["id"]), "", http.StatusOK)
	if item := half["item"].(map[string]any); half["undone"] != float64(1) || half["failed"] != float64(1) || statuses(item) != "undone done " || item["undoable"] != true {
		t.Errorf("a half undo = %v undone, %v failed, actions %q", half["undone"], half["failed"], statuses(half["item"].(map[string]any)))
	}
	// Now nothing at all can be undone, because the email is gone.
	e.refuse(http.MethodPost, undo(plan["id"]), "", http.StatusConflict, "message_gone", "")
	e.refuse(http.MethodPost, "/api/messages/999/undo", "", http.StatusNotFound, "not_found", "")
}

// MAI-12 (a), (b), (c): a decider that cannot work yet is saved with a warning, not
// refused; null puts the environment's default back and an empty string is a value; and
// `keys` says where each key comes from.
func TestSettingsWarningsResetAndKeySources(t *testing.T) {
	e := newEnv(t)
	e.signIn()
	e.sett.Env.OllamaURL, e.sett.Env.AnthropicAPIKey = "http://from-env:11434", "sk-ant-from-env"
	warned := func(s map[string]any) (out string) {
		for _, w := range s["warnings"].([]any) {
			w := w.(map[string]any)
			if why := forAPerson(w["message"].(string), ""); why != "" {
				t.Errorf("warning %q: %s", w["message"], why)
			}
			out += fmt.Sprint(w["code"], "@", w["path"], " ")
		}
		return out
	}

	// A fresh install: Jev is chosen and has no key. That is a warning on GET too.
	got := e.call(http.MethodGet, "/api/settings", "", http.StatusOK)
	conform(t, e.doc, "Settings", got)
	if warned(got) != "decider_not_ready@keys.openrouter_api_key " {
		t.Errorf("warnings on a fresh install = %q", warned(got))
	}
	if k := got["keys"].(map[string]any); k["anthropic_api_key"] != "environment" || k["openrouter_api_key"] != "none" {
		t.Errorf("key sources = %v", k)
	}

	// The wizard picks Ollama first and gives the URL in a later step: saved, with a warning.
	got = e.call(http.MethodPatch, "/api/settings", `{"decider":"ollama","decider_model":"llama3.2","ollama_url":""}`, http.StatusOK)
	conform(t, e.doc, "Settings", got)
	if got["decider"] != "ollama" || got["ollama_url"] != "" || warned(got) != "decider_not_ready@ollama_url " {
		t.Fatalf("ollama without its URL = decider %v, url %q, warnings %q", got["decider"], got["ollama_url"], warned(got))
	}
	if warned(e.call(http.MethodGet, "/api/settings", "", http.StatusOK)) != "decider_not_ready@ollama_url " {
		t.Error("GET does not repeat the warning")
	}
	// null forgets the stored (empty) URL: the environment's is back, and the warning goes.
	got = e.call(http.MethodPatch, "/api/settings", `{"ollama_url":null}`, http.StatusOK)
	if got["ollama_url"] != "http://from-env:11434" || warned(got) != "" {
		t.Errorf("after ollama_url null = %q, warnings %q", got["ollama_url"], warned(got))
	}

	// The same rule for every setting: "" is a value where empty means something, refused
	// where it cannot work; null is the environment's default.
	got = e.call(http.MethodPatch, "/api/settings", `{"fallback_model":"","composer_model":"my-composer","retention_days":90,"dry_run":false}`, http.StatusOK)
	if got["fallback_model"] != "" || got["composer_model"] != "my-composer" || got["retention_days"] != float64(90) || got["dry_run"] != false {
		t.Fatalf("after patch = %v", got)
	}
	e.refuse(http.MethodPatch, "/api/settings", `{"composer_model":""}`, http.StatusBadRequest, "invalid_input", "composer_model")
	e.refuse(http.MethodPatch, "/api/settings", `{"decider_model":""}`, http.StatusBadRequest, "invalid_input", "decider_model") // ollama has no default model
	got = e.call(http.MethodPatch, "/api/settings", `{"fallback_model":null,"composer_model":null,"retention_days":null,"dry_run":null,"decider":null,"decider_model":null}`, http.StatusOK)
	if got["fallback_model"] != "claude-haiku-4-5" || got["composer_model"] != "claude-haiku-4-5" || got["retention_days"] != float64(30) || got["dry_run"] != true ||
		got["decider"] != "jev" || got["decider_model"] != "" {
		t.Errorf("after null = %v", got)
	}
	if n := e.count(`SELECT COUNT(*) FROM settings WHERE key NOT LIKE 'key.%'`); n != 0 {
		t.Errorf("%d settings still stored after every one was reset", n)
	}

	// A stored key wins over the environment's and says so; removing it shows the environment's again.
	got = e.call(http.MethodPatch, "/api/settings", `{"keys":{"anthropic_api_key":"sk-ant-stored","openrouter_api_key":"sk-or-stored"}}`, http.StatusOK)
	if k := got["keys"].(map[string]any); k["anthropic_api_key"] != "stored" || k["openrouter_api_key"] != "stored" || warned(got) != "" {
		t.Errorf("after storing keys = %v, warnings %q", k, warned(got))
	}
	got = e.call(http.MethodPatch, "/api/settings", `{"keys":{"anthropic_api_key":"","openrouter_api_key":null}}`, http.StatusOK)
	if k := got["keys"].(map[string]any); k["anthropic_api_key"] != "environment" || k["openrouter_api_key"] != "none" {
		t.Errorf("after removing keys = %v", k)
	}
}

// MAI-12 (d): editing one rule checks its model exactly as a batch save and an import do.
func TestRuleModelIsCheckedTheSameEverywhere(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.call(http.MethodPost, "/api/rules/batch", `{"rules":[{"name":"News","intent":"Newsletters","actions":[{"type":"keep"}]}]}`, http.StatusCreated)
	for model, ok := range map[string]bool{"": true, "jev": true, " clef ": true, "clef:clef-flash": true, "ollama:llama3.2": true, "openai:gpt-4o-mini": true,
		"gpt": false, "ollama": false, "openai": false, "ollama:": false, ":x": false, "Jev": false} {
		want := http.StatusBadRequest
		if ok {
			want = http.StatusOK
		}
		patch := e.do(http.MethodPatch, "/api/rules/1", fmt.Sprintf(`{"model":%q}`, model))
		batch := e.do(http.MethodPost, "/api/rules/batch", fmt.Sprintf(`{"rules":[{"name":"Draft %s","intent":"x","actions":[{"type":"keep"}],"model":%q}]}`, model, model))
		if patch.status != want || (batch.status == http.StatusCreated) != ok {
			t.Errorf("model %q: edit answers %d, batch save %d; want accepted = %v by both", model, patch.status, batch.status, ok)
		}
		if !ok && (patch.body.Error.Message != batch.body.Error.Message || patch.body.Error.Code != "rule_invalid" || patch.body.Error.Path != "model" || batch.body.Error.Path != "rules[0].model") {
			t.Errorf("model %q: edit says %q at %q, batch save says %q at %q", model, patch.body.Error.Message, patch.body.Error.Path, batch.body.Error.Message, batch.body.Error.Path)
		}
	}
}

// MAI-11: what a feed row tells the UI, so the browser need not work it out.
func TestFeedRowSaysWhoWhatAndHow(t *testing.T) {
	e := newEnv(t)
	e.signIn() // dry-run is still on
	e.call(http.MethodPost, "/api/accounts", accountBody, http.StatusCreated)
	e.live()
	e.call(http.MethodPost, "/api/rules/batch", `{"rules":[`+foodRule+`,
		{"name":"Junk","conditions":{"field":"from_domain","op":"eq","value":"junk.example"},"actions":[{"type":"trash"}]},
		{"name":"Later","intent":"Things for later","actions":[{"type":"archive"}]}]}`, http.StatusCreated)
	e.decider.DecideFunc = func(models.DecideRequest) (models.Decision, models.Usage, error) {
		return models.Decision{RuleID: 3, Confidence: 0.4}, models.Usage{Provider: "fake", Model: "fake-1"}, nil
	}
	named := func(from, subject string) {
		e.mb.Deliver("INBOX", "From: "+from+"\r\nTo: me@example.test\r\nSubject: "+subject+"\r\nMessage-ID: <"+strings.ReplaceAll(subject, " ", "-")+"@example.test>\r\n\r\nbody\r\n")
	}

	// (a) the sender's display name; (c) the outcome as a sentence, also in dry-run.
	named(`"Swiggy Orders" <noreply@swiggy.in>`, "dry order")
	dry := e.item("dry order", "acted")
	if dry["from_name"] != "Swiggy Orders" || dry["from"] != "noreply@swiggy.in" || dry["outcome"] != "Would move to Food · mark read" {
		t.Errorf("dry-run row = name %q, outcome %q", dry["from_name"], dry["outcome"])
	}
	e.call(http.MethodPatch, "/api/settings", `{"dry_run":false}`, http.StatusOK)
	e.deliver("noreply@swiggy.in", "order one")
	e.deliver("spam@junk.example", "win big")
	e.deliver("friend@example.org", "lunch") // the model is unsure: Needs review
	named("someone@gmail.com", "hello")
	one, lunch := e.item("order one", "acted"), e.item("lunch", "review")
	for subject, want := range map[string]string{"order one": "Moved to Food · read", "win big": "Moved to Trash", "lunch": "In Inbox", "hello": "In Inbox"} {
		state := map[string]string{"lunch": "review", "hello": "review"}[subject]
		if state == "" {
			state = "acted"
		}
		if got := e.item(subject, state); got["outcome"] != want || got["from_name"] != "" {
			t.Errorf("%s: outcome %q, name %q; want %q and no name", subject, got["outcome"], got["from_name"], want)
		}
	}
	if d := e.call(http.MethodGet, fmt.Sprintf("/api/messages/%d", id(dry["id"])), "", http.StatusOK)["message"].(map[string]any); d["from_name"] != "Swiggy Orders" || d["outcome"] == "" {
		t.Errorf("message detail = name %q, outcome %q", d["from_name"], d["outcome"])
	}
	if r := e.call(http.MethodGet, "/api/review", "", http.StatusOK)["items"].([]any)[0].(map[string]any); r["outcome"] != "In Inbox" {
		t.Errorf("review item outcome = %q", r["outcome"])
	}
	undone := e.call(http.MethodPost, fmt.Sprintf("/api/messages/%d/undo", id(one["id"])), "", http.StatusOK)["item"].(map[string]any)
	if undone["outcome"] != "Undone · back in Inbox" {
		t.Errorf("after undo: outcome %q", undone["outcome"])
	}

	// (e) ?outcome= lists exactly what the Overview's four groups count.
	went := e.call(http.MethodGet, "/api/stats/summary", "", http.StatusOK)["went"].(map[string]any)
	for _, group := range []string{"sorted", "trashed", "inbox", "review"} {
		page := e.call(http.MethodGet, "/api/activity?outcome="+group, "", http.StatusOK)
		conform(t, e.doc, "ActivityPage", page)
		if n := len(page["items"].([]any)); float64(n) != went[group] {
			t.Errorf("?outcome=%s lists %d emails, the summary counts %v", group, n, went[group])
		}
	}
	if went["sorted"] != float64(1) || went["trashed"] != float64(1) || went["inbox"] != float64(1) || went["review"] != float64(2) {
		t.Errorf("went = %v, want the dry-run order sorted, one trashed, the undone order in the inbox, two in review", went)
	}
	e.refuse(http.MethodGet, "/api/activity?outcome=lost", "", http.StatusBadRequest, "invalid_input", "outcome")

	// (b) always for the address or the whole domain; never for a free-mail domain. (d) the
	// row says whether a person's last word was a review answer or a correction.
	resolve := fmt.Sprintf("/api/review/%d/resolve", id(lunch["id"]))
	e.refuse(http.MethodPost, resolve, `{"rule_id":null,"always_for":"everyone"}`, http.StatusBadRequest, "invalid_input", "always_for")
	answered := e.call(http.MethodPost, resolve, `{"rule_id":null,"always_for":"domain"}`, http.StatusOK)
	conform(t, e.doc, "FixResult", answered)
	if c := answered["item"].(map[string]any)["correction"].(map[string]any); c["kind"] != "review" || c["rule_id"] != nil {
		t.Errorf("after answering in Needs review: correction = %v", c)
	}
	if n := e.count(`SELECT COUNT(*) FROM sender_rules WHERE match_type = 'domain' AND value = 'example.org' AND verdict = 'keep' AND source = 'user'`); n != 1 {
		t.Errorf("%d domain sender rules for example.org, want 1", n)
	}
	hello := e.item("hello", "review")
	gmail := fmt.Sprintf("/api/review/%d/resolve", id(hello["id"]))
	e.refuse(http.MethodPost, gmail, `{"rule_id":1,"always_for":"domain"}`, http.StatusUnprocessableEntity, "domain_too_broad", "always_for")
	if e.item("hello", "review")["correction"] != nil || e.count(`SELECT COUNT(*) FROM sender_rules WHERE value LIKE '%gmail%'`) != 0 {
		t.Error("a refused domain rule changed something")
	}
	e.call(http.MethodPost, gmail, `{"rule_id":null,"always_for":"address"}`, http.StatusOK)
	if n := e.count(`SELECT COUNT(*) FROM sender_rules WHERE match_type = 'address' AND value = 'someone@gmail.com'`); n != 1 {
		t.Errorf("%d address sender rules for the gmail sender, want 1", n)
	}
	fixed := e.call(http.MethodPost, fmt.Sprintf("/api/messages/%d/correct", id(one["id"])), `{"rule_id":2,"always_for_sender":true}`, http.StatusOK)["item"].(map[string]any)
	if c := fixed["correction"].(map[string]any); c["kind"] != "correction" || fixed["outcome"] != "Moved to Trash" ||
		e.count(`SELECT COUNT(*) FROM sender_rules WHERE match_type = 'address' AND value = 'noreply@swiggy.in' AND rule_id = 2`) != 1 {
		t.Errorf("after a correction with always_for_sender: correction %v, outcome %q", c, fixed["outcome"])
	}
}

// MAI-11 (f): a correction the account cannot carry out has already undone what was done
// to the email. The stream says so: the undone actions, then the row as it now is.
func TestFailedCorrectionStillSendsItsEvents(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.call(http.MethodPost, "/api/rules/batch", `{"rules":[`+foodRule+`,{"name":"Archive it","intent":"Things to archive","actions":[{"type":"archive"}]}]}`, http.StatusCreated)
	e.decider.DecideFunc = func(models.DecideRequest) (models.Decision, models.Usage, error) {
		return models.Decision{}, models.Usage{Provider: "fake", Model: "fake-1"}, nil
	}
	e.deliver("noreply@swiggy.in", "order one")
	msg := id(e.item("order one", "acted")["id"])
	if _, err := e.db.ExecContext(t.Context(), `UPDATE folders SET special_use = NULL WHERE name = 'Archive'`); err != nil {
		t.Fatal(err)
	}
	_, live, cancel := e.hub.Subscribe(0)
	defer cancel()
	e.refuse(http.MethodPost, fmt.Sprintf("/api/messages/%d/correct", msg), `{"rule_id":2}`, http.StatusUnprocessableEntity, "no_special_folder", "")
	var names []string
	for len(live) > 0 {
		names = append(names, (<-live).Name)
	}
	if got := strings.Join(names, " "); got != "action.undone action.undone message.processed" {
		t.Errorf("events after the refused correction = %q", got)
	}
	if it := e.item("order one", "acted"); it["outcome"] != "Failed: no Archive folder" || it["undoable"] != false || e.folderOf("order one") != "INBOX" {
		t.Errorf("the row after the refused correction: outcome %q, undoable %v, in %q", it["outcome"], it["undoable"], e.folderOf("order one"))
	}
}

// MAI-13: the small contract gaps, one by one.
func TestContractGaps(t *testing.T) {
	e := newEnv(t)
	e.connect()

	// Two rules cannot share a name: not within a batch, and not with a saved rule.
	e.refuse(http.MethodPost, "/api/rules/batch", `{"rules":[`+foodRule+`,`+foodRule+`]}`, http.StatusBadRequest, "rule_invalid", "rules[1].name")
	e.call(http.MethodPost, "/api/rules/batch", `{"rules":[`+foodRule+`]}`, http.StatusCreated)
	r := e.do(http.MethodPost, "/api/rules/batch", `{"rules":[{"name":"Other","intent":"x","actions":[{"type":"keep"}]},`+foodRule+`]}`)
	if r.status != http.StatusBadRequest || r.body.Error.Path != "rules[1].name" || forAPerson(r.body.Error.Message, r.body.Error.Path) != "" || !strings.Contains(r.body.Error.Message, `"Food"`) {
		t.Errorf("a name that is already saved = %d %q at %q", r.status, r.body.Error.Message, r.body.Error.Path)
	}
	if n := e.count(`SELECT COUNT(*) FROM rules`); n != 1 {
		t.Errorf("%d rules after two refused batches, want 1", n)
	}
	// A draft may be tested under the name of the rule it would replace.
	e.call(http.MethodPost, "/api/rules/test", `{"account_id":1,"rules":[`+foodRule+`]}`, http.StatusOK)

	// A provider says what it calls the secret the user pastes.
	labels := map[string]any{}
	for _, p := range e.call(http.MethodGet, "/api/presets", "", http.StatusOK)["items"].([]any) {
		conform(t, e.doc, "Preset", p)
		labels[p.(map[string]any)["name"].(string)] = p.(map[string]any)["secret_label"]
	}
	if labels["icloud"] != "App-specific password" || labels["fastmail"] != "App password" || labels["generic"] != "Password" {
		t.Errorf("secret labels = %v", labels)
	}

	// last_event_at is the status change; last_mail_at is the mail.
	acct := e.call(http.MethodGet, "/api/accounts/1", "", http.StatusOK)["account"].(map[string]any)
	if acct["last_mail_at"] != nil || acct["last_event_at"] == nil {
		t.Errorf("before any mail: last_mail_at %v, last_event_at %v", acct["last_mail_at"], acct["last_event_at"])
	}
	e.mb.Deliver("INBOX", "From: noreply@swiggy.in\r\nDate: Mon, 11 Jan 2027 10:00:00 +0000\r\nSubject: order one\r\nMessage-ID: <order-one@example.test>\r\n\r\nbody\r\n")
	one := e.item("order one", "acted")
	acct = e.call(http.MethodGet, "/api/accounts/1", "", http.StatusOK)["account"].(map[string]any)
	conform(t, e.doc, "Account", acct)
	if acct["last_mail_at"] != one["received_at"] || acct["last_mail_at"] == nil {
		t.Errorf("last_mail_at = %v, want the email's received time %v", acct["last_mail_at"], one["received_at"])
	}

	// A stored account is checked on a connection of its own: the watcher is not restarted.
	before, _ := e.st.Account(t.Context(), 1)
	tested := e.call(http.MethodPost, "/api/accounts/1/test", "", http.StatusOK)
	conform(t, e.doc, "AccountTestResult", tested)
	if after, _ := e.st.Account(t.Context(), 1); tested["username"] != "me@example.test" || after.Status != "live" || after.LastEventAt != before.LastEventAt {
		t.Errorf("stored account test = %v; status %q, last event %d (was %d)", tested, after.Status, after.LastEventAt, before.LastEventAt)
	}
	e.connectErr = fmt.Errorf("login: %w", mail.ErrAuth)
	e.refuse(http.MethodPost, "/api/accounts/1/test", "", http.StatusUnprocessableEntity, "auth_failed", "password")
	if after, _ := e.st.Account(t.Context(), 1); after.Status != "live" {
		t.Errorf("a failed test changed the account's status to %q", after.Status)
	}
	e.connectErr = nil
	e.refuse(http.MethodPost, "/api/accounts/9/test", "", http.StatusNotFound, "not_found", "")

	// A cleanup preview's groups have keys that stay put.
	e.mb.AddFolder("Old", "")
	e.mb.Deliver("Old", "From: noreply@swiggy.in\r\nSubject: old order\r\n\r\nbody\r\n")
	e.mb.Deliver("Old", "From: friend@example.org\r\nSubject: old lunch\r\n\r\nbody\r\n")
	e.mb.Deliver("Old", "From: news@keep.example\r\nSubject: old news\r\n\r\nbody\r\n")
	e.call(http.MethodPut, "/api/senders/domain/keep.example", `{"verdict":"keep"}`, http.StatusOK)
	var keys []string
	for _, g := range e.call(http.MethodPost, "/api/cleanup/preview", `{"account_id":1,"folder":"Old"}`, http.StatusOK)["groups"].([]any) {
		keys = append(keys, g.(map[string]any)["key"].(string))
	}
	slices.Sort(keys)
	if got := strings.Join(keys, " "); got != "none rule:1 sender:keep" {
		t.Errorf("preview group keys = %q", got)
	}

	// A sender rule for an address with no mail has no name: the UI shows the address once.
	e.call(http.MethodPut, "/api/senders/address/quiet@nowhere.example", `{"verdict":"keep"}`, http.StatusOK)
	for _, s := range e.call(http.MethodGet, "/api/senders?source=user", "", http.StatusOK)["items"].([]any) {
		if s := s.(map[string]any); s["value"] == "quiet@nowhere.example" && (s["name"] != "" || s["messages"] != float64(0)) {
			t.Errorf("a sender with a rule and no mail = %v", s)
		}
	}

	// Usage by day says what each model was used for.
	if err := e.st.AddUsage(t.Context(), e.ck.now().UTC().Format(time.DateOnly), "anthropic", "claude-haiku-4-5", "escalate", 1, 100, 10, 0.01); err != nil {
		t.Fatal(err)
	}
	if err := e.st.AddUsage(t.Context(), e.ck.now().UTC().Format(time.DateOnly), "anthropic", "claude-haiku-4-5", "compose", 2, 100, 10, 0.02); err != nil {
		t.Fatal(err)
	}
	usage := e.call(http.MethodGet, "/api/stats/usage", "", http.StatusOK)
	conform(t, e.doc, "StatsUsage", usage)
	days := usage["days"].([]any)
	var purposes []string
	for _, m := range days[len(days)-1].(map[string]any)["models"].([]any) {
		purposes = append(purposes, m.(map[string]any)["purpose"].(string))
	}
	if got := strings.Join(purposes, " "); got != "compose escalate" {
		t.Errorf("today's models by purpose = %q", got)
	}
}

// MAI-13: what MailRules did stays undoable for 30 days, and not a day longer; and undoing
// a batch that only ever recorded dry-run actions is a no-op.
func TestUndoWindowAndDryRunBatches(t *testing.T) {
	e := newEnv(t)
	e.signIn() // dry-run is on
	e.call(http.MethodPost, "/api/accounts", accountBody, http.StatusCreated)
	e.live()
	e.call(http.MethodPost, "/api/rules/batch", `{"rules":[`+foodRule+`]}`, http.StatusCreated)
	e.deliver("noreply@swiggy.in", "dry order")
	dry := e.item("dry order", "acted")
	dryBatch := id(dry["actions"].([]any)[0].(map[string]any)["batch_id"])
	res := e.call(http.MethodPost, fmt.Sprintf("/api/batches/%d/undo", dryBatch), "", http.StatusOK)
	conform(t, e.doc, "UndoResult", res)
	if b := res["batch"].(map[string]any); res["undone"] != float64(0) || res["failed"] != float64(0) || b["status"] != "done" || b["actions"].(map[string]any)["dry_run"] != float64(2) {
		t.Errorf("undoing a dry-run batch = %v", res)
	}

	e.call(http.MethodPatch, "/api/settings", `{"dry_run":false}`, http.StatusOK)
	e.deliver("noreply@swiggy.in", "order one")
	one := e.item("order one", "acted")
	move := one["actions"].([]any)[0].(map[string]any)
	e.ck.t = e.ck.t.AddDate(0, 0, 29)
	if e.item("order one", "acted")["undoable"] != true {
		t.Error("an action 29 days old is no longer undoable")
	}
	e.ck.t = e.ck.t.AddDate(0, 0, 2) // 31 days on
	if e.item("order one", "acted")["undoable"] != false {
		t.Error("an action 31 days old still reads as undoable")
	}
	e.refuse(http.MethodPost, fmt.Sprintf("/api/actions/%d/undo", id(move["id"])), "", http.StatusConflict, "too_old", "")
	e.refuse(http.MethodPost, fmt.Sprintf("/api/batches/%d/undo", id(move["batch_id"])), "", http.StatusConflict, "too_old", "")
	e.refuse(http.MethodPost, fmt.Sprintf("/api/messages/%d/undo", id(one["id"])), "", http.StatusConflict, "too_old", "")
	if since := e.call(http.MethodPost, "/api/actions/undo?since=1", "", http.StatusOK); since["undone"] != float64(0) || since["failed"] != float64(0) || e.folderOf("order one") != "Food" {
		t.Errorf("undo since the beginning, 31 days on = %v; the email is in %q", since, e.folderOf("order one"))
	}
}
