package composer

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/message"
	"github.com/TurtleByte-IN/mailrules/internal/models"
	"github.com/TurtleByte-IN/mailrules/internal/pipeline"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// stored records an email MailRules saw, with a decision for rule (0 = none) in state.
func (e *env) stored(t *testing.T, uid uint32, from, subject, listID string, rule int64, state string) store.Message {
	t.Helper()
	ctx := t.Context()
	m, _, err := e.st.IngestMessage(ctx, mail.MsgRef{AccountID: e.acct.ID, Folder: "INBOX", UIDValidity: 9, UID: uid}, 1)
	if err != nil {
		t.Fatal(err)
	}
	m.FromAddr, m.FromDomain, m.Subject, m.ListID, m.Snippet = from, from[strings.Index(from, "@")+1:], subject, listID, "secret body text"
	if err := e.st.SaveMessageSummary(ctx, m); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.AddDecision(ctx, store.Decision{MessageID: m.ID, Stage: "decider", RuleID: rule, Confidence: 0.6,
		Reason: "Looks like a food order", CreatedAt: 2}, state); err != nil {
		t.Fatal(err)
	}
	return m
}

// corrected records a correction of m from wrong to right, its example as corrections store it.
func (e *env) corrected(t *testing.T, m store.Message, wrong, right int64, kind string) {
	t.Helper()
	ex, _ := json.Marshal(message.Summary{From: m.FromAddr, FromDomain: m.FromDomain, Subject: m.Subject, ListID: m.ListID, Body: "secret body text"})
	if _, err := e.st.AddCorrection(t.Context(), 1, e.user.ID, store.Correction{MessageID: m.ID, WrongRuleID: wrong, RightRuleID: right,
		Kind: kind, Example: string(ex), CreatedAt: 3}, ""); err != nil {
		t.Fatal(err)
	}
}

// Improve rule: the composer is given the corrections naming the rule, what waits in Needs
// review for it and a fresh test of it, never a body; the draft is tested alone on the same
// emails and the ones it starts or stops taking come back with what changed.
func TestImproveFromEvidence(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	food, err := e.st.CreateRule(ctx, 1, rules.Rule{UserID: e.user.ID, Name: "Food", Said: "Swiggy goes to Food",
		Conditions: cond(t, `{"field":"from_domain","op":"eq","value":"swiggy.in"}`), Actions: []rules.Action{{Type: rules.ActMove, Folder: "Food"}}, Enabled: true}, 1)
	if err != nil {
		t.Fatal(err)
	}
	bills, err := e.st.CreateRule(ctx, 1, rules.Rule{UserID: e.user.ID, Name: "Bills", Intent: "Bills", Actions: []rules.Action{{Type: rules.ActMove, Folder: "Archive"}}, Enabled: true}, 1)
	if err != nil {
		t.Fatal(err)
	}
	e.corrected(t, e.stored(t, 1, "deals@swiggy.in", "50% off sale", "deals.swiggy.in", food.ID, store.StateActed), food.ID, 0, store.CorrectionFix)
	e.corrected(t, e.stored(t, 2, "orders@zomato.com", "your order", "", 0, store.StateActed), 0, food.ID, store.CorrectionReview)
	e.corrected(t, e.stored(t, 3, "bank@acmebank.example", "statement", "", 0, store.StateActed), 0, bills.ID, store.CorrectionFix) // names another rule
	e.stored(t, 4, "hi@swiggy.in", "dinner?", "", food.ID, store.StateReview)
	e.stored(t, 5, "x@bills.example", "due", "", bills.ID, store.StateReview) // waits for another rule

	e.deliver("noreply@swiggy.in", "order 1")
	e.deliver("deals@swiggy.in", "Big sale today")
	e.deliver("orders@zomato.com", "order 2")
	e.deliver("friend@example.org", "lunch")

	var user string
	answer := `{"rules":[{"name":"Food","parts":[],"intent":null,"conditions":{"field":"from_domain","op":"in","value":["swiggy.in","zomato.com"]},
		"exceptions":{"field":"subject","op":"contains","value":"sale"},"actions":[{"type":"move","folder":"Food"}],"min_confidence":null,"new_folders":[],"question":null,"conflicts":[]}],
		"unparsed":[],"changes":["Added zomato.com: you said its order belongs in Food.", " ", "Skips sales: you moved a Swiggy sale out of Food."]}`
	c := Composer{Store: e.st, Gen: &models.Fake{GenerateFunc: func(_, usr string, _ json.RawMessage, out any) (models.Usage, error) {
		user = usr
		return models.Usage{Provider: "anthropic", Model: "m", TokensIn: 10}, json.Unmarshal([]byte(answer), out)
	}}, BodyChars: 2000}
	req := Request{Viewer: e.user.Viewer(), Rule: &food, Account: &e.acct, Mailbox: e.mb,
		Decider: pipeline.Decider{Router: decider(e.st), MinConfidence: 0.75, Now: time.Unix(1_800_000_000, 0)}}
	out, err := c.Compose(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`{"from":"deals@swiggy.in","subject":"50% off sale","list_id":"deals.swiggy.in","this_rule":"wrong","other":"inbox","where":"feed"}`,
		`{"from":"orders@zomato.com","subject":"your order","this_rule":"right","other":"inbox","where":"needs_review"}`,
		`{"from":"hi@swiggy.in","subject":"dinner?","confidence":0.6,"reason":"Looks like a food order"}`,
		"The rule alone, tested on the newest 4 emails:",
		`{"from":"deals@swiggy.in","subject":"Big sale today","confidence":1,"reason":"Matched \"Food\" by its conditions"}`,
		"asked to improve this rule from how it behaved, and said nothing more",
	} {
		if !strings.Contains(user, want) {
			t.Errorf("the model was not shown %s:\n%s", want, user)
		}
	}
	for _, never := range []string{"secret body text", "The body of", "acmebank", "bills.example"} {
		if strings.Contains(user, never) {
			t.Errorf("the model was shown %q:\n%s", never, user)
		}
	}
	d := out.Drafts[0]
	if fmt.Sprint(out.Changes) != "[Added zomato.com: you said its order belongs in Food. Skips sales: you moved a Swiggy sale out of Food.]" ||
		d.Said != "Swiggy goes to Food" || d.Tested != 4 || d.MatchCount != 2 || len(d.Samples) != 2 {
		t.Errorf("changes %q, draft %+v", out.Changes, d)
	}
	var got []string
	for _, s := range out.Shifts {
		got = append(got, s.Change+" "+s.After.Subject+" "+s.Before.RuleName+">"+s.After.RuleName)
	}
	if fmt.Sprint(got) != "[starts order 2 >Food stops Big sale today Food>]" {
		t.Errorf("shifts = %q", got)
	}

	// Typed words come with the same evidence; without a mailbox there is no test to compare.
	req.Text, req.Mailbox = "also Zomato", nil
	if out, err = c.Compose(ctx, req); err != nil || len(out.Shifts) != 0 || out.Shifts == nil || out.Drafts[0].Tested != 0 ||
		!strings.Contains(user, "[1] also Zomato") || !strings.Contains(user, "could not be tested") || !strings.Contains(user, "50% off sale") {
		t.Errorf("typed, offline: %v %+v\n%s", err, out, user)
	}
}

// When the rule cannot be tested, its evidence says so and the draft is tested as a new
// draft would be; a draft that cannot be tested gets no comparison.
func TestImproveTestGating(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.deliver("noreply@swiggy.in", "order 1")
	byCond := `{"field":"from_domain","op":"eq","value":"swiggy.in"}`
	draft := func(intent, conds string) string {
		return `{"rules":[{"name":"Food","intent":` + intent + `,"conditions":` + conds + `,"actions":[{"type":"archive"}]}],"unparsed":[],"changes":[]}`
	}
	for name, tc := range map[string]struct {
		intent     string // the saved rule's
		router     bool
		answer     string
		ruleTested bool
		tested     int // the draft's
	}{
		"conditions, model":             {"", true, draft("null", byCond), true, 1},
		"conditions, no model":          {"", false, draft("null", byCond), true, 1},
		"intent, no model; draft too":   {"Food orders", false, draft(`"Food orders"`, "{}"), false, 0},
		"intent, no model; draft conds": {"Food orders", false, draft("null", byCond), false, 1},
		"draft with errors":             {"", true, draft("null", "{}"), true, 0},
	} {
		t.Run(name, func(t *testing.T) {
			rule := rules.Rule{ID: 7, Name: "Food", Intent: tc.intent, Conditions: cond(t, byCond), Actions: []rules.Action{{Type: rules.ActMove, Folder: "Food"}}}
			var user string
			c := Composer{Store: e.st, Gen: &models.Fake{GenerateFunc: func(_, usr string, _ json.RawMessage, out any) (models.Usage, error) {
				user = usr
				return models.Usage{}, json.Unmarshal([]byte(tc.answer), out)
			}}}
			req := Request{Viewer: e.user.Viewer(), Rule: &rule, Account: &e.acct, Mailbox: e.mb}
			if tc.router {
				req.Decider.Router = decider(e.st)
			}
			out, err := c.Compose(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(user, "could not be tested") == tc.ruleTested || out.Drafts[0].Tested != tc.tested || out.Shifts == nil || out.Changes == nil {
				t.Errorf("rule tested %v, draft tested %d, shifts %v, changes %v\n%s", !strings.Contains(user, "could not be tested"), out.Drafts[0].Tested, out.Shifts, out.Changes, user)
			}
		})
	}
}
