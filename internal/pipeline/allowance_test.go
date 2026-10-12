package pipeline

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/message"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// withAllowance gives the tenant an allowance of limit emails a month, of which used are
// spent in the month of e.now, and says which of the env's models (primary, fallback) run
// on the operator's keys.
func (e *env) withAllowance(limit, used int, operator ...string) {
	e.t.Helper()
	if used > 0 {
		if err := e.st.SpendAIEmails(e.t.Context(), 1, e.now, used); err != nil {
			e.t.Fatal(err)
		}
	}
	e.p.Router.Metered = func(provider string) bool {
		for _, o := range operator {
			if o == provider {
				return true
			}
		}
		return false
	}
	e.p.Allowance = func(ctx context.Context, tenantID int64, now time.Time) (bool, error) {
		n, err := e.st.AIEmails(ctx, tenantID, store.Month(now))
		return n >= limit, err
	}
}

func (e *env) aiEmails(at time.Time) int {
	e.t.Helper()
	n, err := e.st.AIEmails(e.t.Context(), 1, store.Month(at))
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestAllowance(t *testing.T) {
	tests := []struct {
		name      string
		from      string
		setup     func(e *env)
		wantState string
		wantApply []string
		wantCalls int    // model calls, primary and fallback together
		wantUsed  int    // the month's count afterwards
		wantWhy   string // the decision's reason, when it matters
	}{
		{
			name: "below the allowance a model on the operator's keys decides, and the email counts", from: "orders@swiggy.example",
			setup:     func(e *env) { e.withAllowance(3, 2, "primary", "fallback") },
			wantState: store.StateActed, wantApply: []string{"move:Food"}, wantCalls: 1, wantUsed: 3,
		},
		{
			name: "at the allowance, mail that needs a model waits in Needs review", from: "orders@swiggy.example",
			setup:     func(e *env) { e.withAllowance(3, 3, "primary", "fallback") },
			wantState: store.StateReview, wantCalls: 0, wantUsed: 3, wantWhy: ReasonAllowance,
		},
		{
			name: "at the allowance, a condition rule still acts", from: "weekly@news.example",
			setup:     func(e *env) { e.withAllowance(3, 3, "primary", "fallback") },
			wantState: store.StateActed, wantApply: []string{"move:Reading"}, wantUsed: 3,
		},
		{
			name: "at the allowance, a learned sender still sorts", from: "orders@swiggy.example",
			setup: func(e *env) {
				e.withAllowance(3, 3, "primary", "fallback")
				if _, err := e.st.PutSenderRule(e.t.Context(), 1, rules.SenderRule{UserID: e.user.ID, MatchType: rules.MatchAddress,
					Value: "orders@swiggy.example", RuleID: e.food.ID, Verdict: rules.VerdictRoute, Source: "learned"}, 1); err != nil {
					e.t.Fatal(err)
				}
			},
			wantState: store.StateActed, wantApply: []string{"move:Food"}, wantUsed: 3,
		},
		{
			name: "a tenant on its own keys is neither counted nor stopped", from: "orders@swiggy.example",
			setup:     func(e *env) { e.withAllowance(3, 3) },
			wantState: store.StateActed, wantApply: []string{"move:Food"}, wantCalls: 1, wantUsed: 3,
		},
		{
			name: "at the allowance, its own decision model decides without the operator's fallback", from: "orders@swiggy.example",
			setup: func(e *env) {
				e.withAllowance(3, 3, "fallback")
				e.primary.DecideFunc = answer(e.food.ID, 0.8) // unsure enough to escalate, were the fallback asked
				e.p.Router.EscalateBelow = 0.9
			},
			wantState: store.StateActed, wantApply: []string{"move:Food"}, wantCalls: 1, wantUsed: 3,
		},
		{
			name: "a new month starts the count again", from: "orders@swiggy.example",
			setup: func(e *env) {
				e.withAllowance(3, 3, "primary", "fallback")
				e.now = time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC) // e.now was in January 2027
			},
			wantState: store.StateActed, wantApply: []string{"move:Food"}, wantCalls: 1, wantUsed: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.primary.DecideFunc = answer(e.food.ID, 0.93)
			tt.setup(e)
			row := e.process(tt.from, "order 1")
			if row.Message.State != tt.wantState {
				t.Errorf("state = %s, want %s (reason %q)", row.Message.State, tt.wantState, row.Decision.Reason)
			}
			if got := e.exec.applied(); !slices.Equal(got, tt.wantApply) {
				t.Errorf("applied = %v, want %v", got, tt.wantApply)
			}
			if calls := len(e.primary.Requests()) + len(e.fallback.Requests()); calls != tt.wantCalls {
				t.Errorf("model calls = %d, want %d", calls, tt.wantCalls)
			}
			if got := e.aiEmails(e.now); got != tt.wantUsed {
				t.Errorf("AI-sorted emails this month = %d, want %d", got, tt.wantUsed)
			}
			if tt.wantWhy != "" && row.Decision.Reason != tt.wantWhy {
				t.Errorf("reason = %q, want %q", row.Decision.Reason, tt.wantWhy)
			}
		})
	}
}

// A message whose actions failed is tried again later: it is counted once, and decided
// again even though its own count used up the allowance meanwhile.
func TestAllowanceCountsARetriedEmailOnce(t *testing.T) {
	e := newEnv(t)
	e.primary.DecideFunc = answer(e.food.ID, 0.93)
	e.withAllowance(1, 0, "primary", "fallback")
	e.exec.err = errors.New("the server dropped the connection")
	row := e.process("orders@swiggy.example", "order 1")
	if row.Message.State != store.StateError || e.aiEmails(e.now) != 1 {
		t.Fatalf("first try: state %s, %d counted; want error, 1", row.Message.State, e.aiEmails(e.now))
	}
	e.exec.err = nil
	e.now = e.now.Add(RetryEvery)
	if n, err := e.p.RetryDue(t.Context()); err != nil || n != 1 {
		t.Fatalf("RetryDue = %d, %v", n, err)
	}
	if got := e.exec.applied(); !slices.Equal(got, []string{"move:Food", "move:Food"}) {
		t.Errorf("applied = %v, want the move tried twice", got)
	}
	if len(e.primary.Requests()) != 2 || e.aiEmails(e.now) != 1 {
		t.Errorf("model asked %d times, %d counted; want 2 and 1", len(e.primary.Requests()), e.aiEmails(e.now))
	}
}

// A cleanup check counts each email it asks a model about on the operator's keys as it
// goes (Decider.Spend), and SortSaved, which applies the check's answer, counts nothing
// more.
func TestAllowanceCountsACleanupCheckOnce(t *testing.T) {
	e := newEnv(t)
	e.primary.DecideFunc = answer(e.food.ID, 0.93)
	e.withAllowance(5, 0, "primary", "fallback")
	ctx := t.Context()
	ref := e.deliver("orders@swiggy.example", "order 1")
	rs, err := e.st.Rules(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := e.mb.Fetch(ctx, ref, 0)
	sum, err := message.Parse(raw, e.p.Account.ID, 2000)
	if err != nil {
		t.Fatal(err)
	}
	d := Decider{Router: e.p.Router, MinConfidence: 0.75, Now: e.now,
		Spend: func(ctx context.Context) error { return e.st.SpendAIEmails(ctx, 1, e.now, 1) }}
	out, err := d.Settle(ctx, *sum, rs, nil)
	if err != nil || !out.Metered {
		t.Fatalf("settle = %+v, %v", out, err)
	}
	if applied, err := e.p.SortSaved(ctx, ref, out); err != nil || !applied {
		t.Fatalf("SortSaved = %v, %v", applied, err)
	}
	if got := e.aiEmails(e.now); got != 1 {
		t.Errorf("AI-sorted emails = %d, want 1", got)
	}
}
