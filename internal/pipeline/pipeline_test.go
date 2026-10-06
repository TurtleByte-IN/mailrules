package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/actions"
	"github.com/TurtleByte-IN/mailrules/internal/events"
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
	err   error // returned by Apply for rule actions (not for the review tag)
}

type call struct {
	rec  actions.DecisionRecord
	acts []rules.Action
}

func (r *recorder) Apply(_ context.Context, d actions.DecisionRecord, acts []rules.Action, _ int64) ([]actions.ActionRecord, error) {
	r.calls = append(r.calls, call{d, acts})
	if acts[0].Type == actions.KindReview {
		return nil, nil
	}
	return nil, r.err
}

// applied lists the rule actions applied so far, e.g. "move:Food", leaving out review tags.
func (r *recorder) applied() []string {
	var out []string
	for _, c := range r.calls {
		for _, a := range c.acts {
			if a.Type != actions.KindReview {
				out = append(out, a.String())
			}
		}
	}
	return out
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
		r, err := e.st.CreateRule(ctx, rules.Rule{UserID: e.user.ID, Name: name, Intent: intent, Conditions: cond,
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
		Router:        &models.Router{Primary: e.primary, Fallback: e.fallback, EscalateBelow: 0.75, Usage: e.st, Now: func() time.Time { return e.now }},
		MinConfidence: 0.75, BodyChars: 2000, Now: func() time.Time { return e.now },
	}
	return e
}

func (e *env) deliver(from, subject string) mail.MsgRef {
	return e.mb.Deliver("INBOX", "From: "+from+"\r\nTo: me@example.test\r\nSubject: "+subject+
		"\r\nMessage-ID: <"+strings.ReplaceAll(subject, " ", "-")+"@example.test>\r\n\r\nThe body of "+subject+".\r\n")
}

// process delivers one email, runs it through the pipeline and returns its activity row.
func (e *env) process(from, subject string) store.ActivityRow {
	e.t.Helper()
	ref := e.deliver(from, subject)
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
	row, err := e.st.ActivityFor(e.t.Context(), m.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	if row.Decision == nil {
		row.Decision = &store.Decision{}
	}
	return row
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
				_, err := e.st.PutSenderRule(e.t.Context(), rules.SenderRule{UserID: e.user.ID, MatchType: rules.MatchAddress,
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
				_, err := e.st.PutSenderRule(e.t.Context(), rules.SenderRule{UserID: e.user.ID, MatchType: rules.MatchDomain,
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
			// Review tags go through the executor like every other mailbox change.
			tagged := slices.ContainsFunc(e.exec.calls, func(c call) bool { return c.acts[0].Type == actions.KindReview })
			if tagged != (tt.wantState == store.StateReview) {
				t.Errorf("review tag applied = %v in state %s", tagged, m.State)
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
	boom := errors.New("HTTP 503")
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
	if d := final.Decision; d.Stage != "none" || !strings.Contains(d.Reason, "Gave up after 6 retries") || !strings.Contains(d.Reason, "HTTP 503") {
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
		srs, err := e.st.SenderRules(ctx, e.user.ID)
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
	if _, err := e.st.AddCorrection(ctx, e.user.ID, store.Correction{MessageID: row.Message.ID, WrongRuleID: e.food.ID,
		RightRuleID: e.receipts.ID, Example: "{}", CreatedAt: e.now.Unix()}, false); err != nil {
		t.Fatal(err)
	}
	if got := learned(); len(got) != 0 {
		t.Fatalf("learned rule survived a correction: %+v", got)
	}
	if row := e.process(sender, "order 8"); row.Decision.Stage != "decider" || len(learned()) != 0 {
		t.Errorf("after the correction: %+v, learned %+v", row.Decision, learned())
	}

	// "Always for this sender" stores a user rule instead, which learning never replaces.
	if _, err := e.st.AddCorrection(ctx, e.user.ID, store.Correction{MessageID: row.Message.ID, RightRuleID: e.receipts.ID,
		Example: "{}", CreatedAt: e.now.Unix()}, true); err != nil {
		t.Fatal(err)
	}
	if got := learned(); len(got) != 1 || got[0].Source != "user" || got[0].RuleID != e.receipts.ID {
		t.Fatalf("user sender rule = %+v", got)
	}
	if row := e.process(sender, "order 9"); row.Decision.Stage != "sender" || row.Decision.RuleID != e.receipts.ID {
		t.Errorf("user sender rule not used: %+v", row.Decision)
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
	if n, err := x.UndoBatch(ctx, day1); n != 2 || err != nil {
		t.Fatalf("UndoBatch = %d, %v", n, err)
	}
	back, _ := e.st.Message(ctx, row.Message.ID)
	if back.Location().Folder != "INBOX" || back.Location().UID == row.Message.UID {
		t.Fatalf("after undo the message is at %+v", back.Location())
	}
	calls := len(e.primary.Requests())
	if err := e.p.Process(ctx, back.Location()); err != nil {
		t.Fatal(err)
	}
	all, _ := e.st.Activity(ctx, store.ActivityFilter{})
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

func TestReviewTagAndRetryWithTheExecutor(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.withExecutor(false)
	e.fallback.DecideFunc = answer(e.food.ID, 0.5)
	e.primary.DecideFunc = answer(e.food.ID, 0.5)

	// Live: a message in review gets the keyword, as an action that can be undone.
	row := e.process("orders@swiggy.example", "order 1")
	flags, _ := e.mb.Flags(ctx, row.Message.Location())
	if row.Message.State != store.StateReview || !slices.Equal(flags, []string{actions.ReviewKeyword}) ||
		len(row.Actions) != 1 || row.Actions[0].Kind != actions.KindReview || row.Actions[0].Status != store.ActionDone {
		t.Errorf("review live: state %s, flags %v, actions %+v", row.Message.State, flags, row.Actions)
	}

	// Dry-run: never.
	if err := e.st.SetDryRun(ctx, true); err != nil {
		t.Fatal(err)
	}
	row = e.process("orders@swiggy.example", "order 2")
	flags, _ = e.mb.Flags(ctx, row.Message.Location())
	if row.Message.State != store.StateReview || len(flags) != 0 || row.Actions[0].Status != store.ActionDryRun {
		t.Errorf("review in dry-run: state %s, flags %v, actions %+v", row.Message.State, flags, row.Actions)
	}
	// And a rule's actions are recorded, not run.
	e.primary.DecideFunc = answer(e.food.ID, 0.9)
	row = e.process("orders@swiggy.example", "order 3")
	if row.Message.State != store.StateActed || row.Message.Location().Folder != "INBOX" || row.Actions[0].Status != store.ActionDryRun || row.Actions[0].Folder != "Food" {
		t.Errorf("dry-run: %+v, actions %+v", row.Message, row.Actions)
	}

	// Live again, on a server that cannot move: the action fails, is recorded as failed
	// and is not retried.
	if err := e.st.SetDryRun(ctx, false); err != nil {
		t.Fatal(err)
	}
	e.mb.Caps = mail.Caps{}
	row = e.process("orders@swiggy.example", "order 4")
	if m := row.Message; m.State != store.StateError || m.NextAttemptAt != 0 || row.Actions[0].Status != store.ActionFailed ||
		!strings.Contains(row.Actions[0].Error, "unsupported capability") {
		t.Errorf("unsupported move: %+v, actions %+v", m, row.Actions)
	}
}
