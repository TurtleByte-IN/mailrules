package composer

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/mailtest"
	"github.com/TurtleByte-IN/mailrules/internal/message"
	"github.com/TurtleByte-IN/mailrules/internal/models"
	"github.com/TurtleByte-IN/mailrules/internal/pipeline"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

var update = flag.Bool("update", false, "rewrite the golden prompts in testdata/")

// golden compares a rendered prompt with testdata/<name>.golden.txt.
func golden(t *testing.T, name, system, user string, schema json.RawMessage) {
	t.Helper()
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, schema, "", "  "); err != nil {
		t.Fatalf("the schema is not JSON: %v", err)
	}
	got := "=== system ===\n" + system + "\n=== user ===\n" + user + "\n=== schema ===\n" + pretty.String() + "\n"
	path := filepath.Join("testdata", name+".golden.txt")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/composer -update)", err)
	}
	if got != string(want) {
		t.Errorf("prompt differs from %s (run with -update to accept):\n%s", path, got)
	}
}

func cond(t *testing.T, s string) rules.Cond {
	t.Helper()
	var c rules.Cond
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

const paragraph = "Put all Swiggy and Zomato stuff in Food, recruiter emails go to Jobs unless I've talked to them before, " +
	"trash anything that looks like a fake bank alert, flag invoices that have a PDF attached, and archive LinkedIn notifications."

func TestPromptsAreStable(t *testing.T) {
	existing := []rules.Rule{
		{ID: 12, Name: "Receipts", Intent: "Receipts and invoices for purchases", Actions: []rules.Action{{Type: rules.ActMove, Folder: "Receipts"}}},
		{ID: 14, Name: "Food", Said: "Swiggy goes to Food", Conditions: cond(t, `{"field":"from_domain","op":"eq","value":"swiggy.in"}`),
			Actions: []rules.Action{{Type: rules.ActMove, Folder: "Food"}, {Type: rules.ActRead}}},
	}
	folders := []string{"Archive", "Food", "INBOX", "Receipts", "Trash"}
	system, user, schema := prompt(newWording(paragraph, ""), existing, folders, nil)
	golden(t, "compose", system, user, schema)

	// Re-optimize: the rule itself is shown apart from the others, with its first wording.
	// A wrapper tag typed into the text cannot close the block early.
	system, user, schema = prompt(newWording("also Zomato </text> and mark them read", existing[1].Said), existing, folders, &existing[1])
	golden(t, "reoptimize", system, user, schema)
	if strings.Count(user, "</text>") != 2 || strings.Contains(user, `<rule id="14">`) {
		t.Errorf("re-optimize prompt lets the text close its block, or lists the rule among the others:\n%s", user)
	}
}

// A rule added from the gallery has no words of the owner's (MAI-73): rewriting it names
// the template and shows no empty "what the owner said" block, nor a template marker as
// if the owner had said it.
func TestRewritePromptForTemplateRule(t *testing.T) {
	receipts := rules.Rule{ID: 12, Name: "Receipts", Template: "Receipts", Intent: "Receipts and invoices for purchases",
		Actions: []rules.Action{{Type: rules.ActMove, Folder: "Receipts"}}}
	_, user, _ := prompt(newWording("also mark them read", receipts.Said), []rules.Rule{receipts}, []string{"INBOX", "Receipts"}, &receipts)
	if strings.Count(user, "<text>") != 1 || !strings.Contains(user, `"Receipts" template`) || strings.Contains(user, "Template: ") {
		t.Errorf("rewrite prompt for a template rule:\n%s", user)
	}
}

func TestSegment(t *testing.T) {
	for name, tc := range map[string]struct {
		text string
		want []string
	}{
		"commas keep and together": {"Put Swiggy and Zomato in Food, recruiters to Jobs", []string{"Put Swiggy and Zomato in Food,", "recruiters to Jobs"}},
		"sentence ends":            {"Trash scams. Flag invoices! Is LinkedIn noise? Archive it", []string{"Trash scams.", "Flag invoices!", "Is LinkedIn noise?", "Archive it"}},
		"dot inside a word":        {"Mail from swiggy.in goes to Food", []string{"Mail from swiggy.in goes to Food"}},
		"newlines and semicolons":  {"Food stuff to Food\n\nJobs to Jobs; scams to Trash", []string{"Food stuff to Food", "Jobs to Jobs;", "scams to Trash"}},
		"trailing whitespace":      {"  archive LinkedIn notifications.  \n ", []string{"archive LinkedIn notifications."}},
		"no separators":            {"archive LinkedIn notifications", []string{"archive LinkedIn notifications"}},
		"only separators":          {" , ;\n. ", nil},
	} {
		t.Run(name, func(t *testing.T) {
			var got []string
			for _, p := range segment(tc.text) {
				got = append(got, tc.text[p.start:p.end])
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("segment(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

// A draft's wording is cut from the owner's text by piece number, never written by the model.
func TestSaidIsTheOwnersWords(t *testing.T) {
	const text = "Put Swiggy in Food, newsletters to Reading, mark them read. Trash scams"
	for name, tc := range map[string]struct {
		parts string
		prior string
		want  string
	}{
		"one piece":           {`[2]`, "", "newsletters to Reading"},
		"contiguous run":      {`[2,3]`, "", "newsletters to Reading, mark them read."},
		"separate runs":       {`[4,1]`, "", "Put Swiggy in Food … Trash scams"},
		"invalid and repeats": {`[0,1,1,9,-3]`, "", "Put Swiggy in Food"},
		"nothing valid":       {`[0,9]`, "", text},
		"missing":             {`null`, "", text},
		"not numbers":         {`["Put Swiggy in Food"]`, "", text},
		"re-optimize":         {`[3]`, "Swiggy goes to Food", "Swiggy goes to Food\nmark them read."},
		"re-optimize no new":  {`[]`, "Swiggy goes to Food", "Swiggy goes to Food"},
	} {
		t.Run(name, func(t *testing.T) {
			raw := `{"name":"Food","intent":"Food delivery","actions":[{"type":"keep"}],"parts":` + tc.parts + `}`
			if d := readDraft(json.RawMessage(raw), text, newWording(text, tc.prior), nil, nil); d.Said != tc.want {
				t.Errorf("said = %q, want %q", d.Said, tc.want)
			}
		})
	}
}

// Whatever the model writes, every element becomes a card: what cannot be read is left out
// and what is wrong is attached to the card.
func TestMalformedDraftsBecomeCardsWithErrors(t *testing.T) {
	existing := []rules.Rule{{ID: 12, Name: "Receipts"}}
	folders := []string{"INBOX", "Food"}
	const said = "Put Swiggy in Food and newsletters in Reading"
	for name, tc := range map[string]struct {
		raw     string
		errPath string // "" = the draft is fine
		check   func(d Draft) bool
	}{
		"fine": {`{"name":"Food","said":"Put Swiggy in Food","intent":null,"conditions":{"field":"from_domain","op":"eq","value":"swiggy.in"},"exceptions":{},"actions":[{"type":"move","folder":"Food"}],"min_confidence":null,"new_folders":[],"question":null,"conflicts":[{"rule_id":12,"kind":"overlap","note":"Receipts also takes Swiggy invoices"}]}`,
			"", func(d Draft) bool {
				return len(d.Conflicts) == 1 && len(d.NewFolders) == 0 && d.Intent == nil && d.Question == nil
			}},
		"not an object":       {`"move swiggy to food"`, "", func(d Draft) bool { return len(d.Errors) == 1 && d.Errors[0].Path == "" }},
		"missing fields":      {`{"said":"Put Swiggy in Food"}`, "name", nil},
		"no actions":          {`{"name":"Food","intent":"Food delivery"}`, "actions", nil},
		"wrong type":          {`{"name":"Food","intent":"Food delivery","actions":"move to Food"}`, "actions", func(d Draft) bool { return len(d.Actions) == 0 }},
		"unknown rule field":  {`{"name":"Food","conditions":{"field":"sender","op":"eq","value":"swiggy.in"},"actions":[{"type":"keep"}]}`, "conditions.field", nil},
		"unknown cond key":    {`{"name":"Food","intent":"Food delivery","conditions":{"feild":"from"},"actions":[{"type":"keep"}]}`, "conditions", func(d Draft) bool { return d.Conditions.IsEmpty() }},
		"unknown operator":    {`{"name":"Food","conditions":{"all":[{"field":"subject","op":"sounds_like","value":"x"}]},"actions":[{"type":"keep"}]}`, "conditions.all[0].op", nil},
		"unknown action":      {`{"name":"Food","intent":"Food delivery","actions":[{"type":"shred"}]}`, "actions[0].type", nil},
		"trash without a bar": {`{"name":"Scams","intent":"Fake bank alerts","actions":[{"type":"trash"}]}`, "min_confidence", nil},
		"invented folder":     {`{"name":"Promos","intent":"Promotions","actions":[{"type":"move","folder":"Promotions"}],"new_folders":["Promotions"]}`, "actions[0].folder", func(d Draft) bool { return len(d.NewFolders) == 0 }},
		"named new folder": {`{"name":"Reading","intent":"Newsletters","actions":[{"type":"move","folder":"Reading"}],"new_folders":[]}`,
			"", func(d Draft) bool { return len(d.NewFolders) == 1 && d.NewFolders[0] == "Reading" }},
		"more than one question": {`{"name":"Food","intent":"Food delivery","actions":[{"type":"keep"}],"question":["Trash or archive?","And receipts too?"]}`,
			"", func(d Draft) bool { return d.Question != nil && *d.Question == "Trash or archive?" }},
		"two questions in one": {`{"name":"Food","intent":"Food delivery","actions":[{"type":"keep"}],"question":"Trash or archive? And receipts too?"}`,
			"", func(d Draft) bool { return d.Question != nil && *d.Question == "Trash or archive?" }},
		"conflict with no such rule": {`{"name":"Food","intent":"Food delivery","actions":[{"type":"keep"}],"conflicts":[{"rule_id":99,"kind":"overlap","note":"x"},{"rule_id":12,"kind":"clash","note":"x"}]}`,
			"", func(d Draft) bool { return len(d.Conflicts) == 0 }},
	} {
		t.Run(name, func(t *testing.T) {
			d := readDraft(json.RawMessage(tc.raw), said, newWording(said, ""), existing, folders)
			if tc.errPath == "" && tc.check == nil && len(d.Errors) != 0 {
				t.Fatalf("errors = %v", d.Errors)
			}
			if tc.errPath != "" && (len(d.Errors) == 0 || d.Errors[0].Path != tc.errPath) {
				t.Fatalf("errors = %v, want one at %q", d.Errors, tc.errPath)
			}
			if tc.check != nil && !tc.check(d) {
				t.Fatalf("draft = %+v", d)
			}
			// A card always encodes with every list present, so the UI never meets a null.
			b, err := json.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"actions", "new_folders", "conflicts", "errors", "samples"} {
				if strings.Contains(string(b), `"`+key+`":null`) {
					t.Errorf("card JSON has a null %s: %s", key, b)
				}
			}
		})
	}
}

// env is a store with one account and its fake mailbox.
type env struct {
	st   *store.Store
	mb   *mailtest.Mailbox
	acct store.Account
	user store.User
	db   *sql.DB
}

// count is how many rows a query counts.
func (e *env) count(t *testing.T, query string) int {
	t.Helper()
	var n int
	if err := e.db.QueryRowContext(t.Context(), query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := t.Context()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	e := &env{st: store.New(db), mb: mailtest.New(1), db: db}
	if e.user, err = e.st.CreateFirstUser(ctx, "me@example.test", "hash", 1); err != nil {
		t.Fatal(err)
	}
	e.acct, err = e.st.CreateAccount(ctx, make([]byte, 32), store.Account{UserID: e.user.ID, Label: "me", Preset: "generic",
		Host: "imap.example.test", Port: 993, TLSMode: "implicit", Username: "me@example.test", WatchFolder: "INBOX", CreatedAt: 1}, "pw")
	if err != nil {
		t.Fatal(err)
	}
	e.mb.AddFolder("Archive", mail.RoleArchive)
	e.mb.AddFolder("Trash", mail.RoleTrash)
	if err := e.st.SaveFolders(ctx, e.acct.ID, []store.Folder{{Name: "INBOX"}, {Name: "Archive", SpecialUse: mail.RoleArchive}, {Name: "Trash", SpecialUse: mail.RoleTrash}}); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) deliver(from, subject string) {
	e.mb.Deliver("INBOX", "From: "+from+"\r\nTo: me@example.test\r\nSubject: "+subject+"\r\n\r\nThe body of "+subject+".\r\n")
}

// decider answers like a decision model that reads the subject: it picks the candidate
// whose name the subject mentions, at 0.95.
func decider(st *store.Store) *models.Router {
	return (&models.Router{Usage: store.Ledger{Store: st, TenantID: 1}, Primary: &models.Fake{NameValue: "fake", DecideFunc: func(req models.DecideRequest) (models.Decision, models.Usage, error) {
		u := models.Usage{Provider: "fake", Model: "fake-1", TokensIn: 100, TokensOut: 5, CostUSD: 0.001}
		for _, c := range req.Candidates {
			if strings.Contains(strings.ToLower(req.Email.Subject), strings.ToLower(c.Name)) {
				return models.Decision{RuleID: c.RuleID, Confidence: 0.95, Reason: "The subject says " + c.Name}, u, nil
			}
		}
		return models.Decision{Confidence: 0.9}, u, nil
	}}}).For("test")
}

const fiveDrafts = `{"rules":[
 {"name":"Food","parts":[1],"intent":null,"conditions":{"all":[{"field":"from_domain","op":"in","value":["swiggy.in","zomato.com"]}]},"exceptions":{},"actions":[{"type":"move","folder":"Food"}],"min_confidence":null,"new_folders":["Food"],"question":null,"conflicts":[]},
 {"name":"Jobs","parts":[2],"intent":"Recruiter outreach about job openings","conditions":{},"exceptions":{"field":"replied_before","op":"eq","value":true},"actions":[{"type":"move","folder":"Jobs"}],"min_confidence":null,"new_folders":["Jobs"],"question":null,"conflicts":[]},
 {"name":"Scams","parts":[3],"intent":"Fake bank alerts and phishing","conditions":{},"exceptions":{},"actions":[{"type":"trash"}],"min_confidence":0.9,"new_folders":[],"question":"Trash, or move to Junk?","conflicts":[]},
 {"name":"Invoices","parts":[4],"intent":null,"conditions":{"all":[{"field":"subject","op":"contains","value":"invoice"}]},"exceptions":{},"actions":[{"type":"flag"}],"min_confidence":null,"new_folders":[],"question":null,"conflicts":[]},
 {"name":"LinkedIn","parts":[5],"intent":null,"conditions":{"field":"from_domain","op":"eq","value":"linkedin.com"},"exceptions":{},"actions":[{"type":"archive"}],"min_confidence":null,"new_folders":[],"question":null,"conflicts":[]}
],"unparsed":[]}`

// The milestone's demo: a paragraph with five instructions becomes five drafts, each
// tested on the account's recent mail, and nothing is saved or touched.
func TestFiveInstructionsBecomeFiveTestedDrafts(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	for i := range 3 {
		e.deliver("Swiggy <noreply@swiggy.in>", fmt.Sprintf("order %d", i))
	}
	e.deliver("orders@zomato.com", "your order")
	e.deliver("priya@talentbridge.in", "A role for you: jobs at Acme")
	e.deliver("alerts@hdfc-secure-verify.co", "Scams: your account is blocked")
	e.deliver("billing@vendor.example", "Invoice 42")
	e.deliver("notifications@linkedin.com", "You appeared in 9 searches")
	e.deliver("friend@example.org", "lunch on friday?")

	var system, user string
	gen := &models.Fake{GenerateFunc: func(sys, usr string, _ json.RawMessage, out any) (models.Usage, error) {
		system, user = sys, usr
		return models.Usage{Provider: "anthropic", Model: "claude-haiku-4-5", TokensIn: 900, TokensOut: 400, CostUSD: 0.003}, json.Unmarshal([]byte(fiveDrafts), out)
	}}
	now := time.Unix(1_800_000_000, 0)
	c := Composer{Store: e.st, Gen: gen, Now: func() time.Time { return now }, BodyChars: 2000}
	req := Request{Viewer: e.user.Viewer(), Text: paragraph, Account: &e.acct, Mailbox: e.mb,
		Decider: pipeline.Decider{Router: decider(e.st), MinConfidence: 0.75, Now: now}}
	out, err := c.Compose(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(user, "[1] Put all Swiggy and Zomato stuff in Food, [2] recruiter") || !strings.Contains(user, "Folders that exist: Archive, INBOX, Trash") || !strings.Contains(system, "from_domain (string)") {
		t.Errorf("the model was not given the text, the folders and the fields:\n%s\n%s", system, user)
	}
	if len(out.Drafts) != 5 || len(out.Unparsed) != 0 {
		t.Fatalf("%d drafts, unparsed %v", len(out.Drafts), out.Unparsed)
	}
	want := map[string]int{"Food": 4, "Jobs": 1, "Scams": 1, "Invoices": 1, "LinkedIn": 1}
	const delivered = 9 // fewer than DefaultLimit: each card says it was tested on these 9 (MAI-42)
	for _, d := range out.Drafts {
		if len(d.Errors) != 0 || d.MatchCount != want[d.Name] || len(d.Samples) != d.MatchCount || d.Tested != delivered {
			t.Errorf("draft %s: %d matches (want %d) of %d tested, %d samples, errors %v", d.Name, d.MatchCount, want[d.Name], d.Tested, len(d.Samples), d.Errors)
		}
		if !strings.Contains(paragraph, d.Said) || d.Said == "" || strings.HasSuffix(d.Said, ",") {
			t.Errorf("draft %s: said %q is not the owner's words", d.Name, d.Said)
		}
	}
	if out.Drafts[1].Said != "recruiter emails go to Jobs unless I've talked to them before" {
		t.Errorf("Jobs said %q", out.Drafts[1].Said)
	}
	food, jobs, scams := out.Drafts[0], out.Drafts[1], out.Drafts[2]
	if fmt.Sprint(food.NewFolders) != "[Food]" || fmt.Sprint(jobs.NewFolders) != "[Jobs]" || scams.Question == nil || *scams.MinConfidence != 0.9 {
		t.Errorf("drafts = %+v", out.Drafts)
	}
	if s := food.Samples[0]; s.From != "orders@zomato.com" || s.Stage != "condition" || s.RuleID != nil || s.RuleName != "Food" || len(s.Actions) != 1 {
		t.Errorf("newest Food sample = %+v", s)
	}
	if s := jobs.Samples[0]; s.Stage != "decider" || s.Confidence != 0.95 || s.Reason != "The subject says Jobs" {
		t.Errorf("Jobs sample = %+v", s)
	}

	// Nothing was saved, and both kinds of model call are on the ledger under their purpose.
	for _, table := range []string{"rules", "messages", "decisions", "actions"} {
		if n := e.count(t, `SELECT COUNT(*) FROM `+table); n != 0 {
			t.Errorf("compose wrote %d rows to %s", n, table)
		}
	}
	composeCalls := e.count(t, `SELECT COALESCE(SUM(calls), 0) FROM usage_daily WHERE purpose = 'compose' AND model = 'claude-haiku-4-5'`)
	testCalls := e.count(t, `SELECT COALESCE(SUM(calls), 0) FROM usage_daily WHERE purpose = 'test'`)
	if composeCalls != 1 || testCalls != 5 { // five emails got past the Food conditions to the model
		t.Errorf("ledger: %d compose calls, %d test calls", composeCalls, testCalls)
	}

	// With no decision model, only what conditions settle is counted.
	req.Decider.Router = nil
	out, err = c.Compose(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range out.Drafts {
		if d.Intent != nil && (d.MatchCount != 0 || d.Tested != 0) || d.Intent == nil && (d.MatchCount != want[d.Name] || d.Tested != delivered) {
			t.Errorf("without a model, draft %s has %d matches of %d tested", d.Name, d.MatchCount, d.Tested)
		}
	}
	// With the account offline the drafts still come back, untested.
	req.Mailbox = nil
	if out, err = c.Compose(ctx, req); err != nil || len(out.Drafts) != 5 || out.Drafts[0].MatchCount != 0 || out.Drafts[0].Tested != 0 {
		t.Errorf("offline: %v, %+v", err, out.Drafts)
	}
}

func TestComposeFailures(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	answer := ""
	var genErr error
	c := Composer{Store: e.st, Gen: &models.Fake{GenerateFunc: func(_, _ string, _ json.RawMessage, out any) (models.Usage, error) {
		if genErr != nil {
			return models.Usage{Provider: "anthropic", Model: "m"}, genErr
		}
		return models.Usage{Provider: "anthropic", Model: "m", TokensIn: 10}, json.Unmarshal([]byte(answer), out)
	}}}
	req := Request{Viewer: e.user.Viewer(), Text: "Put Swiggy in Food", Account: &e.acct}

	// The model answered with something that is not JSON at all: the adapters report that.
	genErr = fmt.Errorf("anthropic: %w: answer is not the expected JSON", models.ErrBadOutput)
	if _, err := c.Compose(ctx, req); !errors.Is(err, ErrModel) || !errors.Is(err, models.ErrBadOutput) {
		t.Errorf("not JSON: %v", err)
	}
	genErr = nil
	for _, bad := range []string{`[1,2]`, `{"rules":"none"}`} {
		answer = bad
		if _, err := c.Compose(ctx, req); !errors.Is(err, ErrModel) {
			t.Errorf("%s: %v", bad, err)
		}
	}
	// No rules at all is an answer: everything went to unparsed.
	answer = `{"unparsed":[1]}`
	if out, err := c.Compose(ctx, req); err != nil || len(out.Drafts) != 0 || fmt.Sprint(out.Unparsed) != "[Put Swiggy in Food]" {
		t.Errorf("only unparsed: %v %+v", err, out)
	}
	// One bad element does not take the others down.
	answer = `{"rules":[42,{"name":"Food","parts":[1],"conditions":{"field":"from_domain","op":"eq","value":"swiggy.in"},"actions":[{"type":"move","folder":"Food"}]}],"unparsed":"nothing"}`
	out, err := c.Compose(ctx, req)
	if err != nil || len(out.Drafts) != 2 || len(out.Drafts[0].Errors) != 1 || len(out.Drafts[1].Errors) != 0 || out.Unparsed == nil {
		t.Fatalf("mixed answer: %v %+v", err, out)
	}

	// Re-optimize answers with exactly one draft; the rule's first wording counts as named.
	rule := rules.Rule{ID: 7, Name: "Food", Said: "Put Swiggy in Food"}
	req.Rule, req.Text = &rule, "also mark them read"
	answer = `{"rules":[{"name":"Food","parts":[1],"conditions":{"field":"from_domain","op":"eq","value":"swiggy.in"},"actions":[{"type":"move","folder":"Food"},{"type":"read"}]},{"name":"Extra","intent":"x","actions":[{"type":"keep"}]}],"unparsed":[]}`
	if out, err = c.Compose(ctx, req); err != nil || len(out.Drafts) != 1 || len(out.Drafts[0].Errors) != 0 || fmt.Sprint(out.Drafts[0].NewFolders) != "[Food]" ||
		out.Drafts[0].Said != "Put Swiggy in Food\nalso mark them read" {
		t.Errorf("re-optimize: %v %+v", err, out)
	}
	answer = `{"rules":[],"unparsed":[]}`
	if _, err = c.Compose(ctx, req); !errors.Is(err, ErrModel) {
		t.Errorf("re-optimize with no draft: %v", err)
	}
}

// The tester reads and nothing else: every message stays unread and where it was, and no
// message, decision or action row is written.
func TestTesterNeverTouchesTheMailbox(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	for i := range 30 {
		e.deliver("noreply@swiggy.in", fmt.Sprintf("order %d", i))
		e.deliver("hello@news.example", fmt.Sprintf("Reading issue %d", i))
	}
	rs := []rules.Rule{
		{ID: 1, Name: "Food", Enabled: true, Priority: 1, Conditions: cond(t, `{"field":"from_domain","op":"eq","value":"swiggy.in"}`),
			Actions: []rules.Action{{Type: rules.ActMove, Folder: "Food"}, {Type: rules.ActRead}}},
		{ID: -1, Name: "Reading", Enabled: true, Priority: 2, Intent: "Newsletters", Actions: []rules.Action{{Type: rules.ActTrash}}},
	}
	var steps []Progress
	tester := Tester{Store: e.st, Mailbox: e.mb, AccountID: e.acct.ID, BodyChars: 2000,
		Decider: pipeline.Decider{Router: decider(e.st), MinConfidence: 0.75, Now: time.Unix(1_800_000_000, 0)}}
	res, err := tester.Run(ctx, rs, nil, "INBOX", 50, func(p Progress) { steps = append(steps, p) })
	if err != nil {
		t.Fatal(err)
	}
	if res.Tested != 50 || res.Matched != 50 || res.ModelCalls != 25 || res.CostUSD < 0.0249 {
		t.Fatalf("result = tested %d matched %d calls %d", res.Tested, res.Matched, res.ModelCalls)
	}
	// Progress opens with 0 of 50 once the mail is listed, then moves one email at a time
	// to 50 of 50, and the model calls only ever grow to the total.
	if len(steps) != 51 || steps[0] != (Progress{Total: 50}) || steps[50].Done != 50 || steps[50].ModelCalls != res.ModelCalls {
		t.Fatalf("progress = %d steps, first %+v, last %+v", len(steps), steps[0], steps[len(steps)-1])
	}
	for i, p := range steps {
		if p.Done != i || p.Total != 50 || (i > 0 && p.ModelCalls < steps[i-1].ModelCalls) {
			t.Fatalf("step %d = %+v", i, p)
		}
	}
	// Newest first; a saved rule is named by id, a draft by name only.
	if r := res.Rows[0]; r.Subject != "Reading issue 29" || r.RuleID != nil || r.RuleName != "Reading" || r.Stage != "decider" || r.Actions[0].Type != "trash" {
		t.Errorf("row 0 = %+v", r)
	}
	if r := res.Rows[1]; r.Subject != "order 29" || r.RuleID == nil || *r.RuleID != 1 || r.Stage != "condition" || len(r.Actions) != 2 {
		t.Errorf("row 1 = %+v", r)
	}

	refs, _, err := e.mb.FetchSince(ctx, "INBOX", time.Time{}, 0)
	if err != nil || len(refs) != 60 {
		t.Fatalf("INBOX holds %d messages (%v), want all 60", len(refs), err)
	}
	for _, ref := range refs {
		if flags, _ := e.mb.Flags(ctx, ref); len(flags) != 0 {
			t.Fatalf("message %d has flags %v after the test", ref.UID, flags)
		}
	}
	folders, _ := e.mb.Folders(ctx)
	if len(folders) != 3 {
		t.Errorf("the test created a folder: %v", folders)
	}
	for _, table := range []string{"messages", "decisions", "actions", "batches"} {
		if n := e.count(t, `SELECT COUNT(*) FROM `+table); n != 0 {
			t.Errorf("the test wrote %d rows to %s", n, table)
		}
	}

	// A folder that does not exist, and a model that fails, end the run with the cause.
	if _, err := tester.Run(ctx, rs, nil, "Nope", 10, nil); !errors.Is(err, mail.ErrNoFolder) {
		t.Errorf("unknown folder: %v", err)
	}
	tester.Decider.Router = &models.Router{Primary: &models.Fake{DecideFunc: func(models.DecideRequest) (models.Decision, models.Usage, error) {
		return models.Decision{}, models.Usage{}, &models.StatusError{Provider: "fake", Code: 500}
	}}}
	if _, err := tester.Run(ctx, rs, nil, "INBOX", 10, nil); !errors.Is(err, pipeline.ErrModel) {
		t.Errorf("failing model: %v", err)
	}
}

// Reader has no method that could change a mailbox; this keeps it that way.
var _ Reader = (interface {
	FetchMany(ctx context.Context, refs []mail.MsgRef, maxBody int) ([]*message.Raw, error)
	FetchSince(ctx context.Context, folder string, since time.Time, limit int) ([]mail.MsgRef, int, error)
})(nil)

// lagging is a Reader whose fetches take a while.
type lagging struct {
	Reader
	lag time.Duration
}

func (l lagging) FetchMany(ctx context.Context, refs []mail.MsgRef, maxBody int) ([]*message.Raw, error) {
	time.Sleep(l.lag)
	return l.Reader.FetchMany(ctx, refs, maxBody)
}

// A test run at debug level says what it is doing, and none of it is the mail: not a
// subject, a sender, a body, nor the key the model was called with.
func TestTesterLogsNeverCarryMailOrKeys(t *testing.T) {
	const (
		key     = "sk-test-SECRET-4471"
		subject = "Wire the quarterly-bonus-zeta payment"
		sender  = "ceo.zeta@bigcorp-zeta.example"
		body    = "The body of " + subject
	)
	e := newEnv(t)
	for range 6 {
		e.deliver(sender, subject)
	}
	// The decision model is a real adapter: Clef over HTTP, called with the key, the
	// subject and the sender, and failing once so the retry is logged too.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if !bytes.Contains(raw, []byte(subject)) || r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "the call lacks what this test is about", http.StatusBadRequest)
			return
		}
		if hits.Add(1) == 1 {
			http.Error(w, subject+sender, http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"result":{"answers":{"rule":{"choice":"rule_1","probabilities":{"rule_1":0.97,"none":0.03}}},"usage":{"input_tokens":50,"output_tokens":0}}}`)
	}))
	t.Cleanup(srv.Close)
	caller := models.NewCaller(2)
	caller.Sleep = func(context.Context, time.Duration) error { return nil }
	router := (&models.Router{Usage: store.Ledger{Store: e.st, TenantID: 1}, Primary: models.NewClef(srv.URL, "acct", key, "clef", models.Deps{Caller: caller, Prices: models.DefaultPrices()})}).For("test")

	var out bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	oldSlow := slowFetch
	slowFetch = time.Millisecond
	t.Cleanup(func() { slowFetch = oldSlow })

	rs := []rules.Rule{{ID: 1, Name: "Money", Enabled: true, Priority: 1, Intent: "Payment requests", Actions: []rules.Action{{Type: rules.ActFlag}}}}
	tester := Tester{Store: e.st, Mailbox: lagging{e.mb, 5 * time.Millisecond}, AccountID: e.acct.ID, BodyChars: 2000,
		Decider: pipeline.Decider{Router: router, MinConfidence: 0.75, Now: time.Unix(1_800_000_000, 0)}}
	res, err := tester.Run(t.Context(), rs, nil, "INBOX", 6, func(Progress) {})
	if err != nil || res.Tested != 6 || res.Matched != 6 {
		t.Fatalf("run = %+v, %v", res, err)
	}

	log := out.String()
	seen := map[string]bool{}
	for line := range strings.SplitSeq(strings.TrimSpace(log), "\n") {
		var v map[string]any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("not a log line: %q", line)
		}
		seen[v["msg"].(string)] = true
	}
	for _, msg := range []string{"model call", "mail listed", "mail fetched", "slow mail fetch"} {
		if !seen[msg] {
			t.Errorf("no %q line at debug level; got %v", msg, seen)
		}
	}
	for _, text := range []string{key, subject, "quarterly-bonus", sender, "bigcorp-zeta", body, "Bearer"} {
		if strings.Contains(log, text) {
			t.Errorf("the log contains %q:\n%s", text, log)
		}
	}
}

// Check runs the real decision flow over a folder and returns one row per email: its mail
// reference, its display fields and the settled outcome, newest first. Progress opens at
// 0 of N and ends at N of N, with the model calls and cost only growing.
func TestTesterCheckReturnsRowsWithRefs(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	for i := range 3 {
		e.deliver("noreply@swiggy.in", fmt.Sprintf("order %d", i))
	}
	for i := range 2 {
		e.deliver("hello@news.example", fmt.Sprintf("Reading issue %d", i))
	}
	rs := []rules.Rule{
		{ID: 1, Name: "Food", Enabled: true, Priority: 1, Conditions: cond(t, `{"field":"from_domain","op":"eq","value":"swiggy.in"}`),
			Actions: []rules.Action{{Type: rules.ActMove, Folder: "Food"}}},
		{ID: 2, Name: "Reading", Enabled: true, Priority: 2, Intent: "Newsletters", Conditions: cond(t, `{"field":"from_domain","op":"eq","value":"news.example"}`),
			Actions: []rules.Action{{Type: rules.ActTrash}}},
	}
	var steps []CheckProgress
	tester := Tester{Store: e.st, Mailbox: e.mb, AccountID: e.acct.ID, BodyChars: 2000,
		Decider: pipeline.Decider{Router: decider(e.st), MinConfidence: 0.75, Now: time.Unix(1_800_000_000, 0)}}
	rows, err := tester.Check(ctx, rs, nil, "INBOX", time.Time{}, 0, func(p CheckProgress) { steps = append(steps, p) })
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 {
		t.Fatalf("%d rows, want 5", len(rows))
	}
	// Newest first: a newsletter the model decided, carrying a confidence; its ref is real.
	r0 := rows[0]
	if r0.Subject != "Reading issue 1" || r0.From == "" || r0.Ref.Folder != "INBOX" || r0.Ref.UID == 0 || r0.Ref.UIDValidity == 0 ||
		r0.Outcome.Stage != rules.StageDecider || r0.Outcome.RuleID != 2 || !r0.Outcome.Asked || r0.Outcome.Confidence != 0.95 {
		t.Errorf("row 0 = %+v", r0)
	}
	// The oldest row: a swiggy order a condition settled, no model asked.
	last := rows[len(rows)-1]
	if last.Subject != "order 0" || last.Outcome.Stage != rules.StageCondition || last.Outcome.RuleID != 1 || last.Outcome.Asked {
		t.Errorf("last row = %+v", last)
	}
	// Every row's reference points at a message still in the inbox.
	for _, r := range rows {
		if _, err := e.mb.Fetch(ctx, r.Ref, 0); err != nil {
			t.Errorf("row ref %+v is not fetchable: %v", r.Ref, err)
		}
	}
	// Progress: 0 of 5 first, 5 of 5 last; two model calls, their tokens and cost.
	if len(steps) != 6 || steps[0] != (CheckProgress{Total: 5, Matched: 5}) {
		t.Fatalf("progress = %d steps, first %+v", len(steps), steps[0])
	}
	if lp := steps[5]; lp.Done != 5 || lp.Total != 5 || lp.Matched != 5 || lp.ModelCalls != 2 || lp.Tokens != 2*105 || lp.CostUSD < 0.0019 || lp.CostUSD > 0.0021 {
		t.Errorf("last progress = %+v", lp)
	}
	for i, p := range steps {
		if p.Done != i || p.Total != 5 || p.Matched != 5 || (i > 0 && (p.ModelCalls < steps[i-1].ModelCalls || p.CostUSD < steps[i-1].CostUSD)) {
			t.Fatalf("step %d = %+v", i, p)
		}
	}
}

// A check says how many emails the range held before its limit cut it: the number checked
// when the range fits, the whole range when it does not, the newest N below the cap and a
// start time alone above it.
func TestTesterCheckReportsHowManyMatchedBeforeTheLimit(t *testing.T) {
	e := newEnv(t)
	const stored = MaxLimit + 100
	for i := range stored {
		e.deliver("friend@example.org", fmt.Sprintf("note %d", i))
	}
	tester := Tester{Store: e.st, Mailbox: e.mb, AccountID: e.acct.ID, BodyChars: 200,
		Decider: pipeline.Decider{MinConfidence: 0.75, Now: time.Unix(1_800_000_000, 0)}}
	hourAgo, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	for _, tc := range []struct {
		name           string
		since          time.Time
		limit          int
		total, matched int
	}{
		{"newest N below the cap", time.Time{}, 7, 7, stored},
		{"newest N of exactly the range", time.Time{}, stored, stored, stored},
		{"the cap, range above it", time.Time{}, MaxLimit, MaxLimit, stored},
		{"a start time and the cap, range above it", hourAgo, MaxLimit, MaxLimit, stored},
		{"a start time with no limit sees the whole range", hourAgo, 0, stored, stored},
		{"a start time nothing is after", future, MaxLimit, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var first, last CheckProgress
			rows, err := tester.Check(t.Context(), nil, nil, "INBOX", tc.since, tc.limit, func(p CheckProgress) {
				if p.Done == 0 {
					first = p
				}
				last = p
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != tc.total || first.Total != tc.total || first.Matched != tc.matched || last.Done != tc.total || last.Matched != tc.matched {
				t.Errorf("%d rows, first %+v, last %+v; want %d checked of %d", len(rows), first, last, tc.total, tc.matched)
			}
		})
	}
}
