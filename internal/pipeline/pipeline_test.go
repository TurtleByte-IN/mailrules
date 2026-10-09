package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/actions"
	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/learn"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/mailtest"
	"github.com/TurtleByte-IN/mailrules/internal/message"
	"github.com/TurtleByte-IN/mailrules/internal/models"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// recorder is the executor the pipeline tests use: it remembers what it was asked to
// apply and changes nothing.
type recorder struct {
	calls []call
	err   error // returned by Apply
}

type call struct {
	rec  actions.DecisionRecord
	acts []rules.Action
}

func (r *recorder) Apply(_ context.Context, d actions.DecisionRecord, acts []rules.Action, _ int64) ([]actions.ActionRecord, error) {
	r.calls = append(r.calls, call{d, acts})
	return nil, r.err
}

// applied lists the actions applied so far, e.g. "move:Food".
func (r *recorder) applied() []string {
	var out []string
	for _, c := range r.calls {
		for _, a := range c.acts {
			out = append(out, a.String())
		}
	}
	return out
}

// addCutOff adds a condition-only rule for swiggy.example below the intent rules: the
// cut-off, the default when the decider picks none of them.
func (e *env) addCutOff() rules.Rule {
	r, err := e.st.CreateRule(e.t.Context(), 1, rules.Rule{UserID: e.user.ID, Name: "Orders", Priority: 9, Enabled: true,
		Conditions: rules.Cond{Field: "from_domain", Op: rules.OpEq, Value: "swiggy.example"},
		Actions:    []rules.Action{{Type: rules.ActMove, Folder: "Orders"}}}, 1)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

type env struct {
	t        *testing.T
	db       *sql.DB
	st       *store.Store
	mb       *mailtest.Mailbox
	p        *Pipeline
	exec     *recorder
	primary  *models.Fake
	fallback *models.Fake
	now      time.Time
	user     store.User
	food     rules.Rule // intent, move:Food
	receipts rules.Rule // intent, move:Receipts, min_confidence 0.6
	reading  rules.Rule // condition-only on news.example, move:Reading
}

func answer(ruleID int64, confidence float64) func(models.DecideRequest) (models.Decision, models.Usage, error) {
	return func(models.DecideRequest) (models.Decision, models.Usage, error) {
		return models.Decision{RuleID: ruleID, Confidence: confidence}, models.Usage{Provider: "fake", Model: "primary-1", TokensIn: 100, TokensOut: 5, CostUSD: 0.001}, nil
	}
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
	e := &env{t: t, db: db, st: store.New(db), exec: &recorder{}, now: time.Unix(1_800_000_000, 0),
		primary: &models.Fake{NameValue: "primary"}, fallback: &models.Fake{NameValue: "fallback"}}
	if e.user, err = e.st.CreateFirstUser(ctx, "me@example.test", "hash", 1); err != nil {
		t.Fatal(err)
	}
	acct, err := e.st.CreateAccount(ctx, make([]byte, 32), store.Account{UserID: e.user.ID, Label: "x", Preset: "generic", Host: "h", Port: 993, TLSMode: "implicit", Username: "me"}, "pw")
	if err != nil {
		t.Fatal(err)
	}
	rule := func(name, intent string, cond rules.Cond, folder string, priority int, minConf *float64) rules.Rule {
		r, err := e.st.CreateRule(ctx, 1, rules.Rule{UserID: e.user.ID, Name: name, Intent: intent, Conditions: cond,
			Actions: []rules.Action{{Type: rules.ActMove, Folder: folder}}, Priority: priority, MinConfidence: minConf, Enabled: true}, 1)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	low := 0.6
	e.reading = rule("Reading", "", rules.Cond{Field: "from_domain", Op: rules.OpEq, Value: "news.example"}, "Reading", 0, nil)
	e.food = rule("Food", "Food delivery updates", rules.Cond{}, "Food", 1, nil)
	e.receipts = rule("Receipts", "Invoices and receipts", rules.Cond{}, "Receipts", 2, &low)

	e.mb = mailtest.New(acct.ID)
	e.p = &Pipeline{
		Store: e.st, Mailbox: e.mb, Account: acct, Exec: e.exec, Hub: events.NewHub(),
		Router:        &models.Router{Primary: e.primary, Fallback: e.fallback, EscalateBelow: 0.75, Usage: store.Ledger{Store: e.st, TenantID: 1}, Now: func() time.Time { return e.now }},
		MinConfidence: 0.75, BodyChars: 2000, Now: func() time.Time { return e.now },
	}
	return e
}

func (e *env) deliver(from, subject string) mail.MsgRef {
	return e.deliverWith(from, subject, "")
}

// deliverWith delivers an email carrying extra header lines, each ending in CRLF.
func (e *env) deliverWith(from, subject, headers string) mail.MsgRef {
	return e.mb.Deliver("INBOX", "From: "+from+"\r\nTo: me@example.test\r\nSubject: "+subject+
		"\r\nMessage-ID: <"+strings.ReplaceAll(subject, " ", "-")+"@example.test>\r\n"+headers+"\r\nThe body of "+subject+".\r\n")
}

// process delivers one email, runs it through the pipeline and returns its activity row.
func (e *env) process(from, subject string) store.ActivityRow {
	e.t.Helper()
	return e.processWith(from, subject, "")
}

func (e *env) processWith(from, subject, headers string) store.ActivityRow {
	e.t.Helper()
	ref := e.deliverWith(from, subject, headers)
	if err := e.p.Process(e.t.Context(), ref); err != nil {
		e.t.Fatal(err)
	}
	return e.row(ref)
}

func (e *env) row(ref mail.MsgRef) store.ActivityRow {
	e.t.Helper()
	m, _, err := e.st.IngestMessage(e.t.Context(), ref, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	row, err := e.st.ActivityFor(e.t.Context(), e.user.Viewer(), m.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	if row.Decision == nil {
		row.Decision = &store.Decision{}
	}
	return row
}

// settle decides an email as a cleanup check does (Decider.Settle, the model asked once),
// recording nothing, and returns the answer Pipeline.SortSaved replays.
func (e *env) settle(ref mail.MsgRef) Outcome {
	e.t.Helper()
	ctx := e.t.Context()
	rs, err := e.st.Rules(ctx, 1)
	if err != nil {
		e.t.Fatal(err)
	}
	senders, err := e.st.SenderRules(ctx, 1)
	if err != nil {
		e.t.Fatal(err)
	}
	raw, err := e.mb.Fetch(ctx, ref, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	sum, err := message.Parse(raw, e.p.Account.ID, 2000)
	if err != nil {
		e.t.Fatal(err)
	}
	out, err := Decider{Router: e.p.Router, MinConfidence: 0.75, Now: e.now}.Settle(ctx, *sum, rs, senders)
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func TestProcessEndToEnd(t *testing.T) {
	tests := []struct {
		name        string
		from        string
		setup       func(e *env)
		wantStage   string
		wantRule    func(e *env) int64
		wantState   string
		wantApplied []string
		wantCalls   int // model calls, primary and fallback together
		wantReason  string
		wantEvent   string
	}{
		{
			name: "sender rule routes without a model", from: "orders@swiggy.example",
			setup: func(e *env) {
				_, err := e.st.PutSenderRule(e.t.Context(), 1, rules.SenderRule{UserID: e.user.ID, MatchType: rules.MatchAddress,
					Value: "orders@swiggy.example", RuleID: e.receipts.ID, Verdict: rules.VerdictRoute, Source: "user"}, 1)
				if err != nil {
					e.t.Fatal(err)
				}
			},
			wantStage: "sender", wantRule: func(e *env) int64 { return e.receipts.ID }, wantState: store.StateActed,
			wantApplied: []string{"move:Receipts"}, wantReason: `Sender rule: "Receipts"`, wantEvent: events.MessageProcessed,
		},
		{
			name: "blocked sender is trashed", from: "spam@junk.example",
			setup: func(e *env) {
				_, err := e.st.PutSenderRule(e.t.Context(), 1, rules.SenderRule{UserID: e.user.ID, MatchType: rules.MatchDomain,
					Value: "junk.example", Verdict: rules.VerdictBlock, Source: "user"}, 1)
				if err != nil {
					e.t.Fatal(err)
				}
			},
			wantStage: "sender", wantRule: func(*env) int64 { return 0 }, wantState: store.StateActed,
			wantApplied: []string{"trash"}, wantReason: "Sender rule: trash", wantEvent: events.MessageProcessed,
		},
		{
			name: "condition-only rule acts without a model", from: "weekly@news.example",
			wantStage: "condition", wantRule: func(e *env) int64 { return e.reading.ID }, wantState: store.StateActed,
			wantApplied: []string{"move:Reading"}, wantReason: `Matched "Reading" by its conditions`, wantEvent: events.MessageProcessed,
		},
		{
			name: "decider pick above the threshold acts", from: "orders@swiggy.example",
			setup:     func(e *env) { e.primary.DecideFunc = answer(e.food.ID, 0.93) },
			wantStage: "decider", wantRule: func(e *env) int64 { return e.food.ID }, wantState: store.StateActed,
			wantApplied: []string{"move:Food"}, wantCalls: 1, wantReason: `Matched "Food": Food delivery updates (0.93)`, wantEvent: events.MessageProcessed,
		},
		{
			name: "below the threshold goes to review", from: "orders@swiggy.example",
			setup: func(e *env) {
				e.primary.DecideFunc = answer(e.food.ID, 0.5)
				e.fallback.DecideFunc = answer(e.food.ID, 0.6)
			},
			wantStage: "fallback", wantRule: func(e *env) int64 { return e.food.ID }, wantState: store.StateReview,
			wantCalls: 2, wantReason: `Matched "Food"`, wantEvent: events.MessageReview,
		},
		{
			// MAI-62: an unsure pick is never handed to the cut-off rule; nothing is done.
			name: "below the threshold with a cut-off still goes to review", from: "orders@swiggy.example",
			setup: func(e *env) {
				e.reading = e.addCutOff()
				e.primary.DecideFunc = answer(e.food.ID, 0.5)
				e.fallback.DecideFunc = answer(e.food.ID, 0.6)
			},
			wantStage: "fallback", wantRule: func(e *env) int64 { return e.food.ID }, wantState: store.StateReview,
			wantCalls: 2, wantReason: `Matched "Food"`, wantEvent: events.MessageReview,
		},
		{
			name: "the decider says none with a cut-off: the cut-off acts", from: "orders@swiggy.example",
			setup:     func(e *env) { e.reading = e.addCutOff() },
			wantStage: "condition", wantRule: func(e *env) int64 { return e.reading.ID }, wantState: store.StateActed,
			wantApplied: []string{"move:Orders"}, wantCalls: 1, wantReason: `the default rule "Orders" applied`, wantEvent: events.MessageProcessed,
		},
		{
			name: "escalation is recorded", from: "orders@swiggy.example",
			setup: func(e *env) {
				e.primary.DecideFunc = answer(e.food.ID, 0.5)
				e.fallback.DecideFunc = func(models.DecideRequest) (models.Decision, models.Usage, error) {
					return models.Decision{RuleID: e.receipts.ID, Confidence: 0.95, Reason: "It is an invoice."},
						models.Usage{Provider: "fake", Model: "fallback-1", TokensIn: 300, TokensOut: 20, CostUSD: 0.01}, nil
				}
			},
			wantStage: "fallback", wantRule: func(e *env) int64 { return e.receipts.ID }, wantState: store.StateActed,
			wantApplied: []string{"move:Receipts"}, wantCalls: 2, wantReason: "It is an invoice.", wantEvent: events.MessageProcessed,
		},
		{
			name: "the decider says none: nothing happens", from: "friend@people.example",
			wantStage: "none", wantRule: func(*env) int64 { return 0 }, wantState: store.StateSkipped,
			wantCalls: 1, wantReason: "No rule matched", wantEvent: events.MessageProcessed,
		},
		{
			name: "no decision model: mail that needs one waits in review", from: "orders@swiggy.example",
			setup:     func(e *env) { e.p.Router = nil },
			wantStage: "none", wantRule: func(*env) int64 { return 0 }, wantState: store.StateReview,
			wantReason: ReasonNoModel, wantEvent: events.MessageReview,
		},
		{
			// MAI-23: the rules with an intent sit above it and cannot be decided, so they are
			// passed over and the condition rule below them applies.
			name: "no decision model: a condition rule below the intent rules still acts", from: "orders@swiggy.example",
			setup: func(e *env) {
				e.p.Router = nil
				e.reading = e.addCutOff()
			},
			wantStage: "condition", wantRule: func(e *env) int64 { return e.reading.ID }, wantState: store.StateActed,
			wantApplied: []string{"move:Orders"}, wantReason: `Matched "Orders" by its conditions`, wantEvent: events.MessageProcessed,
		},
		{
			name: "no decision model: condition-only rules still act", from: "weekly@news.example",
			setup:     func(e *env) { e.p.Router = nil },
			wantStage: "condition", wantRule: func(e *env) int64 { return e.reading.ID }, wantState: store.StateActed,
			wantApplied: []string{"move:Reading"}, wantReason: `Matched "Reading"`, wantEvent: events.MessageProcessed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			if tt.setup != nil {
				tt.setup(e)
			}
			_, live, cancel := e.p.Hub.Subscribe(0)
			defer cancel()
			row := e.process(tt.from, "hello there")
			d, m := row.Decision, row.Message

			if d.Stage != tt.wantStage || d.RuleID != tt.wantRule(e) || m.State != tt.wantState || !strings.Contains(d.Reason, tt.wantReason) {
				t.Errorf("stage=%s rule=%d state=%s reason=%q; want stage=%s rule=%d state=%s reason~%q",
					d.Stage, d.RuleID, m.State, d.Reason, tt.wantStage, tt.wantRule(e), tt.wantState, tt.wantReason)
			}
			if got := e.exec.applied(); !slices.Equal(got, tt.wantApplied) {
				t.Errorf("applied %v, want %v", got, tt.wantApplied)
			}
			if got := len(e.primary.Requests()) + len(e.fallback.Requests()); got != tt.wantCalls {
				t.Errorf("%d model calls, want %d", got, tt.wantCalls)
			}
			// The row keeps the headers and a snippet, and the folder position moved on.
			if m.FromAddr != tt.from || m.Subject != "hello there" || m.MessageID != "hello-there@example.test" ||
				!strings.HasPrefix(m.Snippet, "The body of") || m.Signals.DMARC != "none" {
				t.Errorf("stored message = %+v", m)
			}
			if f, err := e.st.Folder(t.Context(), m.AccountID, "INBOX"); err != nil || f.LastUID != m.UID || f.UIDValidity != m.UIDValidity {
				t.Errorf("folder position = %+v, %v; want uid %d", f, err, m.UID)
			}
			// Needs review leaves the mailbox alone: the executor is not even asked.
			if tt.wantState == store.StateReview && len(e.exec.calls) != 0 {
				t.Errorf("executor called %d times for an email in review", len(e.exec.calls))
			}
			var names []string
			for len(live) > 0 {
				ev := <-live
				names = append(names, ev.Name)
				if got, ok := ev.Data.(store.ActivityRow); ok && (got.Message.ID != m.ID || got.Decision.ID != d.ID || got.Message.State != tt.wantState) {
					t.Errorf("event %s carries %+v", ev.Name, got)
				}
			}
			if !slices.Contains(names, tt.wantEvent) || slices.Contains(names, events.UsageUpdated) != (tt.wantCalls > 0) {
				t.Errorf("events = %v, want %s (usage.updated only after a model call)", names, tt.wantEvent)
			}
		})
	}
}

func TestEscalationCostIsRecorded(t *testing.T) {
	e := newEnv(t)
	e.primary.DecideFunc = answer(e.food.ID, 0.5)
	e.fallback.DecideFunc = func(models.DecideRequest) (models.Decision, models.Usage, error) {
		return models.Decision{RuleID: e.food.ID, Confidence: 0.95}, models.Usage{Provider: "fake", Model: "fallback-1", TokensIn: 300, TokensOut: 20, CostUSD: 0.01}, nil
	}
	d := e.process("orders@swiggy.example", "order 1").Decision
	if d.Model != "fallback-1" || d.TokensIn != 400 || d.TokensOut != 25 || d.CostUSD != 0.011 || d.Confidence != 0.95 || d.RuleVersion != 1 {
		t.Errorf("decision = %+v", d)
	}
	// The candidates carry the rule name; the fallback saw the same ones.
	req := e.fallback.Requests()[0]
	if len(req.Candidates) != 2 || req.Candidates[0].Name != "Food" || req.Candidates[0].RuleID != e.food.ID || req.Candidates[1].Name != "Receipts" {
		t.Errorf("candidates = %+v", req.Candidates)
	}
	rows, err := e.db.QueryContext(t.Context(), `SELECT purpose, model, calls, tokens_in FROM usage_daily ORDER BY purpose`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var purpose, model string
		var calls, in int
		if err := rows.Scan(&purpose, &model, &calls, &in); err != nil {
			t.Fatal(err)
		}
		got = append(got, purpose+" "+model)
		if calls != 1 {
			t.Errorf("%s: %d calls", purpose, calls)
		}
	}
	if err := rows.Err(); err != nil || !slices.Equal(got, []string{"decide primary-1", "escalate fallback-1"}) {
		t.Errorf("usage ledger = %v, %v", got, err)
	}
}

func TestProcessIsIdempotent(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.primary.DecideFunc = answer(e.food.ID, 0.9)
	ref := e.deliver("orders@swiggy.example", "order 1")

	// The same arrival reported twice acts once.
	for range 2 {
		if err := e.p.Process(ctx, ref); err != nil {
			t.Fatal(err)
		}
	}
	// A crash after acting but before the folder position was stored: the watcher sends
	// the message again from the old position, and nothing is repeated.
	if err := e.st.SetFolderPosition(ctx, ref.AccountID, "INBOX", ref.UIDValidity, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Process(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if got := e.exec.applied(); !slices.Equal(got, []string{"move:Food"}) || len(e.primary.Requests()) != 1 {
		t.Errorf("applied %v with %d model calls; want one of each", got, len(e.primary.Requests()))
	}
	if f, _ := e.st.Folder(ctx, ref.AccountID, "INBOX"); f.LastUID != ref.UID {
		t.Errorf("last_uid = %d, want %d", f.LastUID, ref.UID)
	}

	// A crash before the outcome was recorded leaves the row unfinished: that one is
	// picked up again rather than lost.
	ref2 := e.deliver("orders@swiggy.example", "order 2")
	m, fresh, err := e.st.IngestMessage(ctx, ref2, 1)
	if err != nil || !fresh || m.State != store.StateNew {
		t.Fatalf("ingest = %+v, %v, %v", m, fresh, err)
	}
	if err := e.p.Process(ctx, ref2); err != nil {
		t.Fatal(err)
	}
	if got := e.row(ref2).Message.State; got != store.StateActed || len(e.exec.applied()) != 2 {
		t.Errorf("unfinished message: state %s, applied %v", got, e.exec.applied())
	}

	// A stopping daemon is not a failure: nothing is recorded and the position stays.
	ref3 := e.deliver("orders@swiggy.example", "order 3")
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	if err := e.p.Process(stopped, ref3); !errors.Is(err, context.Canceled) {
		t.Errorf("Process on a cancelled context = %v", err)
	}
	if f, _ := e.st.Folder(ctx, ref.AccountID, "INBOX"); f.LastUID != ref2.UID {
		t.Errorf("last_uid = %d after a cancelled run, want %d", f.LastUID, ref2.UID)
	}
}

func TestRetryJob(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	boom := &models.StatusError{Provider: "fake", Code: 503}
	e.primary.DecideFunc = func(models.DecideRequest) (models.Decision, models.Usage, error) {
		return models.Decision{}, models.Usage{}, boom
	}

	row := e.process("orders@swiggy.example", "order 1")
	if m := row.Message; m.State != store.StateError || m.Attempts != 0 || m.NextAttemptAt != e.now.Add(RetryEvery).Unix() || row.Decision.ID != 0 {
		t.Fatalf("after the first failure: %+v, decision %+v", m, row.Decision)
	}
	if f, _ := e.st.Folder(ctx, row.Message.AccountID, "INBOX"); f.LastUID != row.Message.UID {
		t.Error("a failed message must still advance the folder: its row is committed and the retry job owns it")
	}
	if n, err := e.p.RetryDue(ctx); n != 0 || err != nil {
		t.Fatalf("retry before its time ran %d, %v", n, err)
	}
	for i := 1; i <= MaxRetries; i++ {
		e.now = e.now.Add(RetryEvery)
		if n, err := e.p.RetryDue(ctx); n != 1 || err != nil {
			t.Fatalf("retry %d ran %d, %v", i, n, err)
		}
		m := e.row(row.Message.Location()).Message
		if want := map[bool]string{true: store.StateError, false: store.StateReview}[i < MaxRetries]; m.State != want || m.Attempts != i {
			t.Fatalf("after retry %d: state %s attempts %d, want %s", i, m.State, m.Attempts, want)
		}
	}
	final := e.row(row.Message.Location())
	if d := final.Decision; d.Stage != "none" || !strings.Contains(d.Reason, "Gave up after 6 retries") || !strings.Contains(d.Reason, "503") {
		t.Errorf("give-up decision = %+v", d)
	}
	if got := len(e.primary.Requests()); got != 1+MaxRetries {
		t.Errorf("%d model calls, want %d", got, 1+MaxRetries)
	}
	e.now = e.now.Add(time.Hour)
	if n, _ := e.p.RetryDue(ctx); n != 0 {
		t.Errorf("a message in review was retried")
	}

	// A model that recovers: the retry finishes the message.
	row = e.process("orders@swiggy.example", "order 2")
	e.primary.DecideFunc = answer(e.food.ID, 0.9)
	e.now = e.now.Add(RetryEvery)
	if n, err := e.p.RetryDue(ctx); n != 1 || err != nil {
		t.Fatal(n, err)
	}
	if m := e.row(row.Message.Location()).Message; m.State != store.StateActed || m.Attempts != 1 || m.NextAttemptAt != 0 || !slices.Equal(e.exec.applied(), []string{"move:Food"}) {
		t.Errorf("after recovery: %+v, applied %v", m, e.exec.applied())
	}
}

func TestUnreadableAnswers(t *testing.T) {
	bad := fmt.Errorf("fake: %w: answer is not the expected JSON", models.ErrBadOutput)
	tests := []struct {
		name        string
		badAnswers  int // unreadable answers before a good one
		wantState   string
		wantApplied []string
		wantReason  string
	}{
		{"two unreadable answers, then a good one: sorted by it", 2, store.StateActed, []string{"move:Food"}, ""},
		{"three unreadable answers: kept", 3, store.StateSkipped, nil, ReasonUnreadable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			n := 0
			e.primary.DecideFunc = func(req models.DecideRequest) (models.Decision, models.Usage, error) {
				n++
				if n <= tt.badAnswers {
					return models.Decision{}, models.Usage{Provider: "fake", Model: "primary-1", TokensIn: 100}, bad
				}
				return answer(e.food.ID, 0.9)(req)
			}
			row := e.process("orders@swiggy.example", "order 1")
			m, d := row.Message, row.Decision
			if m.State != tt.wantState || m.NextAttemptAt != 0 || !slices.Equal(e.exec.applied(), tt.wantApplied) {
				t.Errorf("state %s, next attempt %d, applied %v; want %s, none, %v", m.State, m.NextAttemptAt, e.exec.applied(), tt.wantState, tt.wantApplied)
			}
			if tt.wantApplied == nil && len(e.exec.calls) != 0 {
				t.Errorf("executor called %d times, want never", len(e.exec.calls))
			}
			if got := len(e.primary.Requests()); got != models.ReadTries {
				t.Errorf("%d model calls, want %d", got, models.ReadTries)
			}
			if d.ID == 0 || d.Model != "primary-1" || d.TokensIn != 300 || (tt.wantReason != "" && d.Reason != tt.wantReason) {
				t.Errorf("decision = %+v; want one recorded, costing all %d calls", d, models.ReadTries)
			}
			if n, _ := e.p.RetryDue(t.Context()); n != 0 {
				t.Errorf("the retry job ran %d messages, want none", n)
			}
		})
	}
}

// flaky fails chosen mailbox calls.
type flaky struct {
	*mailtest.Mailbox
	fetchErr error
}

func (f *flaky) Fetch(ctx context.Context, ref mail.MsgRef, maxBody int) (*message.Raw, error) {
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	return f.Mailbox.Fetch(ctx, ref, maxBody)
}

func TestFailures(t *testing.T) {
	tests := []struct {
		name          string
		setup         func(e *env, fl *flaky)
		wantState     string
		wantRetry     bool
		wantRediscons int
	}{
		{"action failure is retried", func(e *env, _ *flaky) { e.exec.err = errors.New("connection reset") }, store.StateError, true, 0},
		{"a server that cannot move is not retried", func(e *env, _ *flaky) { e.exec.err = mail.ErrUnsupported }, store.StateError, false, 0},
		{"fetch failure is retried", func(_ *env, fl *flaky) { fl.fetchErr = mail.ErrConnection }, store.StateError, true, 0},
		{"a message that is gone is skipped", func(_ *env, fl *flaky) { fl.fetchErr = mail.ErrNotFound }, store.StateSkipped, false, 0},
		{"an unknown folder re-runs discovery once", func(e *env, _ *flaky) { e.exec.err = mail.ErrNoFolder }, store.StateError, true, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			fl := &flaky{Mailbox: e.mb}
			e.p.Mailbox = fl
			rediscovered := 0
			e.p.Rediscover = func(context.Context) error { rediscovered++; return nil }
			tt.setup(e, fl)
			m := e.process("weekly@news.example", "issue 1").Message
			if m.State != tt.wantState || (m.NextAttemptAt != 0) != tt.wantRetry || rediscovered != tt.wantRediscons {
				t.Errorf("state %s, next attempt %d, %d rediscoveries; want %s, retry %v, %d",
					m.State, m.NextAttemptAt, rediscovered, tt.wantState, tt.wantRetry, tt.wantRediscons)
			}
		})
	}

	t.Run("a message that cannot be parsed goes to review", func(t *testing.T) {
		e := newEnv(t)
		ref := e.mb.Deliver("INBOX", "this is not a header\r\n\r\nbody")
		if err := e.p.Process(t.Context(), ref); err != nil {
			t.Fatal(err)
		}
		if row := e.row(ref); row.Message.State != store.StateReview || row.Decision.Reason != "This message could not be read" {
			t.Errorf("row = %+v, decision %+v", row.Message, row.Decision)
		}
	})
}

func TestLearnedSenderRule(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	const sender = "orders@swiggy.example"
	learned := func() []rules.SenderRule {
		srs, err := e.st.SenderRules(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		return srs
	}

	// Confident, but not three in a row to the same rule: nothing is learned.
	e.primary.DecideFunc = answer(e.food.ID, 0.95)
	e.process(sender, "order 1")
	e.process(sender, "order 2")
	e.primary.DecideFunc = answer(e.food.ID, 0.8) // acts (>= 0.75) but is not confident enough to learn from
	e.process(sender, "order 3")
	if got := learned(); len(got) != 0 {
		t.Fatalf("learned too early: %+v", got)
	}

	e.primary.DecideFunc = answer(e.food.ID, 0.95)
	e.process(sender, "order 4")
	e.process(sender, "order 5")
	if got := learned(); len(got) != 0 {
		t.Fatalf("learned after two: %+v", got)
	}
	e.process(sender, "order 6")
	got := learned()
	if len(got) != 1 || got[0].Value != sender || got[0].MatchType != rules.MatchAddress || got[0].RuleID != e.food.ID ||
		got[0].Verdict != rules.VerdictRoute || got[0].Source != "learned" {
		t.Fatalf("learned = %+v", got)
	}

	// From now on the sender is routed without a model call.
	calls := len(e.primary.Requests())
	row := e.process(sender, "order 7")
	if row.Decision.Stage != "sender" || row.Decision.RuleID != e.food.ID || len(e.primary.Requests()) != calls {
		t.Errorf("after learning: %+v, %d new model calls", row.Decision, len(e.primary.Requests())-calls)
	}

	// Any correction for that sender deletes the learned rule, and the model is asked again.
	if _, err := e.st.AddCorrection(ctx, 1, e.user.ID, store.Correction{MessageID: row.Message.ID, WrongRuleID: e.food.ID,
		RightRuleID: e.receipts.ID, Example: "{}", CreatedAt: e.now.Unix()}, ""); err != nil {
		t.Fatal(err)
	}
	if got := learned(); len(got) != 0 {
		t.Fatalf("learned rule survived a correction: %+v", got)
	}
	if row := e.process(sender, "order 8"); row.Decision.Stage != "decider" || len(learned()) != 0 {
		t.Errorf("after the correction: %+v, learned %+v", row.Decision, learned())
	}

	// "Always for this sender" stores a user rule instead, which learning never replaces.
	if _, err := e.st.AddCorrection(ctx, 1, e.user.ID, store.Correction{MessageID: row.Message.ID, RightRuleID: e.receipts.ID,
		Example: "{}", CreatedAt: e.now.Unix()}, rules.MatchAddress); err != nil {
		t.Fatal(err)
	}
	if got := learned(); len(got) != 1 || got[0].Source != "user" || got[0].RuleID != e.receipts.ID {
		t.Fatalf("user sender rule = %+v", got)
	}
	if row := e.process(sender, "order 9"); row.Decision.Stage != "sender" || row.Decision.RuleID != e.receipts.ID {
		t.Errorf("user sender rule not used: %+v", row.Decision)
	}
}

// A newsletter-type sender (bulk mail that passed DMARC) is learned from one confident
// decision, so its next email skips the model (PRD R15). Anything less keeps the rule of three.
func TestBulkSenderLearnedAfterOne(t *testing.T) {
	const (
		sender = "news@weekly.example"
		unsub  = "List-Unsubscribe: <mailto:unsubscribe@weekly.example>\r\n"
		pass   = "Authentication-Results: mx.example.test; dmarc=pass header.from=weekly.example\r\n"
		none   = "Authentication-Results: mx.example.test; dmarc=none header.from=weekly.example\r\n"
	)
	for name, tc := range map[string]struct {
		earlier    bool // the sender's previous email went to another rule
		headers    string
		confidence float64
		learned    bool
	}{
		"bulk that passed DMARC":         {headers: unsub + pass, confidence: 0.95, learned: true},
		"Precedence list counts as bulk": {headers: "Precedence: list\r\n" + pass, confidence: 0.95, learned: true},
		"bulk without a DMARC pass":      {headers: unsub + none, confidence: 0.95},
		"DMARC pass but not bulk":        {headers: pass, confidence: 0.95},
		"not confident enough to learn":  {headers: unsub + pass, confidence: 0.8},
		"an earlier decision disagrees":  {earlier: true, headers: unsub + pass, confidence: 0.95},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			if tc.earlier {
				e.primary.DecideFunc = answer(e.receipts.ID, 0.95)
				e.process(sender, "issue 1")
			}
			e.primary.DecideFunc = answer(e.food.ID, tc.confidence)
			e.processWith(sender, "issue 2", tc.headers)

			srs, err := e.st.SenderRules(t.Context(), 1)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(srs) == 1 && srs[0].Value == sender && srs[0].RuleID == e.food.ID && srs[0].Source == "learned"; got != tc.learned {
				t.Fatalf("learned = %v, want %v: %+v", got, tc.learned, srs)
			}
			calls := len(e.primary.Requests())
			row := e.processWith(sender, "issue 3", tc.headers)
			if skipped := len(e.primary.Requests()) == calls && row.Decision.Stage == "sender"; skipped != tc.learned {
				t.Errorf("next email skipped the model = %v, want %v (stage %s)", skipped, tc.learned, row.Decision.Stage)
			}
		})
	}
}

// MAI-45: a decision recorded in dry-run teaches the learner nothing, whichever way it was
// made. No sender rule is learned while dry-run is on, and once MailRules is live the
// dry-run decisions do not count either, so a dry-run period never makes a later email
// from that sender skip the model.
func TestDryRunLearnsNothing(t *testing.T) {
	const sender = "orders@swiggy.example"
	for name, decide := range map[string]func(e *env, subject string){
		"new mail": func(e *env, subject string) { e.process(sender, subject) },
		"cleanup Sort of a check's answers": func(e *env, subject string) {
			ref := e.deliver(sender, subject)
			if applied, err := e.p.SortSaved(e.t.Context(), ref, e.settle(ref)); err != nil || !applied {
				e.t.Fatalf("SortSaved = %v, %v", applied, err)
			}
		},
		"Sort of existing mail": func(e *env, subject string) {
			if err := e.p.Sort(e.t.Context(), e.deliver(sender, subject)); err != nil {
				e.t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			ctx := t.Context()
			e.withExecutor(true)
			e.primary.DecideFunc = answer(e.food.ID, 0.95) // confident enough to learn from
			learned := func() []rules.SenderRule {
				srs, err := e.st.SenderRules(ctx, 1)
				if err != nil {
					t.Fatal(err)
				}
				return srs
			}
			for i := range learn.After {
				decide(e, fmt.Sprintf("dry %d", i))
			}
			if got := learned(); len(got) != 0 {
				t.Fatalf("dry-run decisions learned a sender rule: %+v", got)
			}

			if err := e.st.SetDryRun(ctx, 1, false); err != nil {
				t.Fatal(err)
			}
			for i := range learn.After {
				if got := learned(); len(got) != 0 {
					t.Fatalf("learned after %d live decisions, counting the dry-run ones: %+v", i, got)
				}
				if row := e.process(sender, fmt.Sprintf("live %d", i)); row.Decision.Stage != "decider" || row.Actions[0].Status != store.ActionDone {
					t.Fatalf("live email %d: %+v, actions %+v", i, row.Decision, row.Actions)
				}
			}
			if got := learned(); len(got) != 1 || got[0].Source != "learned" || got[0].RuleID != e.food.ID {
				t.Fatalf("after %d live decisions, learned = %+v", learn.After, got)
			}
		})
	}
}

// While leave_own_mail is on, an email from the mailbox's own address is left alone: no
// sender rule, rule, model or action, no Needs review and no lesson. Each email is bulk mail
// that passed DMARC and the model is confident, so one decision would teach a sender rule.
func TestOwnMailLeftAlone(t *testing.T) {
	for name, tc := range map[string]struct {
		preset, username, from string
		off                    bool // leave_own_mail switched off
		blocked                bool // a user sender rule blocks the sender's domain
		alone                  bool
	}{
		"full address":                        {preset: "generic", username: "jane@example.test", from: "jane@example.test", alone: true},
		"different case":                      {preset: "generic", username: "Jane@Example.TEST", from: "JANE@example.test", alone: true},
		"iCloud local-part username":          {preset: "icloud", username: "jane", from: "jane@icloud.com", alone: true},
		"iCloud username, me.com sender":      {preset: "icloud", username: "jane@icloud.com", from: "Jane@me.com", alone: true},
		"iCloud local part, mac.com":          {preset: "icloud", username: "jane", from: "jane@mac.com", alone: true},
		"a sender rule is passed over":        {preset: "generic", username: "jane@example.test", from: "jane@example.test", blocked: true, alone: true},
		"setting off":                         {preset: "generic", username: "jane@example.test", from: "jane@example.test", off: true},
		"setting off, the sender rule":        {preset: "generic", username: "jane@example.test", from: "jane@example.test", off: true, blocked: true},
		"another sender":                      {preset: "generic", username: "jane@example.test", from: "orders@example.test"},
		"another iCloud name":                 {preset: "icloud", username: "jane", from: "john@icloud.com"},
		"local part, provider domain unknown": {preset: "generic", username: "jane", from: "jane@example.test"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			ctx := t.Context()
			e.p.Account.Preset, e.p.Account.Username = tc.preset, tc.username
			if tc.off {
				if err := e.st.SetSetting(ctx, 1, store.SettingLeaveOwnMail, "false"); err != nil {
					t.Fatal(err)
				}
			}
			_, domain, _ := strings.Cut(strings.ToLower(tc.from), "@")
			if tc.blocked {
				if _, err := e.st.PutSenderRule(ctx, 1, rules.SenderRule{UserID: e.user.ID, MatchType: rules.MatchDomain, Value: domain,
					Verdict: rules.VerdictBlock, Source: "user"}, e.now.Unix()); err != nil {
					t.Fatal(err)
				}
			}
			e.primary.DecideFunc = answer(e.food.ID, 0.95)
			row := e.processWith(tc.from, "my reply", "List-Unsubscribe: <mailto:u@"+domain+">\r\n"+
				"Authentication-Results: mx.example.test; dmarc=pass header.from="+domain+"\r\n")

			srs, err := e.st.SenderRules(ctx, 1)
			if err != nil {
				t.Fatal(err)
			}
			learned := slices.ContainsFunc(srs, func(sr rules.SenderRule) bool { return sr.Source == "learned" })
			asked := len(e.primary.Requests()) + len(e.fallback.Requests())
			d := row.Decision
			switch {
			case tc.alone:
				if d.Stage != "none" || d.Reason != ReasonOwnMail || d.RuleID != 0 || d.Model != "" {
					t.Errorf("decision = %+v, want stage none with %q", d, ReasonOwnMail)
				}
				if asked != 0 || len(e.exec.calls) != 0 || learned || row.Message.State != store.StateSkipped {
					t.Errorf("model calls %d, executor calls %d, learned %v, state %s", asked, len(e.exec.calls), learned, row.Message.State)
				}
				// Counted as an email left in the inbox, never as one decided without a model.
				tot, err := e.st.StatsTotals(ctx, e.user.Viewer(), 0)
				if err != nil {
					t.Fatal(err)
				}
				if tot.Processed != 1 || tot.WithoutModel != 0 || tot.WentReview != 0 || tot.WentNowhere != 1 {
					t.Errorf("stats = %+v", tot)
				}
			case tc.blocked:
				if d.Stage != "sender" || asked != 0 || !slices.Equal(e.exec.applied(), []string{"trash"}) {
					t.Errorf("decision = %+v, model calls %d, applied %v, want the sender rule's trash", d, asked, e.exec.applied())
				}
			default:
				if d.Stage != "decider" || asked != 1 || !slices.Equal(e.exec.applied(), []string{"move:Food"}) || !learned {
					t.Errorf("decision = %+v, model calls %d, applied %v, learned %v, want it sorted like any other", d, asked, e.exec.applied(), learned)
				}
			}
		})
	}
}

// oneBox is the executor's view of the test account.
type oneBox struct{ mb mail.Mailbox }

func (o oneBox) Mailbox(int64) (mail.Mailbox, error) { return o.mb, nil }
func (oneBox) Lock(int64) func()                     { return func() {} }

// withExecutor swaps the recording fake for the real executor, live or in dry-run.
func (e *env) withExecutor(dryRun bool) *actions.Exec {
	x := &actions.Exec{Store: e.st, Accounts: oneBox{e.mb}, Hub: e.p.Hub, DryRunDefault: dryRun, Now: func() time.Time { return e.now }}
	e.p.Exec = x
	return x
}

func TestLiveProcessingWithTheExecutor(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	x := e.withExecutor(false)
	e.primary.DecideFunc = answer(e.food.ID, 0.9)

	row := e.process("orders@swiggy.example", "order 1")
	if loc := row.Message.Location(); loc.Folder != "Food" || len(row.Actions) != 1 || row.Actions[0].Status != store.ActionDone ||
		row.Actions[0].DecisionID != row.Decision.ID || row.Actions[0].BatchID == 0 {
		t.Fatalf("after processing: at %+v, actions %+v", loc, row.Actions)
	}
	// Every action of the day shares one live batch; the next day gets its own.
	day1 := row.Actions[0].BatchID
	if b, _ := e.st.Batch(ctx, day1); b.Kind != store.BatchLive {
		t.Errorf("batch = %+v", b)
	}
	if again := e.process("orders@swiggy.example", "order 2"); again.Actions[0].BatchID != day1 {
		t.Errorf("second message of the day is in batch %d, want %d", again.Actions[0].BatchID, day1)
	}

	// "Undo today": the mail comes back to the inbox under a new UID, and the watcher
	// reports that UID. It is the same message, not new mail, and must not be sorted again.
	if u, err := x.UndoBatch(ctx, e.user.Viewer(), day1); u.Actions != 2 || err != nil {
		t.Fatalf("UndoBatch = %+v, %v", u, err)
	}
	back, _ := e.st.Message(ctx, row.Message.ID)
	if back.Location().Folder != "INBOX" || back.Location().UID == row.Message.UID {
		t.Fatalf("after undo the message is at %+v", back.Location())
	}
	calls := len(e.primary.Requests())
	if err := e.p.Process(ctx, back.Location()); err != nil {
		t.Fatal(err)
	}
	all, _ := e.st.Activity(ctx, e.user.Viewer(), store.ActivityFilter{})
	if after, _ := e.st.Message(ctx, row.Message.ID); len(all) != 2 || len(e.primary.Requests()) != calls || after.Location() != back.Location() {
		t.Errorf("undone mail was treated as new: %d messages, %d new model calls, now at %+v", len(all), len(e.primary.Requests())-calls, after.Location())
	}
	if f, _ := e.st.Folder(ctx, back.AccountID, "INBOX"); f.LastUID != back.Location().UID {
		t.Errorf("last_uid = %d, want %d", f.LastUID, back.Location().UID)
	}

	// Later mail the same day starts a new batch (the first is undone), and so does the next day.
	third := e.process("orders@swiggy.example", "order 3")
	e.now = e.now.Add(24 * time.Hour)
	fourth := e.process("orders@swiggy.example", "order 4")
	if a, b := third.Actions[0].BatchID, fourth.Actions[0].BatchID; a == day1 || b == a || b == day1 {
		t.Errorf("batches: day one %d, after its undo %d, next day %d", day1, a, b)
	}
}

// A sender rule counts a hit only for an email whose actions were carried out: in dry-run
// they are only recorded, and the hit is not counted, live or replayed from a cleanup
// check. A keep verdict is recorded as a keep action, so it follows the same rule.
func TestDryRunCountsNoSenderRuleHits(t *testing.T) {
	for _, tc := range []struct {
		name     string
		dry      bool
		verdict  string
		replay   bool
		wantHits int
	}{
		{"live, routed", false, rules.VerdictRoute, false, 1},
		{"dry-run, routed", true, rules.VerdictRoute, false, 0},
		{"dry-run, routed, from a cleanup check", true, rules.VerdictRoute, true, 0},
		{"live, routed, from a cleanup check", false, rules.VerdictRoute, true, 1},
		{"dry-run, kept in the inbox", true, rules.VerdictKeep, false, 0},
		{"live, kept in the inbox", false, rules.VerdictKeep, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			ctx := t.Context()
			e.withExecutor(tc.dry)
			sr := rules.SenderRule{UserID: e.user.ID, MatchType: rules.MatchDomain, Value: "swiggy.example", Verdict: tc.verdict, Source: "user"}
			if tc.verdict == rules.VerdictRoute {
				sr.RuleID = e.food.ID
			}
			if _, err := e.st.PutSenderRule(ctx, 1, sr, 1); err != nil {
				t.Fatal(err)
			}
			var row store.ActivityRow
			if tc.replay {
				ref := e.deliver("orders@swiggy.example", "order 1")
				if applied, err := e.p.SortSaved(ctx, ref, e.settle(ref)); err != nil || !applied {
					t.Fatalf("SortSaved = %v, %v", applied, err)
				}
				row = e.row(ref)
			} else {
				row = e.process("orders@swiggy.example", "order 1")
			}
			if row.Decision.Stage != "sender" || row.Message.State != store.StateActed && tc.verdict == rules.VerdictRoute {
				t.Fatalf("decision %+v, state %s", row.Decision, row.Message.State)
			}
			if srs, _ := e.st.SenderRules(ctx, 1); len(srs) != 1 || srs[0].Hits != tc.wantHits {
				t.Errorf("sender rules = %+v, want %d hits", srs, tc.wantHits)
			}
		})
	}
}

func TestReviewAndRetryWithTheExecutor(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.withExecutor(false)
	e.addCutOff() // even with a default rule to fall back on
	e.fallback.DecideFunc = answer(e.food.ID, 0.5)
	e.primary.DecideFunc = answer(e.food.ID, 0.5)

	// Live, below the threshold: Needs review, and the mailbox is not touched at all.
	for _, dry := range []bool{false, true} {
		if err := e.st.SetDryRun(ctx, 1, dry); err != nil {
			t.Fatal(err)
		}
		row := e.process("orders@swiggy.example", fmt.Sprintf("unsure dry-run %v", dry))
		flags, _ := e.mb.Flags(ctx, row.Message.Location())
		if row.Message.State != store.StateReview || row.Message.Location().Folder != "INBOX" || len(flags) != 0 || len(row.Actions) != 0 {
			t.Errorf("review, dry-run %v: state %s in %s, flags %v, actions %+v", dry, row.Message.State, row.Message.Location().Folder, flags, row.Actions)
		}
	}
	// In dry-run a rule's actions are recorded, not run.
	e.primary.DecideFunc = answer(e.food.ID, 0.9)
	row := e.process("orders@swiggy.example", "order 3")
	if row.Message.State != store.StateActed || row.Message.Location().Folder != "INBOX" || row.Actions[0].Status != store.ActionDryRun || row.Actions[0].Folder != "Food" {
		t.Errorf("dry-run: %+v, actions %+v", row.Message, row.Actions)
	}

	// Live again, on a server that cannot move: the action fails, is recorded as failed
	// and is not retried.
	if err := e.st.SetDryRun(ctx, 1, false); err != nil {
		t.Fatal(err)
	}
	e.mb.Caps = mail.Caps{}
	row = e.process("orders@swiggy.example", "order 4")
	if m := row.Message; m.State != store.StateError || m.NextAttemptAt != 0 || row.Actions[0].Status != store.ActionFailed ||
		!strings.Contains(row.Actions[0].Error, "unsupported capability") {
		t.Errorf("unsupported move: %+v, actions %+v", m, row.Actions)
	}
}

// A rule may name its own model: an email is decided by the model of its highest-priority
// candidate that names one, and by the default when that model cannot be used.
func TestRuleModelOverride(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.receipts.Model = "clef:clef-flash"
	if err := e.st.UpdateRule(ctx, 1, e.receipts, 2); err != nil {
		t.Fatal(err)
	}
	own := &models.Fake{NameValue: "clef", DecideFunc: func(models.DecideRequest) (models.Decision, models.Usage, error) {
		return models.Decision{RuleID: e.receipts.ID, Confidence: 0.9}, models.Usage{Provider: "cloudflare", Model: "clef-flash", TokensIn: 50, CostUSD: 0.0002}, nil
	}}
	var asked []string
	usable := true
	e.p.Override = func(_ context.Context, _ int64, spec string) *models.Router {
		asked = append(asked, spec)
		if !usable {
			return nil
		}
		return &models.Router{Primary: own, Usage: store.Ledger{Store: e.st, TenantID: 1}}
	}
	e.primary.DecideFunc = answer(e.food.ID, 0.9)

	row := e.process("billing@vendor.example", "invoice 1")
	if d := row.Decision; d.RuleID != e.receipts.ID || d.Model != "clef-flash" || d.Stage != "decider" || len(e.primary.Requests()) != 0 || !slices.Equal(asked, []string{"clef:clef-flash"}) {
		t.Fatalf("decision = %+v; the default was asked %d times; override asked for %v", d, len(e.primary.Requests()), asked)
	}
	// Both candidates were put to the rule's model, not only the rule that named it.
	if req := own.Requests()[0]; len(req.Candidates) != 2 {
		t.Errorf("candidates = %+v", req.Candidates)
	}
	// Mail that the rule is no candidate for never reaches its model.
	e.process("hello@news.example", "weekly issue")
	if len(own.Requests()) != 1 {
		t.Errorf("the rule's model was asked about mail a condition settled")
	}
	// The rule's model cannot be used (its key was removed): the default decides.
	usable = false
	if d := e.process("billing@vendor.example", "invoice 2").Decision; d.RuleID != e.food.ID || d.Model != "primary-1" {
		t.Errorf("fallback to the default: %+v", d)
	}
	// With no default model either, the rule's own model alone is enough.
	usable, e.p.Router = true, nil
	if d := e.process("billing@vendor.example", "invoice 3").Decision; d.RuleID != e.receipts.ID || d.Model != "clef-flash" {
		t.Errorf("only the rule's model is set: %+v", d)
	}
}

// When the decision model is unsure, the fallback is shown the user's corrections, the
// ones most like this email first, and what the decision model thought of each candidate
// is kept with the decision.
func TestFallbackSeesCorrectionsAndTheSpreadIsKept(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.primary.DecideFunc = answer(e.food.ID, 0.95)
	corrected := func(from, subject string, right int64) {
		row := e.process(from, subject)
		ex, _ := json.Marshal(message.Summary{From: from, FromDomain: from[strings.IndexByte(from, '@')+1:], Subject: subject})
		e.now = e.now.Add(time.Minute)
		if _, err := e.st.AddCorrection(ctx, 1, e.user.ID, store.Correction{MessageID: row.Message.ID, WrongRuleID: e.food.ID,
			RightRuleID: right, Example: string(ex), CreatedAt: e.now.Unix()}, ""); err != nil {
			t.Fatal(err)
		}
	}
	corrected("someone@elsewhere.example", "unrelated", e.receipts.ID)
	corrected("billing@vendor.example", "same domain", e.receipts.ID)
	corrected("orders@vendor.example", "same sender", 0) // the user kept it in the inbox
	corrected("other@elsewhere.example", "newest unrelated", e.food.ID)

	e.primary.DecideFunc = answer(e.food.ID, 0.5) // unsure: escalate
	e.primary.Spread = map[int64]float64{e.food.ID: 0.5, e.receipts.ID: 0.3, 0: 0.2}
	e.fallback.DecideFunc = func(models.DecideRequest) (models.Decision, models.Usage, error) {
		return models.Decision{RuleID: e.receipts.ID, Confidence: 0.9}, models.Usage{Provider: "fake", Model: "fallback-1"}, nil
	}
	row := e.process("orders@vendor.example", "invoice 9")
	var shown []string
	for _, ex := range e.fallback.Requests()[0].Examples {
		shown = append(shown, fmt.Sprint(ex.Email.Subject, "→", ex.RightRuleID))
	}
	want := []string{"same sender→0", fmt.Sprint("same domain→", e.receipts.ID), fmt.Sprint("newest unrelated→", e.food.ID), fmt.Sprint("unrelated→", e.receipts.ID)}
	if !slices.Equal(shown, want) {
		t.Errorf("examples shown to the fallback = %v, want %v", shown, want)
	}
	if last := e.primary.Requests()[len(e.primary.Requests())-1]; len(last.Examples) != 0 {
		t.Errorf("the decision model was shown %d examples; they are for the fallback only", len(last.Examples))
	}
	if d := row.Decision; d.Stage != "fallback" || d.RuleID != e.receipts.ID || len(d.Probabilities) != 3 || d.Probabilities[e.food.ID] != 0.5 || d.Probabilities[0] != 0.2 {
		t.Errorf("decision = %+v", d)
	}
	// A decision no model spread came with stores none.
	e.primary.Spread = nil
	if d := e.process("hello@news.example", "weekly issue").Decision; d.Probabilities != nil {
		t.Errorf("a condition's decision has probabilities: %+v", d)
	}
}

// Cleanup sorts mail that is already there through the same steps as new mail: into its own
// batch, again even if it was seen before, and without moving the folder's watch position.
func TestSortExistingMail(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.primary.DecideFunc = answer(e.food.ID, 0.95)
	ref := e.deliver("orders@swiggy.example", "order 1")
	batch, err := e.st.CreateCleanupBatch(ctx, e.p.Account.ID, "INBOX", 0, 0, 0, 1, e.now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	p := *e.p
	p.Batch = batch.ID
	var tokens int
	var cost float64
	p.Spent = func(t int, c float64) { tokens, cost = tokens+t, cost+c }
	x := e.withExecutor(true) // dry-run first
	p.Exec = x
	if err := p.Sort(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if row := e.row(ref); len(row.Actions) != 1 || row.Actions[0].Status != store.ActionDryRun || row.Actions[0].BatchID != batch.ID || tokens != 105 || cost != 0.001 {
		t.Fatalf("dry-run sort: %+v, %d tokens, %v USD", row.Actions, tokens, cost)
	}
	if _, err := e.st.Folder(ctx, e.p.Account.ID, "INBOX"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("sorting existing mail moved the watch position: %v", err)
	}
	// Live: the same email is decided afresh and moved, though it was seen before.
	if err := e.st.SetDryRun(ctx, 1, false); err != nil {
		t.Fatal(err)
	}
	if err := p.Sort(ctx, ref); err != nil {
		t.Fatal(err)
	}
	row := e.row(ref)
	if len(row.Actions) != 2 || row.Actions[1].Status != store.ActionDone || row.Actions[1].BatchID != batch.ID || row.Message.Location().Folder != "Food" {
		t.Fatalf("live sort: %+v in %s", row.Actions, row.Message.Location().Folder)
	}
	if ds, _ := e.st.MessageDecisions(ctx, row.Message.ID); len(ds) != 2 {
		t.Errorf("%d decisions, want one per run", len(ds))
	}
	// New mail still goes to the day's live batch.
	if live := e.process("orders@swiggy.example", "order 2"); live.Actions[0].BatchID == batch.ID {
		t.Error("new mail landed in the cleanup batch")
	}

	// A sender rule that settles an email counts the hit.
	sr, err := e.st.PutSenderRule(ctx, 1, rules.SenderRule{UserID: e.user.ID, MatchType: rules.MatchDomain, Value: "swiggy.example", Verdict: rules.VerdictKeep, Source: "user"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	e.process("orders@swiggy.example", "order 3")
	e.process("offers@mail.swiggy.example", "order 4")
	e.process("hello@news.example", "weekly issue")
	if srs, _ := e.st.SenderRules(ctx, 1); len(srs) != 1 || srs[0].ID != sr.ID || srs[0].Hits != 2 {
		t.Errorf("sender rules = %+v, want 2 hits", srs)
	}
}

// SortSaved replays an answer a cleanup check already settled: it records the same decision
// as the live/direct path, without asking the model again and without booking its cost a
// second time, and it passes over a reference that is gone.
func TestSortSavedReplaysTheCheck(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.primary.DecideFunc = answer(e.food.ID, 0.93)

	// The direct path: Pipeline.Sort decides and records as live processing does.
	refA := e.deliver("orders@swiggy.example", "order A")
	if err := e.p.Sort(ctx, refA); err != nil {
		t.Fatal(err)
	}
	d1 := e.row(refA).Decision

	// The check path: Decider.Settle settles the answer (one model call), then SortSaved
	// replays it with no further call.
	refB := e.deliver("orders@swiggy.example", "order B")
	out := e.settle(refB)
	callsBefore := len(e.primary.Requests())
	applied, err := e.p.SortSaved(ctx, refB, out)
	if err != nil || !applied {
		t.Fatalf("SortSaved = %v, %v", applied, err)
	}
	if n := len(e.primary.Requests()); n != callsBefore {
		t.Errorf("SortSaved asked the model %d times", n-callsBefore)
	}
	d2 := e.row(refB).Decision

	// The two recorded decisions agree on what was decided, by whom, and that a model decided it.
	if d1.Stage != "decider" || d1.Stage != d2.Stage || d1.RuleID != d2.RuleID || d1.Confidence != d2.Confidence ||
		d1.Model != d2.Model || d1.RuleName != d2.RuleName {
		t.Errorf("decisions differ: direct %+v, replayed %+v", d1, d2)
	}
	if got := e.exec.applied(); !slices.Equal(got, []string{"move:Food", "move:Food"}) {
		t.Errorf("applied %v, want two move:Food", got)
	}
	// The replayed decision carries the stage, rule, model, confidence and reason, but no cost.
	if d2.Model == "" || d2.Confidence != 0.93 || d2.Reason == "" || d2.TokensIn != 0 || d2.TokensOut != 0 || d2.CostUSD != 0 {
		t.Errorf("replayed decision = %+v", d2)
	}
	// The direct decision booked the model cost once.
	if d1.CostUSD == 0 {
		t.Errorf("the direct decision recorded no cost: %+v", d1)
	}

	// A reference that is gone since the check is passed over, not acted on.
	e.p.Mailbox = &flaky{Mailbox: e.mb, fetchErr: mail.ErrNotFound}
	if applied, err := e.p.SortSaved(ctx, refB, out); err != nil || applied {
		t.Errorf("SortSaved of a gone reference = %v, %v", applied, err)
	}
}

// A manual run of only some rules (MAI-43) hands Settle just those rules: the others are not
// walked and the model is not offered them, while sender rules still apply, a route among
// them reaching a rule that is not walked through Decider.RouteOnly.
func TestSettleWithOnlySomeRules(t *testing.T) {
	ctx := t.Context()
	routeToReceipts := func(e *env) []rules.SenderRule {
		return []rules.SenderRule{{UserID: e.user.ID, MatchType: rules.MatchAddress, Value: "orders@swiggy.example", RuleID: e.receipts.ID, Verdict: rules.VerdictRoute}}
	}
	block := rules.SenderRule{MatchType: rules.MatchDomain, Value: "junk.example", Verdict: rules.VerdictBlock}
	tests := []struct {
		name      string
		from      string
		walk      func(e *env) []rules.Rule
		routeOnly func(e *env) []rules.Rule
		senders   func(e *env) []rules.SenderRule
		wantStage rules.Stage
		wantRule  string
		wantAsked []string // the rules the model was offered, in order; nil = not asked
	}{
		{name: "the model is offered only the picked rules", from: "hello@bank.example",
			walk:      func(e *env) []rules.Rule { return []rules.Rule{e.food} },
			wantStage: rules.StageDecider, wantRule: "Food", wantAsked: []string{"Food"}},
		{name: "an unpicked condition rule above does not take the email", from: "weekly@news.example",
			walk:      func(e *env) []rules.Rule { return []rules.Rule{e.receipts} },
			wantStage: rules.StageDecider, wantRule: "Receipts", wantAsked: []string{"Receipts"}},
		{name: "the picked rules decide in their usual order", from: "hello@bank.example",
			walk:      func(e *env) []rules.Rule { return []rules.Rule{e.food, e.receipts} },
			wantStage: rules.StageDecider, wantRule: "Food", wantAsked: []string{"Food", "Receipts"}},
		{name: "a sender block applies whatever is picked", from: "spam@junk.example",
			walk:      func(e *env) []rules.Rule { return []rules.Rule{e.food} },
			senders:   func(*env) []rules.SenderRule { return []rules.SenderRule{block} },
			wantStage: rules.StageSender},
		{name: "a sender route to a rule that is not picked still routes, with no model call", from: "orders@swiggy.example",
			walk:      func(e *env) []rules.Rule { return []rules.Rule{e.food} },
			routeOnly: func(e *env) []rules.Rule { return []rules.Rule{e.receipts} },
			senders:   routeToReceipts,
			wantStage: rules.StageSender, wantRule: "Receipts"},
		{name: "without RouteOnly the same route falls through to the picked rules", from: "orders@swiggy.example",
			walk:      func(e *env) []rules.Rule { return []rules.Rule{e.food} },
			senders:   routeToReceipts,
			wantStage: rules.StageDecider, wantRule: "Food", wantAsked: []string{"Food"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.primary.DecideFunc = func(req models.DecideRequest) (models.Decision, models.Usage, error) {
				return models.Decision{RuleID: req.Candidates[0].RuleID, Confidence: 0.95}, models.Usage{Provider: "fake", Model: "primary-1", TokensIn: 100, TokensOut: 5}, nil
			}
			ref := e.deliver(tc.from, "an email")
			raw, err := e.mb.Fetch(ctx, ref, 0)
			if err != nil {
				t.Fatal(err)
			}
			sum, err := message.Parse(raw, e.p.Account.ID, 2000)
			if err != nil {
				t.Fatal(err)
			}
			d := Decider{Router: e.p.Router, MinConfidence: 0.75, Now: e.now}
			if tc.routeOnly != nil {
				d.RouteOnly = tc.routeOnly(e)
			}
			var senders []rules.SenderRule
			if tc.senders != nil {
				senders = tc.senders(e)
			}
			out, err := d.Settle(ctx, *sum, tc.walk(e), senders)
			if err != nil {
				t.Fatal(err)
			}
			if out.Stage != tc.wantStage || out.RuleName != tc.wantRule {
				t.Errorf("settled as %s by %q, want %s by %q (%s)", out.Stage, out.RuleName, tc.wantStage, tc.wantRule, out.Reason)
			}
			var asked []string
			for _, req := range e.primary.Requests() {
				for _, c := range req.Candidates {
					asked = append(asked, c.Name)
				}
			}
			if !slices.Equal(asked, tc.wantAsked) {
				t.Errorf("the model was offered %v, want %v", asked, tc.wantAsked)
			}
		})
	}
}
