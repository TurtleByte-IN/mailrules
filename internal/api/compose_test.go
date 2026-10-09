package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/models"
	"github.com/TurtleByte-IN/mailrules/internal/telemetry"
)

// count is how many rows a query counts.
func (e *env) count(query string) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRowContext(e.t.Context(), query).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) hasFolder(name string) bool {
	e.t.Helper()
	folders, _ := e.mb.Folders(e.t.Context())
	for _, f := range folders {
		if f.Name == name {
			return true
		}
	}
	return false
}

// bySubject makes the fake decision model pick the candidate whose name the subject mentions.
func bySubject(reason string) func(models.DecideRequest) (models.Decision, models.Usage, error) {
	return func(req models.DecideRequest) (models.Decision, models.Usage, error) {
		u := models.Usage{Provider: "fake", Model: "fake-1", TokensIn: 100, TokensOut: 5, CostUSD: 0.001}
		for _, c := range req.Candidates {
			if strings.Contains(strings.ToLower(req.Email.Subject), strings.ToLower(c.Name)) {
				return models.Decision{RuleID: c.RuleID, Confidence: 0.95, Reason: reason + c.Name}, u, nil
			}
		}
		return models.Decision{Confidence: 0.9}, u, nil
	}
}

const foodRule = `{"name":"Food","said":"Swiggy to Food","conditions":{"field":"from_domain","op":"eq","value":"swiggy.in"},"actions":[{"type":"move","folder":"Food"},{"type":"read"}],"new_folders":["Food"]}`

// Saving rules needs no model at all: this is how the condition builder and the template
// gallery create a rule, one at a time, and how composer drafts are saved together.
func TestRulesBatch(t *testing.T) {
	e := newEnv(t)
	e.noDecider = true
	e.connect()

	saved := e.call(http.MethodPost, "/api/rules/batch", `{"rules":[`+foodRule+`]}`, http.StatusCreated)["items"].([]any)
	food := saved[0].(map[string]any)
	conform(t, e.doc, "Rule", food)
	if len(saved) != 1 || id(food["id"]) != 1 || food["priority"] != float64(1) || food["enabled"] != true || food["intent"] != "" || food["said"] != "Swiggy to Food" || !e.hasFolder("Food") {
		t.Fatalf("saved = %v; Food folder exists: %v", saved, e.hasFolder("Food"))
	}
	// The new folder is on record at once, before the next folder discovery.
	if n := len(e.call(http.MethodGet, "/api/accounts/1/folders", "", http.StatusOK)["items"].([]any)); n != 5 {
		t.Errorf("the account lists %d folders, want 5 with Food", n)
	}

	// One invalid rule saves nothing, and creates no folder either.
	jobs := `{"name":"Jobs","intent":"Recruiter outreach","actions":[{"type":"move","folder":"Jobs"}],"new_folders":["Jobs"]}`
	bad := `{"name":"Bad","conditions":{"all":[{"field":"subject","op":"sounds_like","value":"x"}]},"actions":[{"type":"keep"}]}`
	e.refuse(http.MethodPost, "/api/rules/batch", `{"rules":[`+jobs+`,`+bad+`]}`, http.StatusBadRequest, "rule_invalid", "rules[1].conditions.all[0].op")
	if n := len(e.call(http.MethodGet, "/api/rules", "", http.StatusOK)["items"].([]any)); n != 1 || e.hasFolder("Jobs") {
		t.Fatalf("after a refused batch: %d rules, Jobs folder exists: %v", n, e.hasFolder("Jobs"))
	}
	for body, want := range map[string][2]string{
		`{"rules":[]}`:            {"invalid_input", "rules"},
		`{"rules":[{"nam":"x"}]}`: {"invalid_input", "rules[0]"},
		`{"rules":[{"name":"x","actions":[{"type":"keep"}],"conditions":{"feild":"from"}}]}`:     {"invalid_input", "rules[0]"},
		`{"rules":[{"name":"x","intent":"y","actions":[{"type":"keep"}],"account_id":99}]}`:      {"invalid_input", "rules[0].account_id"},
		`{"rules":[{"name":"x","intent":"y","actions":[{"type":"keep"}],"model":"gpt"}]}`:        {"rule_invalid", "rules[0].model"},
		`{"rules":[{"name":"x","intent":"y","actions":[{"type":"keep"}],"model":"ollama"}]}`:     {"rule_invalid", "rules[0].model"},
		`{"rules":[{"name":"x","intent":"y","actions":[{"type":"keep"}],"new_folders":["a*"]}]}`: {"rule_invalid", "rules[0].new_folders[0]"},
		`{"rules":[{"name":"x","intent":"y","actions":[{"type":"trash"}]}]}`:                     {"rule_invalid", "rules[0].min_confidence"},
		`{"rules":[` + foodRule + `,{"name":" ","actions":[{"type":"keep"}]}]}`:                  {"rule_invalid", "rules[1].name"},
		`{"rules":[{"name":"x","actions":[{"type":"keep"}]}]}`:                                   {"rule_invalid", "rules[0].conditions"},
		`{"rule":[]}`: {"invalid_json", ""},
	} {
		e.refuse(http.MethodPost, "/api/rules/batch", body, http.StatusBadRequest, want[0], want[1])
	}

	// Two more, one dropped at the top of the order: priorities are renumbered, versions are not.
	first := `{"name":"VIP","conditions":{"field":"from","op":"eq","value":"boss@example.org"},"actions":[{"type":"flag"}],"position":0,"account_id":1,"stack":true,"enabled":false,"model":""}`
	saved = e.call(http.MethodPost, "/api/rules/batch", `{"rules":[`+jobs+`,`+first+`]}`, http.StatusCreated)["items"].([]any)
	if j, v := saved[0].(map[string]any), saved[1].(map[string]any); j["priority"] != float64(3) || v["priority"] != float64(1) || v["enabled"] != false ||
		v["stack"] != true || id(v["account_id"]) != 1 || j["intent"] != "Recruiter outreach" || !e.hasFolder("Jobs") {
		t.Fatalf("second batch = %v", saved)
	}
	list := e.call(http.MethodGet, "/api/rules", "", http.StatusOK)["items"].([]any)
	if names := fmt.Sprint(list[0].(map[string]any)["name"], list[1].(map[string]any)["name"], list[2].(map[string]any)["name"]); names != "VIPFoodJobs" ||
		list[1].(map[string]any)["version"] != float64(1) {
		t.Errorf("order after the second batch = %s", names)
	}

	// A rule saved this way sorts mail with no model set.
	e.deliver("noreply@swiggy.in", "order one")
	if it := e.item("order one", "acted"); it["decision"].(map[string]any)["stage"] != "condition" || e.folderOf("order one") != "Food" {
		t.Errorf("after the batch save: %v in %q", it, e.folderOf("order one"))
	}

	// In dry-run a save creates no folder: that is a mailbox change like any other.
	e.call(http.MethodPatch, "/api/settings", `{"dry_run":true}`, http.StatusOK)
	e.call(http.MethodPost, "/api/rules/batch", `{"rules":[{"name":"Later","intent":"Things for later","actions":[{"type":"move","folder":"Later"}],"new_folders":["Later"]}]}`, http.StatusCreated)
	if e.hasFolder("Later") {
		t.Error("a folder was created while dry-run is on")
	}
}

// Every gallery template saves as it is served (MAI-73). The rule names its template and
// has no words of the user's; an edit, a rewrite's new words, an export and an import keep
// the template.
func TestTemplateRules(t *testing.T) {
	e := newEnv(t)
	e.noDecider = true
	e.connect()

	var rs []string
	names := map[string]bool{}
	for _, it := range e.call(http.MethodGet, "/api/templates", "", http.StatusOK)["items"].([]any) {
		tpl := it.(map[string]any)
		conform(t, e.doc, "Template", tpl)
		b, _ := json.Marshal(tpl["rule"])
		rs = append(rs, string(b))
		names[tpl["name"].(string)] = true
	}
	saved := e.call(http.MethodPost, "/api/rules/batch", `{"rules":[`+strings.Join(rs, ",")+`]}`, http.StatusCreated)["items"].([]any)
	if len(saved) != 9 {
		t.Fatalf("saved %d of the 9 templates", len(saved))
	}
	for _, s := range saved {
		r := s.(map[string]any)
		conform(t, e.doc, "Rule", r)
		if r["said"] != "" || !names[r["template"].(string)] {
			t.Errorf("%v: said %q, template %q", r["name"], r["said"], r["template"])
		}
	}

	// MAI-106: Cold sales says "never emailed", so a sender you have replied to is excepted, as for Recruiters.
	for _, s := range saved {
		r := s.(map[string]any)
		if r["name"] != "Cold sales" {
			continue
		}
		if ex, _ := r["exceptions"].(map[string]any); ex["field"] != "replied_before" || ex["value"] != true {
			t.Errorf("Cold sales exceptions = %v, want replied_before", r["exceptions"])
		}
	}

	receipts := saved[1].(map[string]any)
	path := fmt.Sprintf("/api/rules/%d", id(receipts["id"]))
	patched := e.call(http.MethodPatch, path, `{"said":"also the ones from Amazon","enabled":false}`, http.StatusOK)["rule"].(map[string]any)
	if patched["template"] != "Receipts" || patched["said"] != "also the ones from Amazon" {
		t.Errorf("after an edit: %v", patched)
	}

	r := e.do(http.MethodGet, "/api/rules/export", "")
	if !strings.Contains(string(r.raw), "template: Receipts") {
		t.Fatalf("export lacks the template:\n%s", r.raw)
	}
	e.call(http.MethodPatch, path, `{"said":""}`, http.StatusOK)
	e.call(http.MethodPost, "/api/rules/import", string(r.raw), http.StatusOK)
	if back := e.call(http.MethodGet, path, "", http.StatusOK)["rule"].(map[string]any); back["template"] != "Receipts" || back["said"] != "also the ones from Amazon" {
		t.Errorf("after an import: %v", back)
	}
}

const fiveDrafts = `{"rules":[
 {"name":"Food","parts":[1],"intent":null,"conditions":{"all":[{"field":"from_domain","op":"in","value":["swiggy.in","zomato.com"]}]},"exceptions":{},"actions":[{"type":"move","folder":"Food"}],"min_confidence":null,"new_folders":["Food"],"question":null,"conflicts":[]},
 {"name":"Jobs","parts":[2],"intent":"Recruiter outreach about job openings","conditions":{},"exceptions":{"field":"replied_before","op":"eq","value":true},"actions":[{"type":"move","folder":"Jobs"}],"min_confidence":null,"new_folders":["Jobs"],"question":null,"conflicts":[]},
 {"name":"Scams","parts":[3],"intent":"Fake bank alerts and phishing","conditions":{},"exceptions":{},"actions":[{"type":"trash"}],"min_confidence":0.9,"new_folders":[],"question":"Trash, or move to Junk?","conflicts":[]},
 {"name":"Promos","parts":[],"intent":"Promotions","conditions":{},"exceptions":{},"actions":[{"type":"move","folder":"Promotions"}],"min_confidence":null,"new_folders":["Promotions"],"question":null,"conflicts":[]},
 {"name":"LinkedIn","parts":[4],"intent":null,"conditions":{"field":"from_domain","op":"eq","value":"linkedin.com"},"exceptions":{},"actions":[{"type":"archive"}],"min_confidence":null,"new_folders":[],"question":null,"conflicts":[]}
],"unparsed":[5]}`

const paragraph = "Put all Swiggy and Zomato stuff in Food, recruiter emails go to Jobs unless I've talked to them before, " +
	"trash anything that looks like a fake bank alert, and archive LinkedIn notifications. And do something about the rest."

func TestComposeAndReoptimize(t *testing.T) {
	e := newEnv(t)
	// Mail that is already there when the account is connected is left alone by live sorting.
	e.deliver("noreply@swiggy.in", "order one")
	e.deliver("orders@zomato.com", "order two")
	e.deliver("priya@talentbridge.in", "jobs at Acme")
	e.deliver("notifications@linkedin.com", "you appeared in 9 searches")
	e.connect()
	e.decider.DecideFunc = bySubject("The subject says ")

	// No generative model yet: the composer says what to do about it, and saves nothing.
	e.refuse(http.MethodPost, "/api/rules/compose", `{"text":"Put Swiggy in Food"}`, http.StatusConflict, "no_composer_model", "")
	if r := e.do(http.MethodPost, "/api/rules/compose", `{"text":"Put Swiggy in Food"}`); !strings.Contains(r.body.Error.Message, "Settings") {
		t.Errorf("the message does not point at Settings: %q", r.body.Error.Message)
	}

	var seen string
	answer := fiveDrafts
	e.gen = &models.Fake{GenerateFunc: func(_, user string, _ json.RawMessage, out any) (models.Usage, error) {
		seen = user
		return models.Usage{Provider: "anthropic", Model: "claude-haiku-4-5", TokensIn: 900, TokensOut: 400, CostUSD: 0.003}, json.Unmarshal([]byte(answer), out)
	}}
	for body, path := range map[string]string{
		`{"text":"  "}`: "text",
		`{"text":"` + strings.Repeat("x", 4001) + `"}`: "text",
		`{"text":"Put Swiggy in Food","account_id":9}`: "account_id",
	} {
		e.refuse(http.MethodPost, "/api/rules/compose", body, http.StatusBadRequest, "invalid_input", path)
	}

	body, _ := json.Marshal(map[string]any{"text": paragraph, "account_id": 1})
	res := e.call(http.MethodPost, "/api/rules/compose", string(body), http.StatusOK)
	conform(t, e.doc, "ComposeResult", res)
	drafts := res["rules"].([]any)
	if len(drafts) != 5 || len(res["unparsed"].([]any)) != 1 || !strings.Contains(seen, "Put all Swiggy") || !strings.Contains(seen, "Archive, INBOX, Sent, Trash") {
		t.Fatalf("compose = %v\nthe model saw:\n%s", res, seen)
	}
	want := map[string]float64{"Food": 2, "Jobs": 1, "Scams": 0, "Promos": 0, "LinkedIn": 1}
	var save []any
	for _, d := range drafts {
		d := d.(map[string]any)
		conform(t, e.doc, "RuleDraft", d)
		if d["match_count"] != want[d["name"].(string)] || len(d["samples"].([]any)) != int(want[d["name"].(string)]) {
			t.Errorf("draft %v: match_count %v, %d samples", d["name"], d["match_count"], len(d["samples"].([]any)))
		}
		// The invented folder is flagged on its own card; the other four are fine.
		if errs := d["errors"].([]any); (d["name"] == "Promos") != (len(errs) == 1) {
			t.Errorf("draft %v: errors %v", d["name"], errs)
		} else if len(errs) == 0 {
			save = append(save, map[string]any{"name": d["name"], "said": d["said"], "intent": d["intent"], "conditions": d["conditions"],
				"exceptions": d["exceptions"], "actions": d["actions"], "min_confidence": d["min_confidence"], "new_folders": d["new_folders"]})
		}
	}
	if s := drafts[0].(map[string]any)["samples"].([]any)[0].(map[string]any); s["from"] != "orders@zomato.com" || s["rule_id"] != nil || s["rule_name"] != "Food" || s["stage"] != "condition" {
		t.Errorf("sample = %v", s)
	}
	if n := e.count(`SELECT COUNT(*) FROM rules`); n != 0 || e.hasFolder("Food") {
		t.Fatalf("compose saved %d rules; Food folder exists: %v", n, e.hasFolder("Food"))
	}
	if n := e.count(`SELECT COALESCE(SUM(calls), 0) FROM usage_daily WHERE purpose = 'compose'`); n != 1 {
		t.Errorf("%d compose calls on the ledger, want 1", n)
	}

	// Approve the four good cards: one request, one transaction, their folders created.
	body, _ = json.Marshal(map[string]any{"rules": save})
	if saved := e.call(http.MethodPost, "/api/rules/batch", string(body), http.StatusCreated)["items"].([]any); len(saved) != 4 || !e.hasFolder("Food") || !e.hasFolder("Jobs") {
		t.Fatalf("saving the drafts = %v", saved)
	}

	// Re-optimize the first rule: one draft comes back, built from its first wording and the new text.
	answer = `{"rules":[{"name":"Food","parts":[1],"intent":null,"conditions":{"all":[{"field":"from_domain","op":"in","value":["swiggy.in","zomato.com"]}]},"exceptions":{},"actions":[{"type":"move","folder":"Food"},{"type":"read"}],"min_confidence":null,"new_folders":[],"question":null,"conflicts":[{"rule_id":2,"kind":"overlap","note":"x"}]}],"unparsed":[]}`
	re := e.call(http.MethodPost, "/api/rules/1/compose", `{"text":"and mark it read"}`, http.StatusOK)["rule"].(map[string]any)
	conform(t, e.doc, "RuleDraft", re)
	if len(re["actions"].([]any)) != 2 || re["match_count"] != float64(2) || len(re["conflicts"].([]any)) != 1 || len(re["new_folders"].([]any)) != 0 ||
		!strings.Contains(seen, "Put all Swiggy and Zomato stuff in Food") || !strings.Contains(seen, "and mark it read") || !strings.Contains(seen, `<rule id="2">`) {
		t.Errorf("re-optimize = %v\nthe model saw:\n%s", re, seen)
	}
	if r := e.call(http.MethodGet, "/api/rules/1", "", http.StatusOK)["rule"].(map[string]any); r["version"] != float64(1) || len(r["actions"].([]any)) != 1 {
		t.Errorf("re-optimize changed the saved rule: %v", r)
	}
	e.refuse(http.MethodPost, "/api/rules/99/compose", `{"text":"x"}`, http.StatusNotFound, "not_found", "")
	e.refuse(http.MethodPost, "/api/rules/1/compose", `{"text":""}`, http.StatusBadRequest, "invalid_input", "text")

	// A model that fails, or answers with something that is not a list of rules, is a 502.
	answer = `"I would rather not"`
	e.refuse(http.MethodPost, "/api/rules/compose", `{"text":"Put Swiggy in Food"}`, http.StatusBadGateway, "model_error", "")
	e.gen = nil
	e.refuse(http.MethodPost, "/api/rules/1/compose", `{"text":"x"}`, http.StatusConflict, "no_composer_model", "")
}

// sse posts like the web client does: it accepts a JSON body and an event stream.
func (e *env) sse(path, body string) reply {
	e.t.Helper()
	return e.send(e.t.Context(), http.MethodPost, path, body, http.Header{"Accept": {"application/json, text/event-stream"}})
}

// logs is what the daemon logged, as JSON lines.
type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// lines are the log lines so far, decoded.
func (l *logs) lines(t *testing.T) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(l.buf.String()), "\n") {
		var v map[string]any
		if line != "" && json.Unmarshal([]byte(line), &v) != nil {
			t.Fatalf("not a log line: %q", line)
		}
		if v != nil {
			out = append(out, v)
		}
	}
	return out
}

// find returns the first line whose message starts with msg.
func (l *logs) find(t *testing.T, msg string) map[string]any {
	t.Helper()
	for _, line := range l.lines(t) {
		if m, _ := line["msg"].(string); strings.HasPrefix(m, msg) {
			return line
		}
	}
	return nil
}

// captureLogs makes the daemon log at level into the returned buffer until the test ends,
// through the daemon's own logger, so a test sees what production writes after its
// redaction (a field named "body" or "token" is dropped there).
func captureLogs(t *testing.T, level slog.Level) *logs {
	t.Helper()
	l, old := &logs{}, slog.Default()
	slog.SetDefault(telemetry.NewLogger(l, level.String()))
	t.Cleanup(func() { slog.SetDefault(old) })
	return l
}

// events splits an event-stream body into its events, name and JSON data.
func streamEvents(t *testing.T, raw []byte) (names []string, data []map[string]any) {
	t.Helper()
	for _, block := range strings.Split(strings.TrimSpace(string(raw)), "\n\n") {
		name, payload, ok := strings.Cut(block, "\n")
		var v map[string]any
		if !ok || !strings.HasPrefix(name, "event: ") || json.Unmarshal([]byte(strings.TrimPrefix(payload, "data: ")), &v) != nil {
			t.Fatalf("not an event: %q", block)
		}
		names, data = append(names, strings.TrimPrefix(name, "event: ")), append(data, v)
	}
	return names, data
}

func TestRuleTester(t *testing.T) {
	e := newEnv(t)
	for i := range 150 {
		e.deliver("noreply@swiggy.in", fmt.Sprintf("order %d", i))
		e.deliver("hello@news.example", fmt.Sprintf("Reading issue %d", i))
	}
	e.connect()
	e.call(http.MethodPost, "/api/rules/import", rulesYAML, http.StatusOK) // 1 Food: swiggy.in by condition; 2 Reading: by intent
	e.decider.DecideFunc = bySubject("The subject says ")

	// The saved rule set as it is, over the default 200 newest emails.
	res := e.call(http.MethodPost, "/api/rules/test", `{"account_id":1}`, http.StatusOK)
	conform(t, e.doc, "TestResult", res)
	rows := res["results"].([]any)
	if res["tested"] != float64(200) || res["matched"] != float64(200) || res["model_calls"] != float64(100) || len(rows) != 200 || res["cost_usd"].(float64) < 0.0999 {
		t.Fatalf("test = tested %v matched %v calls %v cost %v", res["tested"], res["matched"], res["model_calls"], res["cost_usd"])
	}
	news, order := rows[0].(map[string]any), rows[1].(map[string]any)
	if news["subject"] != "Reading issue 149" || news["stage"] != "decider" || id(news["rule_id"]) != 2 || news["rule_name"] != "Reading" || news["confidence"] != 0.95 ||
		news["reason"] != "The subject says Reading" || news["review"] != false || news["actions"].([]any)[0].(map[string]any)["folder"] != "Reading" {
		t.Errorf("row 0 = %v", news)
	}
	if order["subject"] != "order 149" || order["stage"] != "condition" || id(order["rule_id"]) != 1 || len(order["actions"].([]any)) != 2 || order["from"] != "noreply@swiggy.in" {
		t.Errorf("row 1 = %v", order)
	}

	// One saved rule on its own, a draft tested on its own, and a rule's own model.
	only := e.call(http.MethodPost, "/api/rules/test", `{"account_id":1,"rule_ids":[1],"limit":10,"folder":"INBOX"}`, http.StatusOK)
	if only["tested"] != float64(10) || only["matched"] != float64(5) || only["model_calls"] != float64(0) {
		t.Errorf("rule 1 alone = %v", only)
	}
	draft := e.call(http.MethodPost, "/api/rules/test", `{"account_id":1,"limit":4,"rule_ids":[],"rules":[{"name":"Issues","conditions":{"field":"subject","op":"contains","value":"issue"},"actions":[{"type":"flag"}]}]}`, http.StatusOK)
	if row := draft["results"].([]any)[0].(map[string]any); draft["matched"] != float64(2) || row["rule_id"] != nil || row["rule_name"] != "Issues" || row["stage"] != "condition" {
		t.Errorf("draft test = %v", draft)
	}
	e.refuse(http.MethodPatch, "/api/rules/2", `{"model":"gpt"}`, http.StatusBadRequest, "rule_invalid", "model")
	e.call(http.MethodPatch, "/api/rules/2", `{"model":"clef"}`, http.StatusOK)
	e.own = map[string]*models.Fake{"clef": {NameValue: "clef", DecideFunc: bySubject("Clef says ")}}
	if row := e.call(http.MethodPost, "/api/rules/test", `{"account_id":1,"limit":1}`, http.StatusOK)["results"].([]any)[0].(map[string]any); row["reason"] != "Clef says Reading" {
		t.Errorf("a rule's own model was not asked: %v", row)
	}
	e.own = nil // that model cannot be used: the default decides

	// A client that accepts an event stream gets one at any size: progress from 0 of 300,
	// then the result (TestRuleProgress goes through the sizes and the rest of the contract).
	r := e.sse("/api/rules/test", `{"account_id":1,"limit":300}`)
	if r.status != http.StatusOK || r.header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream = %d %s %s", r.status, r.header.Get("Content-Type"), r.raw)
	}
	names, data := streamEvents(t, r.raw)
	if last := len(names) - 1; names[0] != "progress" || names[last] != "done" || data[last]["tested"] != float64(300) || data[last]["model_calls"] != float64(150) {
		t.Fatalf("stream events = %v", names)
	}
	// A run that cannot start is plain JSON, whatever the client accepts.
	if r := e.sse("/api/rules/test", `{"account_id":1,"limit":300,"folder":"Nope"}`); r.status != http.StatusBadRequest || r.header.Get("Content-Type") != "application/json" || r.body.Error.Path != "folder" {
		t.Errorf("a refused run = %d %s %s", r.status, r.header.Get("Content-Type"), r.raw)
	}

	// The tester only read: every email is where it was, unread, and nothing was recorded
	// about it. The calls it made are on the ledger as tests.
	refs, _, err := e.mb.FetchSince(t.Context(), "INBOX", time.Time{}, 0)
	if err != nil || len(refs) != 300 {
		t.Fatalf("INBOX holds %d emails (%v), want 300", len(refs), err)
	}
	for _, ref := range refs {
		if flags, _ := e.mb.Flags(t.Context(), ref); len(flags) != 0 {
			t.Fatalf("email %d has flags %v after the tests", ref.UID, flags)
		}
	}
	for _, table := range []string{"actions", "messages", "decisions", "batches"} {
		if n := e.count(`SELECT COUNT(*) FROM ` + table); n != 0 {
			t.Errorf("the tester wrote %d rows to %s", n, table)
		}
	}
	if e.hasFolder("Food") || e.hasFolder("Reading") {
		t.Error("the tester created a folder")
	}
	if n := e.count(`SELECT COALESCE(SUM(calls), 0) FROM usage_daily WHERE purpose = 'test'`); n != 100+1+150 || e.count(`SELECT COUNT(*) FROM usage_daily WHERE purpose <> 'test'`) != 0 {
		t.Errorf("%d test calls on the ledger, want 251, and none under another purpose", n)
	}

	for body, want := range map[string][3]string{
		`{"account_id":9}`:                 {"400", "invalid_input", "account_id"},
		`{}`:                               {"400", "invalid_input", "account_id"},
		`{"account_id":1,"limit":0}`:       {"400", "invalid_input", "limit"},
		`{"account_id":1,"limit":2001}`:    {"400", "invalid_input", "limit"},
		`{"account_id":1,"rule_ids":[99]}`: {"400", "invalid_input", "rule_ids"},
		`{"account_id":1,"folder":"Nope"}`: {"400", "invalid_input", "folder"},
		`{"account_id":1,"rules":[{"name":"x","intent":"y","actions":[]}]}`: {"400", "rule_invalid", "rules[0].actions"},
		`{"account_id":1,"since":1}`:                                        {"400", "invalid_json", ""},
	} {
		e.refuse(http.MethodPost, "/api/rules/test", body, http.StatusBadRequest, want[1], want[2])
	}
	// A model that fails ends the test with a 502, not a half result.
	e.decider.DecideFunc = func(models.DecideRequest) (models.Decision, models.Usage, error) {
		return models.Decision{}, models.Usage{}, &models.StatusError{Provider: "fake", Code: 503}
	}
	e.refuse(http.MethodPost, "/api/rules/test", `{"account_id":1,"limit":5}`, http.StatusBadGateway, "model_error", "")
	// Midway through a stream there is no status left to send: the stream ends with an error event.
	var asked atomic.Int32
	e.decider.DecideFunc = func(req models.DecideRequest) (models.Decision, models.Usage, error) {
		if asked.Add(1) > 60 {
			return models.Decision{}, models.Usage{}, &models.StatusError{Provider: "fake", Code: 503}
		}
		return bySubject("")(req)
	}
	names, data = streamEvents(t, e.sse("/api/rules/test", `{"account_id":1,"limit":300,"rule_ids":[1,2]}`).raw)
	if last := len(names) - 1; names[last] != "error" || data[last]["error"].(map[string]any)["code"] != "test_failed" {
		t.Errorf("a stream that fails midway ends with %v %v", names[last], data[last])
	}

	// Rules with an intent need a decision model; conditions alone do not.
	e.noDecider = true
	e.refuse(http.MethodPost, "/api/rules/test", `{"account_id":1}`, http.StatusConflict, "no_composer_model", "")
	if r := e.do(http.MethodPost, "/api/rules/test", `{"account_id":1}`); !strings.Contains(r.body.Error.Message, "decision model") || strings.Contains(r.body.Error.Message, "Claude") {
		t.Errorf("the tester's sentence does not name the decision model: %q", r.body.Error.Message)
	}
	// MAI-22: a draft with conditions and no intent is tested on its own, so the saved
	// rule with an intent (Reading) does not make it need a model. A draft with an intent does.
	alone := e.call(http.MethodPost, "/api/rules/test", `{"account_id":1,"limit":4,"rules":[{"name":"Issues","conditions":{"field":"subject","op":"contains","value":"issue"},"actions":[{"type":"flag"}]}]}`, http.StatusOK)
	if alone["tested"] != float64(4) || alone["matched"] != float64(2) || alone["model_calls"] != float64(0) {
		t.Errorf("a conditions-only draft without a model = %v", alone)
	}
	for _, row := range alone["results"].([]any) {
		if row := row.(map[string]any); row["rule_name"] != "Issues" && row["stage"] != "none" {
			t.Errorf("a rule that was not named took part in a draft's test: %v", row)
		}
	}
	e.refuse(http.MethodPost, "/api/rules/test", `{"account_id":1,"rules":[{"name":"News","intent":"Newsletters","actions":[{"type":"flag"}]}]}`, http.StatusConflict, "no_composer_model", "")
	if got := e.call(http.MethodPost, "/api/rules/test", `{"account_id":1,"rule_ids":[1],"limit":2}`, http.StatusOK); got["tested"] != float64(2) {
		t.Errorf("a condition-only test without a model = %v", got)
	}
	e.call(http.MethodPatch, "/api/accounts/1", `{"paused":true}`, http.StatusOK)
	e.refuse(http.MethodPost, "/api/rules/test", `{"account_id":1,"rule_ids":[1]}`, http.StatusConflict, "account_offline", "")
}

// Every decider can be pointed at its server from the browser; the environment is the default.
func TestSettingsModelURLs(t *testing.T) {
	e := newEnv(t)
	e.signIn()
	if got := e.call(http.MethodGet, "/api/settings", "", http.StatusOK); got["openai_base_url"] != "" || got["ollama_url"] != "" {
		t.Fatalf("defaults = %v %v", got["openai_base_url"], got["ollama_url"])
	}
	for body, path := range map[string]string{`{"ollama_url":"localhost:11434"}`: "ollama_url", `{"openai_base_url":"ftp://x"}`: "openai_base_url", `{"ollama_url":7}`: "ollama_url"} {
		e.refuse(http.MethodPatch, "/api/settings", body, http.StatusBadRequest, "invalid_input", path)
	}
	got := e.call(http.MethodPatch, "/api/settings", `{"decider":"ollama","decider_model":"llama3.2","ollama_url":" http://localhost:11434 ","openai_base_url":"https://openrouter.ai/api/v1"}`, http.StatusOK)
	conform(t, e.doc, "Settings", got)
	if got["ollama_url"] != "http://localhost:11434" || got["openai_base_url"] != "https://openrouter.ai/api/v1" {
		t.Errorf("after patch = %v", got)
	}
	// Ollama needs no key: with its URL set the decider is ready, with no restart.
	if router, _ := e.sett.Live(t.Context(), 1); router == nil || router.Name() != "ollama" {
		t.Fatalf("the ollama decider is not in use: %v", router)
	}
	// A rule's own model is built from the same settings, and only when it can be used.
	if own := e.sett.RouterFor(t.Context(), 1, "ollama:phi4"); own == nil || own != e.sett.RouterFor(t.Context(), 1, "ollama:phi4") {
		t.Error("a rule's ollama model is not built, or is rebuilt on every email")
	}
	if e.sett.RouterFor(t.Context(), 1, "jev") != nil || e.sett.RouterFor(t.Context(), 1, "nope") != nil {
		t.Error("a model without its key, or an unknown one, is used")
	}
	// An empty string means no URL: the decider can no longer run.
	if got := e.call(http.MethodPatch, "/api/settings", `{"ollama_url":""}`, http.StatusOK); got["ollama_url"] != "" {
		t.Errorf("after clearing = %v", got["ollama_url"])
	}
	if router, _ := e.sett.Live(t.Context(), 1); router != nil || e.sett.RouterFor(t.Context(), 1, "ollama:phi4") != nil {
		t.Error("ollama is still in use after its URL was removed")
	}
	if _, err := e.sett.Composer(t.Context(), 1); err == nil {
		t.Error("a composer model without an Anthropic key")
	}
	e.call(http.MethodPatch, "/api/settings", `{"keys":{"anthropic_api_key":"sk-ant-test"}}`, http.StatusOK)
	if gen, err := e.sett.Composer(t.Context(), 1); err != nil || gen == nil {
		t.Errorf("composer with a key: %v", err)
	}
}

// Progress comes at every size: 0 of N as soon as the mail is listed, then about a
// hundred steps at most, the last of them N of N, then the result.
func TestRuleTestProgress(t *testing.T) {
	e := newEnv(t)
	for i := range 150 {
		e.deliver("noreply@swiggy.in", fmt.Sprintf("order %d", i))
		e.deliver("hello@news.example", fmt.Sprintf("Reading issue %d", i))
	}
	e.connect()
	e.call(http.MethodPost, "/api/rules/import", rulesYAML, http.StatusOK)
	e.decider.DecideFunc = bySubject("The subject says ")

	for _, c := range []struct {
		name   string
		limit  int
		events int // progress events
		step   int
	}{
		{"a handful", 5, 6, 1},
		{"a quick look", 20, 21, 1},
		{"below the old streaming size", 150, 151, 1},
		{"the old one-body size", 200, 101, 2},
		{"a long run", 300, 101, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := e.sse("/api/rules/test", fmt.Sprintf(`{"account_id":1,"limit":%d}`, c.limit))
			if r.status != http.StatusOK || r.header.Get("Content-Type") != "text/event-stream" {
				t.Fatalf("answer = %d %s", r.status, r.header.Get("Content-Type"))
			}
			names, data := streamEvents(t, r.raw)
			n := len(names) - 1 // the last event is the result
			if n != c.events || names[n] != "done" {
				t.Fatalf("%d events ending in %q, want %d progress events then done", len(names), names[n], c.events)
			}
			prevDone, prevCalls := -c.step, 0.0
			for i, v := range data[:n] {
				conform(t, e.doc, "TestProgress", v)
				done, calls := v["done"].(float64), v["model_calls"].(float64)
				if names[i] != "progress" || v["total"] != float64(c.limit) || int(done) != prevDone+c.step && int(done) != c.limit || calls < prevCalls {
					t.Fatalf("event %d = %s %v after done %d", i, names[i], v, prevDone)
				}
				prevDone, prevCalls = int(done), calls
			}
			if data[0]["done"] != float64(0) || data[n-1]["done"] != float64(c.limit) || data[n-1]["model_calls"] != data[n]["model_calls"] {
				t.Errorf("progress runs from %v to %v (%v calls), the result has %v calls", data[0]["done"], data[n-1]["done"], data[n-1]["model_calls"], data[n]["model_calls"])
			}
			conform(t, e.doc, "TestResult", data[n])
			if data[n]["tested"] != float64(c.limit) {
				t.Errorf("tested = %v, want %d", data[n]["tested"], c.limit)
			}
		})
	}

	t.Run("one body for a client that does not accept a stream", func(t *testing.T) {
		for _, accept := range []string{"", "application/json", "*/*", "text/plain, application/json;q=0.9"} {
			h := http.Header{}
			if accept != "" {
				h.Set("Accept", accept)
			}
			for _, limit := range []int{20, 300} {
				r := e.send(t.Context(), http.MethodPost, "/api/rules/test", fmt.Sprintf(`{"account_id":1,"limit":%d}`, limit), h)
				if r.status != http.StatusOK || r.header.Get("Content-Type") != "application/json" || r.object(t)["tested"] != float64(limit) {
					t.Errorf("Accept %q, limit %d = %d %s", accept, limit, r.status, r.header.Get("Content-Type"))
				}
			}
		}
		for _, accept := range []string{"text/event-stream", "application/json, text/event-stream;q=0.5", "Text/Event-Stream"} {
			r := e.send(t.Context(), http.MethodPost, "/api/rules/test", `{"account_id":1,"limit":3}`, http.Header{"Accept": {accept}})
			if r.header.Get("Content-Type") != "text/event-stream" {
				t.Errorf("Accept %q = %s, want a stream", accept, r.header.Get("Content-Type"))
			}
		}
	})

	t.Run("a run that cannot start is JSON with its usual status", func(t *testing.T) {
		for _, c := range []struct {
			body, code, path string
			status           int
		}{
			{`{"account_id":1,"limit":20,"folder":"Nope"}`, "invalid_input", "folder", http.StatusBadRequest},
			{`{"account_id":1,"limit":2001}`, "invalid_input", "limit", http.StatusBadRequest},
			{`{"account_id":9}`, "invalid_input", "account_id", http.StatusBadRequest},
			{`{"account_id":1,"limit":0}`, "invalid_input", "limit", http.StatusBadRequest},
		} {
			r := e.sse("/api/rules/test", c.body)
			if r.status != c.status || r.header.Get("Content-Type") != "application/json" || r.body.Error.Code != c.code || r.body.Error.Path != c.path {
				t.Errorf("%s = %d %s %s", c.body, r.status, r.header.Get("Content-Type"), r.raw)
			}
		}
		e.noDecider = true
		defer func() { e.noDecider = false }()
		r := e.sse("/api/rules/test", `{"account_id":1,"limit":20}`)
		if r.status != http.StatusConflict || r.header.Get("Content-Type") != "application/json" || r.body.Error.Code != "no_composer_model" {
			t.Errorf("no model = %d %s %s", r.status, r.header.Get("Content-Type"), r.raw)
		}
	})
}

// A test writes a start and a finish line with what it did; a failing one says so; a
// client that drops the request is a plain fact, not an error.
func TestRuleTestLogs(t *testing.T) {
	e := newEnv(t)
	for i := range 20 {
		e.deliver("noreply@swiggy.in", fmt.Sprintf("order %d", i))
		e.deliver("hello@news.example", fmt.Sprintf("Reading issue %d", i))
	}
	e.connect()
	e.call(http.MethodPost, "/api/rules/import", rulesYAML, http.StatusOK)
	e.decider.DecideFunc = bySubject("The subject says ")

	t.Run("start and finish", func(t *testing.T) {
		l := captureLogs(t, slog.LevelDebug)
		e.sse("/api/rules/test", `{"account_id":1,"limit":20,"folder":"INBOX"}`)
		start, end := l.find(t, "rule test started"), l.find(t, "rule test finished")
		if start == nil || end == nil || start["level"] != "INFO" || end["level"] != "INFO" {
			t.Fatalf("start %v, finish %v", start, end)
		}
		if start["account"] != float64(1) || start["folder"] != "INBOX" || start["limit"] != float64(20) || start["rules"] != float64(2) || start["stream"] != true {
			t.Errorf("start line = %v", start)
		}
		for key, want := range map[string]any{"account": float64(1), "folder": "INBOX", "limit": float64(20), "rules": float64(2),
			"tested": float64(20), "matched": float64(20), "model_calls": float64(10)} {
			if end[key] != want {
				t.Errorf("finish line %s = %v, want %v", key, end[key], want)
			}
		}
		if cost, _ := end["cost_usd"].(float64); cost < 0.0099 {
			t.Errorf("finish line cost_usd = %v", end["cost_usd"])
		}
		if _, ok := end["duration_ms"].(float64); !ok {
			t.Errorf("finish line has no duration_ms: %v", end)
		}
		if p := l.find(t, "rule test progress"); p == nil || p["level"] != "DEBUG" {
			t.Errorf("no debug progress line: %v", p)
		}
		n := 0
		for _, line := range l.lines(t) {
			if line["msg"] == "rule test progress" {
				n++
			}
		}
		if n > 12 {
			t.Errorf("%d progress lines for one run of 20: it should be about one per tenth", n)
		}
		if l.find(t, "mail listed") == nil || l.find(t, "mail fetched") == nil {
			t.Error("the mail server reads are not logged")
		}
	})

	t.Run("an info level run does not write the debug lines", func(t *testing.T) {
		l := captureLogs(t, slog.LevelInfo)
		e.call(http.MethodPost, "/api/rules/test", `{"account_id":1,"limit":5}`, http.StatusOK)
		if l.find(t, "rule test finished") == nil || l.find(t, "rule test progress") != nil || l.find(t, "mail listed") != nil {
			t.Errorf("lines = %v", l.lines(t))
		}
	})

	t.Run("dropped by the client", func(t *testing.T) {
		l := captureLogs(t, slog.LevelDebug)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var asked atomic.Int32
		e.decider.DecideFunc = func(req models.DecideRequest) (models.Decision, models.Usage, error) {
			if asked.Add(1) == 5 {
				cancel() // the browser leaves after a few emails
			}
			return bySubject("")(req)
		}
		defer func() { e.decider.DecideFunc = bySubject("The subject says ") }()
		r := e.send(ctx, http.MethodPost, "/api/rules/test", `{"account_id":1,"limit":40}`, http.Header{"Accept": {"text/event-stream"}})
		if r.status != http.StatusOK { // the 200 and the first events went out before it left
			t.Errorf("status = %d", r.status)
		}
		line := l.find(t, "rule test cancelled by the client after ")
		if line == nil || line["level"] != "INFO" {
			t.Fatalf("no cancel line: %v", l.lines(t))
		}
		if total := line["total"]; total != float64(40) || line["done"].(float64) >= 40 || !strings.HasSuffix(line["msg"].(string), fmt.Sprintf(" of %d", 40)) {
			t.Errorf("cancel line = %v", line)
		}
		for _, other := range l.lines(t) {
			if other["level"] == "ERROR" || other["level"] == "WARN" || other["msg"] == "rule test finished" {
				t.Errorf("a dropped request logged %v", other)
			}
		}
	})

	t.Run("dropped before the run began", func(t *testing.T) {
		l := captureLogs(t, slog.LevelDebug)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		e.send(ctx, http.MethodPost, "/api/rules/test", `{"account_id":1,"limit":40}`, http.Header{"Accept": {"text/event-stream"}})
		if l.find(t, "request cancelled by the client") == nil && l.find(t, "rule test cancelled by the client after 0 of 0") == nil {
			t.Errorf("lines = %v", l.lines(t))
		}
		for _, line := range l.lines(t) {
			if line["level"] == "ERROR" {
				t.Errorf("logged %v", line)
			}
		}
	})

	t.Run("a request that fails is not called cancelled", func(t *testing.T) {
		l := captureLogs(t, slog.LevelInfo)
		e.decider.DecideFunc = func(models.DecideRequest) (models.Decision, models.Usage, error) {
			return models.Decision{}, models.Usage{}, &models.StatusError{Provider: "fake", Code: 503}
		}
		defer func() { e.decider.DecideFunc = bySubject("The subject says ") }()
		e.refuse(http.MethodPost, "/api/rules/test", `{"account_id":1,"limit":5}`, http.StatusBadGateway, "model_error", "")
		if f := l.find(t, "rule test failed"); f == nil || f["level"] != "WARN" || l.find(t, "rule test cancelled") != nil {
			t.Errorf("lines = %v", l.lines(t))
		}
	})
}
