package api

import (
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/models"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// Senders: who writes most, the rules MailRules learned by itself, the ones the user sets,
// and how often each has applied.
func TestSenders(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.call(http.MethodPost, "/api/rules/import", rulesYAML, http.StatusOK) // 1 Food: swiggy.in by condition; 2 Reading: by intent
	e.decider.DecideFunc = func(models.DecideRequest) (models.Decision, models.Usage, error) {
		return models.Decision{RuleID: 2, Confidence: 0.95}, models.Usage{Provider: "fake", Model: "fake-1"}, nil
	}
	for i := range 3 {
		e.deliver("Swiggy <noreply@swiggy.in>", fmt.Sprintf("order %d", i))
		e.item(fmt.Sprintf("order %d", i), "acted")
	}
	// Three confident, identical model decisions teach MailRules the sender; the next
	// two are routed by the learned rule, with no model call.
	for i := range 5 {
		e.mb.Deliver("INBOX", fmt.Sprintf("From: Weekly Digest <hello@news.example>\r\nTo: me@example.test\r\nSubject: issue %d\r\n"+
			"List-Unsubscribe: <https://news.example/u>\r\nMessage-ID: <issue-%d@example.test>\r\n\r\nNews.\r\n", i, i))
		e.item(fmt.Sprintf("issue %d", i), "acted")
	}

	list := e.call(http.MethodGet, "/api/senders", "", http.StatusOK)
	items := list["items"].([]any)
	if len(items) != 2 || list["next_cursor"] != nil {
		t.Fatalf("senders = %v", list)
	}
	news, swiggy := items[0].(map[string]any), items[1].(map[string]any)
	conform(t, e.doc, "Sender", news)
	if news["type"] != "address" || news["value"] != "hello@news.example" || news["name"] != "Weekly Digest" || news["messages"] != float64(5) || news["has_list_unsubscribe"] != true ||
		news["verdict"] != "route" || id(news["rule_id"]) != 2 || news["source"] != "learned" || news["hits"] != float64(2) || news["last_seen_at"] == nil {
		t.Errorf("learned sender = %v", news)
	}
	if swiggy["value"] != "noreply@swiggy.in" || swiggy["name"] != "Swiggy" || swiggy["messages"] != float64(3) || swiggy["verdict"] != nil ||
		swiggy["rule_id"] != nil || swiggy["source"] != nil || swiggy["hits"] != float64(0) || swiggy["has_list_unsubscribe"] != false {
		t.Errorf("sender without a rule = %v", swiggy)
	}

	// The user sets a sender: kept in the inbox from now on, and counted.
	set := e.call(http.MethodPut, "/api/senders/address/noreply%40swiggy.in", `{"verdict":"keep"}`, http.StatusOK)["sender"].(map[string]any)
	conform(t, e.doc, "Sender", set)
	if set["verdict"] != "keep" || set["source"] != "user" || set["rule_id"] != nil || set["messages"] != float64(3) || set["name"] != "Swiggy" {
		t.Fatalf("after PUT = %v", set)
	}
	e.deliver("Swiggy <noreply@swiggy.in>", "order kept")
	if it := e.item("order kept", "acted"); it["decision"].(map[string]any)["stage"] != "sender" || e.folderOf("order kept") != "INBOX" {
		t.Errorf("a kept sender's mail: %v in %q", it["decision"], e.folderOf("order kept"))
	}
	// A domain can be routed too, and a quiet address blocked before it ever writes.
	domain := e.call(http.MethodPut, "/api/senders/domain/News.Example", `{"verdict":"route","rule_id":1}`, http.StatusOK)["sender"].(map[string]any)
	if domain["type"] != "domain" || domain["value"] != "news.example" || domain["messages"] != float64(5) || id(domain["rule_id"]) != 1 || domain["has_list_unsubscribe"] != true || domain["name"] != "" {
		t.Errorf("domain sender = %v", domain)
	}
	e.call(http.MethodPut, "/api/senders/address/spam%40junk.example", `{"verdict":"block","rule_id":1}`, http.StatusOK)
	e.deliver("spam@junk.example", "you won")
	if it := e.item("you won", "acted"); it["decision"].(map[string]any)["stage"] != "sender" || e.folderOf("you won") != "Trash" {
		t.Errorf("a blocked sender's mail: %v in %q", it["decision"], e.folderOf("you won"))
	}

	for query, want := range map[string][]string{
		"":                      {"hello@news.example", "news.example", "noreply@swiggy.in", "spam@junk.example"}, // by volume, then by name
		"?sort=recent":          nil,
		"?source=learned":       {"hello@news.example"},
		"?source=user":          {"news.example", "noreply@swiggy.in", "spam@junk.example"},
		"?q=WEEKLY":             {"hello@news.example"},
		"?q=news":               {"hello@news.example", "news.example"},
		"?limit=1&cursor=1":     {"news.example"},
		"?limit=2&cursor=9":     {},
		"?source=user&q=swiggy": {"noreply@swiggy.in"},
	} {
		got := e.call(http.MethodGet, "/api/senders"+query, "", http.StatusOK)["items"].([]any)
		if want == nil {
			if len(got) != 4 {
				t.Errorf("senders%s has %d rows", query, len(got))
			}
			continue
		}
		var values []string
		for _, it := range got {
			values = append(values, it.(map[string]any)["value"].(string))
		}
		if fmt.Sprint(values) != fmt.Sprint(want) {
			t.Errorf("senders%s = %v, want %v", query, values, want)
		}
	}
	if page := e.call(http.MethodGet, "/api/senders?limit=3", "", http.StatusOK); page["next_cursor"] != "3" {
		t.Errorf("next_cursor = %v", page["next_cursor"])
	}
	if kept := e.call(http.MethodGet, "/api/senders?q=swiggy", "", http.StatusOK)["items"].([]any)[0].(map[string]any); kept["hits"] != float64(1) || kept["messages"] != float64(4) {
		t.Errorf("the kept sender after one more email = %v", kept)
	}

	// "read %" and the unread sort have no data behind them, so they are not offered.
	for query, path := range map[string]string{"sort=unread": "sort", "source=robot": "source", "limit=0": "limit", "cursor=-1": "cursor"} {
		e.refuse(http.MethodGet, "/api/senders?"+query, "", http.StatusBadRequest, "invalid_input", path)
	}
	for target, want := range map[string][2]string{
		"/api/senders/name/x@y.example|{\"verdict\":\"keep\"}":                    {"invalid_input", "type"},
		"/api/senders/address/nope|{\"verdict\":\"keep\"}":                        {"invalid_input", "value"},
		"/api/senders/domain/x@y.example|{\"verdict\":\"keep\"}":                  {"invalid_input", "value"},
		"/api/senders/domain/localhost|{\"verdict\":\"keep\"}":                    {"invalid_input", "value"},
		"/api/senders/address/x@y.example|{\"verdict\":\"trash\"}":                {"invalid_input", "verdict"},
		"/api/senders/address/x@y.example|{\"verdict\":\"route\"}":                {"invalid_input", "rule_id"},
		"/api/senders/address/x@y.example|{\"verdict\":\"route\",\"rule_id\":99}": {"invalid_input", "rule_id"},
		"/api/senders/address/x@y.example|{\"verdict\":\"keep\",\"hits\":3}":      {"invalid_json", ""},
	} {
		path, body, _ := strings.Cut(target, "|")
		e.refuse(http.MethodPut, path, body, http.StatusBadRequest, want[0], want[1])
	}
	e.refuse(http.MethodDelete, "/api/senders/name/x@y.example", "", http.StatusBadRequest, "invalid_input", "type")

	// Forgetting a rule, learned or user-made, hands the sender back to the rules.
	e.call(http.MethodDelete, "/api/senders/domain/news.example", "", http.StatusNoContent)
	e.call(http.MethodDelete, "/api/senders/address/hello%40news.example", "", http.StatusNoContent)
	e.call(http.MethodDelete, "/api/senders/address/hello%40news.example", "", http.StatusNoContent) // again: there was none
	if got := e.call(http.MethodGet, "/api/senders?source=learned", "", http.StatusOK)["items"].([]any); len(got) != 0 {
		t.Errorf("learned rules after forgetting = %v", got)
	}
	if n := e.count(`SELECT COUNT(*) FROM sender_rules`); n != 2 {
		t.Errorf("%d sender rules left, want the two the user set", n)
	}
}

func near(got any, want float64) bool {
	f, ok := got.(float64)
	return ok && math.Abs(f-want) < 1e-9
}

// The stats are sums over the decisions and the cost ledger; seed both with known rows
// and check every figure.
func TestStatsMath(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.call(http.MethodPost, "/api/rules/import", rulesYAML, http.StatusOK) // 1 Food, 2 Reading
	ctx := t.Context()
	now := e.ck.t // 2027-01-15 08:00 UTC
	day := func(ago int) time.Time { return now.AddDate(0, 0, -ago) }
	uid := uint32(0)
	// decided seeds one email and a decision for it.
	decided := func(at time.Time, state, stage string, ruleID int64, model string, cost float64) store.Message {
		t.Helper()
		uid++
		m, _, err := e.st.IngestMessage(ctx, mail.MsgRef{AccountID: 1, Folder: "Old", UIDValidity: 7, UID: uid}, at.Unix())
		if err != nil {
			t.Fatal(err)
		}
		name := map[int64]string{1: "Food", 2: "Reading"}[ruleID]
		if _, err := e.st.AddDecision(ctx, store.Decision{MessageID: m.ID, Stage: stage, RuleID: ruleID, RuleName: name, Confidence: 0.9,
			Model: model, TokensIn: 100, TokensOut: 10, CostUSD: cost, CreatedAt: at.Unix()}, state); err != nil {
			t.Fatal(err)
		}
		if state == store.StateActed { // a sorted email has an action in effect
			if _, err := e.st.InsertAction(ctx, store.Action{MessageID: m.ID, AccountID: 1, Kind: "read", Status: store.ActionDone, CreatedAt: at.Unix()}); err != nil {
				t.Fatal(err)
			}
		}
		return m
	}
	usage := func(at time.Time, provider, model, purpose string, calls int, cost float64) {
		t.Helper()
		if err := e.st.AddUsage(ctx, at.UTC().Format(time.DateOnly), provider, model, purpose, calls, calls*100, calls*10, cost); err != nil {
			t.Fatal(err)
		}
	}
	// Today: one by conditions, one by the model (and trashed), one the model matched to no
	// rule, one waiting in review.
	decided(now, store.StateActed, "condition", 1, "", 0)
	trashed := decided(now, store.StateActed, "decider", 2, "jev-1", 0.002)
	if _, err := e.st.InsertAction(ctx, store.Action{MessageID: trashed.ID, AccountID: 1, Kind: "trash", Status: store.ActionDone, CreatedAt: now.Unix()}); err != nil {
		t.Fatal(err)
	}
	decided(now, store.StateSkipped, "none", 0, "jev-1", 0.001)
	decided(now, store.StateReview, "decider", 2, "jev-1", 0.001)
	usage(now, "openrouter", "jev-1", "decide", 3, 0.004)
	usage(now, "anthropic", "claude-haiku-4-5", "compose", 1, 0.003)
	// Three days ago: one by conditions.
	decided(day(3), store.StateActed, "condition", 1, "", 0)
	// Twenty days ago: one email decided twice (a retry): first Food by the model, then
	// Reading by the fallback. It counts once, as Reading; both calls were paid for.
	twice := decided(day(20), store.StateDecided, "decider", 1, "jev-1", 0.004)
	if _, err := e.st.AddDecision(ctx, store.Decision{MessageID: twice.ID, Stage: "fallback", RuleID: 2, RuleName: "Reading", Confidence: 0.9,
		Model: "claude-haiku-4-5", CostUSD: 0.01, CreatedAt: day(20).Unix()}, store.StateActed); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.InsertAction(ctx, store.Action{MessageID: twice.ID, AccountID: 1, Kind: "move", Status: store.ActionDone, CreatedAt: day(20).Unix()}); err != nil {
		t.Fatal(err)
	}
	usage(day(20), "openrouter", "jev-1", "decide", 2, 0.005)
	usage(day(20), "anthropic", "claude-haiku-4-5", "escalate", 1, 0.01)
	// Forty days ago: outside every range.
	decided(day(40), store.StateActed, "condition", 1, "", 0)
	usage(day(40), "openrouter", "jev-1", "decide", 5, 0.01)

	today := time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC).Unix()
	for _, tc := range []struct {
		query                              string
		since                              int64
		processed, sorted, trashed, review float64
		free, cost                         float64
		models                             int
		top                                string
	}{
		{"", today, 4, 2, 1, 1, 0.25, 0.007, 2, "Food:1 Reading:1"},
		{"?range=day", today, 4, 2, 1, 1, 0.25, 0.007, 2, "Food:1 Reading:1"},
		{"?range=week", today - 6*86400, 5, 3, 1, 1, 0.4, 0.007, 2, "Food:2 Reading:1"},
		{"?range=month", today - 29*86400, 6, 4, 1, 1, 2.0 / 6, 0.022, 3, "Food:2 Reading:2"},
	} {
		got := e.call(http.MethodGet, "/api/stats/summary"+tc.query, "", http.StatusOK)
		conform(t, e.doc, "StatsSummary", got)
		counts := got["counts"].(map[string]any)
		var top string
		for i, r := range got["top_rules"].([]any) {
			r := r.(map[string]any)
			top += fmt.Sprintf("%s%v:%v", map[bool]string{true: " "}[i > 0], r["rule_name"], r["hits"])
		}
		if got["since"] != float64(tc.since) || counts["processed"] != tc.processed || counts["sorted"] != tc.sorted || counts["trashed"] != tc.trashed || counts["review"] != tc.review ||
			!near(got["decided_without_model"], tc.free) || !near(got["cost_usd"], tc.cost) || len(got["calls_by_model"].([]any)) != tc.models || top != tc.top {
			t.Errorf("summary%s = since %v counts %v free %v cost %v, %d models, top %q; want %+v", tc.query, got["since"], counts,
				got["decided_without_model"], got["cost_usd"], len(got["calls_by_model"].([]any)), top, tc)
		}
		went := got["went"].(map[string]any)
		if sum := went["sorted"].(float64) + went["inbox"].(float64) + went["review"].(float64) + went["trashed"].(float64); sum != counts["processed"] {
			t.Errorf("summary%s: went %v adds up to %v, want processed %v", tc.query, went, sum, counts["processed"])
		}
		if went["sorted"].(float64)+went["trashed"].(float64) != counts["sorted"] {
			t.Errorf("summary%s: went %v does not agree with counts %v", tc.query, went, counts)
		}
		if _, ok := got["quiet_rules"].(float64); !ok {
			t.Errorf("summary%s: quiet_rules missing", tc.query)
		}
		if acct := got["accounts"].([]any)[0].(map[string]any); acct["status"] != "live" || id(acct["account_id"]) != 1 || acct["label"] != "me@example.test" || acct["preset"] == "" || acct["username"] == "" || acct["folder_count"].(float64) < 1 {
			t.Errorf("account health = %v", acct)
		}
	}
	e.refuse(http.MethodGet, "/api/stats/summary?range=year", "", http.StatusBadRequest, "invalid_input", "range")
	e.refuse(http.MethodGet, "/api/stats/usage?range=week", "", http.StatusBadRequest, "invalid_input", "range")

	u := e.call(http.MethodGet, "/api/stats/usage", "", http.StatusOK)
	conform(t, e.doc, "StatsUsage", u)
	days := u["days"].([]any)
	if u["range"] != "month" || u["processed"] != float64(6) || u["emails"] != float64(4) || u["calls"] != float64(7) || !near(u["cost_usd"], 0.022) || u["without_model"] != float64(2) || len(days) != 30 {
		t.Fatalf("usage = emails %v calls %v cost %v without %v, %d days", u["emails"], u["calls"], u["cost_usd"], u["without_model"], len(days))
	}
	first, quiet, last := days[0].(map[string]any), days[28].(map[string]any), days[29].(map[string]any)
	if first["day"] != "2026-12-17" || len(first["models"].([]any)) != 0 || len(quiet["models"].([]any)) != 0 || last["day"] != "2027-01-15" || len(last["models"].([]any)) != 2 ||
		len(days[9].(map[string]any)["models"].([]any)) != 2 {
		t.Errorf("days = first %v, yesterday %v, today %v", first, quiet, last)
	}
	if m := last["models"].([]any)[1].(map[string]any); m["provider"] != "openrouter" || m["calls"] != float64(3) || !near(m["cost_usd"], 0.004) {
		t.Errorf("today's decision model = %v", m)
	}
	var byRule, byModel string
	for _, r := range u["by_rule"].([]any) {
		r := r.(map[string]any)
		byRule += fmt.Sprintf("%v/%v emails %v calls %v $%.3f; ", r["rule_name"], r["rule_id"], r["emails"], r["calls"], r["cost_usd"])
	}
	for _, m := range u["by_model"].([]any) {
		m := m.(map[string]any)
		byModel += fmt.Sprintf("%v %v %v $%.3f; ", m["model"], m["purpose"], m["calls"], m["cost_usd"])
	}
	if want := "Reading/2 emails 3 calls 3 $0.013; Food/1 emails 3 calls 1 $0.004; /<nil> emails 1 calls 1 $0.001; "; byRule != want {
		t.Errorf("by_rule = %q, want %q", byRule, want)
	}
	if want := "claude-haiku-4-5 escalate 1 $0.010; jev-1 decide 5 $0.009; claude-haiku-4-5 compose 1 $0.003; "; byModel != want {
		t.Errorf("by_model = %q, want %q", byModel, want)
	}
}

// A correction that cannot be applied for a reason that is not the mail server's doing says
// what the reason is, instead of a bare 500.
func TestCorrectionToARuleTheAccountCannotCarryOut(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.call(http.MethodPost, "/api/rules/batch", `{"rules":[{"name":"Archive it","intent":"Things to archive","actions":[{"type":"archive"}]}]}`, http.StatusCreated)
	e.deliver("friend@example.org", "lunch")
	msg := id(e.item("lunch", "skipped")["id"])
	// This account has no folder marked for archiving, and MailRules never guesses one.
	if _, err := e.db.ExecContext(t.Context(), `UPDATE folders SET special_use = NULL WHERE name = 'Archive'`); err != nil {
		t.Fatal(err)
	}
	e.refuse(http.MethodPost, fmt.Sprintf("/api/messages/%d/correct", msg), `{"rule_id":1}`, http.StatusUnprocessableEntity, "no_special_folder", "")
	if r := e.do(http.MethodPost, fmt.Sprintf("/api/messages/%d/correct", msg), `{"rule_id":1}`); r.body.Error.Message == "" || e.folderOf("lunch") != "INBOX" {
		t.Errorf("message %q; the email is in %q", r.body.Error.Message, e.folderOf("lunch"))
	}
	if n := e.count(`SELECT COUNT(*) FROM corrections`); n != 0 {
		t.Errorf("%d corrections recorded for a correction that failed", n)
	}
}

// "Why this happened" shows what the decision model thought of every rule it chose between.
func TestTraceShowsTheCandidatesProbabilities(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.call(http.MethodPost, "/api/rules/import", rulesYAML, http.StatusOK)
	e.decider.Spread = map[int64]float64{0: 0.15, 2: 0.85}
	e.decider.DecideFunc = func(models.DecideRequest) (models.Decision, models.Usage, error) {
		return models.Decision{RuleID: 2, Confidence: 0.85, Reason: "A newsletter"}, models.Usage{Provider: "openrouter", Model: "jev-1"}, nil
	}
	e.deliver("hello@news.example", "issue one")
	msg := id(e.item("issue one", "acted")["id"])
	detail := e.call(http.MethodGet, fmt.Sprintf("/api/messages/%d", msg), "", http.StatusOK)["message"].(map[string]any)
	conform(t, e.doc, "MessageDetail", detail)
	trace := detail["trace"].([]any)
	cands := trace[0].(map[string]any)["candidates"].([]any)
	if len(cands) != 2 || id(cands[0].(map[string]any)["rule_id"]) != 2 || cands[0].(map[string]any)["rule_name"] != "Reading" || cands[0].(map[string]any)["probability"] != 0.85 ||
		cands[1].(map[string]any)["rule_id"] != nil || cands[1].(map[string]any)["probability"] != 0.15 {
		t.Errorf("candidates = %v", cands)
	}
	if action := trace[1].(map[string]any); len(action["candidates"].([]any)) != 0 {
		t.Errorf("an action step has candidates: %v", action)
	}
}

// MAI-19: 15 emails processed, 10 of them sorted by a rule, 5 matched by no rule. The free
// share is counted over the processed emails and only the rule-settled ones count (MAI-31),
// so it is 10 of 15 and can never pass it.
func TestUsageFreeShareIsOverProcessed(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.call(http.MethodPost, "/api/rules/batch", `{"rules":[{"name":"Food","conditions":{"field":"from_domain","op":"eq","value":"swiggy.in"},"actions":[{"type":"move","folder":"Food"}]}]}`, http.StatusCreated)
	for i := range 15 {
		from, state := "noreply@swiggy.in", "acted"
		if i >= 10 {
			from, state = "friend@example.org", "skipped"
		}
		e.deliver(from, fmt.Sprintf("mail %d", i))
		e.item(fmt.Sprintf("mail %d", i), state)
	}
	u := e.call(http.MethodGet, "/api/stats/usage", "", http.StatusOK)
	conform(t, e.doc, "StatsUsage", u)
	if u["processed"] != float64(15) || u["emails"] != float64(10) || u["without_model"] != float64(10) {
		t.Fatalf("usage = processed %v, emails %v, without_model %v; want 15, 10, 10", u["processed"], u["emails"], u["without_model"])
	}
	if u["without_model"].(float64) > u["processed"].(float64) {
		t.Errorf("without_model %v is more than processed %v", u["without_model"], u["processed"])
	}
}

// MAI-31: no model key, one rule that needs a model, one email nothing can take. It waits
// in Needs review and nobody decided it, so both free figures read 0; a condition rule
// that does act makes it 1 of 2 processed.
func TestNothingDecidedIsNotDecidedWithoutAModel(t *testing.T) {
	e := newEnv(t)
	e.noDecider = true
	e.connect()
	e.call(http.MethodPost, "/api/rules/batch", `{"rules":[
		{"name":"Newsletters","intent":"Newsletters and promotions","actions":[{"type":"move","folder":"Reading"}]},
		{"name":"Food","conditions":{"field":"from_domain","op":"eq","value":"swiggy.in"},"actions":[{"type":"move","folder":"Food"}]}]}`, http.StatusCreated)
	e.deliver("friend@example.org", "lunch")
	e.item("lunch", "review")
	free := func() (float64, float64, float64) {
		s := e.call(http.MethodGet, "/api/stats/summary?range=month", "", http.StatusOK)
		u := e.call(http.MethodGet, "/api/stats/usage", "", http.StatusOK)
		return s["decided_without_model"].(float64), u["without_model"].(float64), u["processed"].(float64)
	}
	if share, n, processed := free(); share != 0 || n != 0 || processed != 1 {
		t.Errorf("one email in review: share %v, without_model %v of %v; want 0, 0 of 1", share, n, processed)
	}
	e.deliver("noreply@swiggy.in", "order one")
	e.item("order one", "acted")
	if share, n, processed := free(); share != 0.5 || n != 1 || processed != 2 {
		t.Errorf("one by a condition rule: share %v, without_model %v of %v; want 0.5, 1 of 2", share, n, processed)
	}
}

// MAI-23: with no decision model, a rule with an intent is passed over and the condition
// rule below it applies. Only mail that nothing but the intent rule could take waits in
// Needs review. Live processing and the cleanup preview agree.
func TestConditionRuleBelowAnIntentRuleWithoutAModel(t *testing.T) {
	e := newEnv(t)
	e.noDecider = true
	e.connect()
	e.call(http.MethodPost, "/api/rules/batch", `{"rules":[
		{"name":"Newsletters","intent":"Newsletters and promotions","actions":[{"type":"move","folder":"Reading"}]},
		{"name":"Food","conditions":{"field":"from_domain","op":"eq","value":"swiggy.in"},"actions":[{"type":"move","folder":"Food"}]}]}`, http.StatusCreated)

	e.deliver("noreply@swiggy.in", "order one")
	if d := e.item("order one", "acted")["decision"].(map[string]any); d["stage"] != "condition" || d["rule_name"] != "Food" || e.folderOf("order one") != "Food" {
		t.Errorf("the condition rule below the AI rule did not fire: %v, in %q", d, e.folderOf("order one"))
	}
	e.deliver("friend@example.org", "lunch")
	if d := e.item("lunch", "review")["decision"].(map[string]any); d["reason"] != "No decision model is set" || e.folderOf("lunch") != "INBOX" {
		t.Errorf("mail only the AI rule could take = %v, in %q", d, e.folderOf("lunch"))
	}

	// A cleanup check says the same of mail that is already there: with no model, the
	// condition rule takes the swiggy order and the friend's mail waits in review.
	e.mb.AddFolder("Old", "")
	e.mb.Deliver("Old", "From: noreply@swiggy.in\r\nSubject: old order\r\n\r\nbody\r\n")
	e.mb.Deliver("Old", "From: friend@example.org\r\nSubject: old lunch\r\n\r\nbody\r\n")
	got := map[string]bool{}
	for _, r := range rowsOf(e.checkReady(`{"account_id":1,"folder":"Old"}`)) {
		got[fmt.Sprint(r["stage"], ":", r["selectable"])] = true
	}
	if !got["condition:true"] || !got["none:false"] {
		t.Errorf("check rows = %v, want a Food condition row and a review row", got)
	}
	// With a model set, the same two emails are the model's to decide: the intent rule is
	// above the condition rule for both, so both reach the model.
	e.noDecider = false
	e.decider.DecideFunc = bySubject("")
	if chk := e.checkReady(`{"account_id":1,"folder":"Old"}`); id(chk["model_calls"]) != 2 {
		t.Errorf("check with a model made %v model calls, want 2", chk["model_calls"])
	}
}

// MAI-23: after everything done today was undone, the Overview must not still say "Sorted":
// an email whose actions were all undone counts as left in the inbox.
func TestUndoneEmailsAreNotCountedAsSorted(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.call(http.MethodPost, "/api/rules/batch", `{"rules":[
		{"name":"Food","conditions":{"field":"from_domain","op":"eq","value":"swiggy.in"},"actions":[{"type":"move","folder":"Food"},{"type":"read"}]},
		{"name":"Junk","conditions":{"field":"from_domain","op":"eq","value":"junk.example"},"actions":[{"type":"trash"}]}]}`, http.StatusCreated)
	for i := range 3 {
		e.deliver("noreply@swiggy.in", fmt.Sprintf("order %d", i))
		e.item(fmt.Sprintf("order %d", i), "acted")
	}
	e.deliver("spam@junk.example", "win big")
	e.item("win big", "acted")
	summary := func() (counts, went map[string]any) {
		s := e.call(http.MethodGet, "/api/stats/summary", "", http.StatusOK)
		conform(t, e.doc, "StatsSummary", s)
		return s["counts"].(map[string]any), s["went"].(map[string]any)
	}
	if c, w := summary(); c["sorted"] != float64(4) || c["trashed"] != float64(1) || w["sorted"] != float64(3) || w["trashed"] != float64(1) || w["inbox"] != float64(0) {
		t.Fatalf("before the undo: counts %v, went %v", c, w)
	}
	e.call(http.MethodPost, fmt.Sprintf("/api/actions/undo?since=%d", e.ck.now().Unix()-3600), "", http.StatusOK)
	if c, w := summary(); c["processed"] != float64(4) || c["sorted"] != float64(0) || c["trashed"] != float64(0) ||
		w["sorted"] != float64(0) || w["trashed"] != float64(0) || w["inbox"] != float64(4) || w["review"] != float64(0) {
		t.Errorf("after everything was undone: counts %v, went %v; want nothing sorted and 4 in the inbox", c, w)
	}
}
