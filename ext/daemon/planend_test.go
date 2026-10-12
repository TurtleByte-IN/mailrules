package daemon

import (
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// In a build that sells plans, a hosted tenant's mail stops being sorted, and its
// mailboxes stop starting, the moment its trial runs out with no plan bought, and again
// once one is; the self-host tenant has no plan. A build that sells none has no gate.
func TestPlanEndedGate(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	st := store.New(db)
	if planEnded(st, []ext.Module{{Name: "free"}}) != nil {
		t.Fatal("a build that sells no plans has a gate")
	}
	const created = 1_800_000_000
	u, err := st.SignInIdentity(ctx, store.Identity{Provider: "p", Subject: "s", Email: "a@example.test", Tenant: "o"}, created)
	if err != nil {
		t.Fatal(err)
	}
	ended := planEnded(st, []ext.Module{{Name: "billing", Billing: &ext.Billing{}}})
	if ended == nil {
		t.Fatal("a build that sells plans has no gate")
	}
	trialEnd := time.Unix(created+store.TrialDays*24*3600, 0)
	for _, c := range []struct {
		name   string
		tenant int64
		at     time.Time
		buy    bool
		want   bool
	}{
		{"in the trial", u.TenantID, trialEnd.Add(-time.Second), false, false},
		{"the trial is over", u.TenantID, trialEnd, false, true},
		{"the self-host tenant, long after", store.SelfHostTenant, trialEnd.AddDate(1, 0, 0), false, false},
		{"a plan bought", u.TenantID, trialEnd.Add(time.Hour), true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.buy {
				if _, _, err := st.SetPlan(ctx, c.tenant, store.Plan{Plan: store.PlanPersonal, Interval: store.IntervalMonth,
					Status: store.StatusActive}, c.at.Unix()); err != nil {
					t.Fatal(err)
				}
			}
			got, err := ended(ctx, c.tenant, c.at)
			if err != nil || got != c.want {
				t.Errorf("ended = %v, %v; want %v", got, err, c.want)
			}
		})
	}
}
