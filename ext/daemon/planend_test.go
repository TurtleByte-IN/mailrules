package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/config"
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
	u, err := st.SignInIdentity(ctx, store.Identity{Provider: "p", Subject: "s", Email: "a@example.test", Tenant: "o"}, created, nil)
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

// A build that sells plans must be able to warn people before it deletes the data of a plan
// that ended, so it does not start without a mail server, and says which settings to add.
// A build that sells none starts without one, as before.
func TestPlansNeedAMailServer(t *testing.T) {
	billing := ext.Module{Name: "billing",
		Routes: func(ext.Host) []ext.Route {
			return []ext.Route{{Method: http.MethodGet, Path: "/api/billing/checkout.js", Public: true, Handler: func(http.ResponseWriter, *http.Request) {}}}
		},
		Billing: &ext.Billing{
			Plans:      map[string]ext.PlanTerms{store.PlanTrial: {Name: "Trial"}, store.PlanPersonal: {Name: "Personal"}, store.PlanPro: {Name: "Pro"}},
			Checkout:   func(context.Context, ext.CheckoutRequest) (json.RawMessage, error) { return nil, nil },
			ChangePlan: func(context.Context, ext.PlanChange) error { return nil },
			Portal:     func(context.Context, int64, string) (string, error) { return "", nil },
			Script:     "/api/billing/checkout.js",
		}}
	dir := t.TempDir()
	cfg := loadCfg(t, dir, map[string]string{"MAILRULES_LISTEN": "127.0.0.1:0", "LOG_LEVEL": "error"})
	err := serve(t.Context(), cfg, "test", []ext.Module{billing})
	if err == nil || !strings.Contains(err.Error(), "set MAILRULES_SMTP_HOST, MAILRULES_SMTP_FROM") {
		t.Fatalf("serve without a mail server = %v", err)
	}
	if fileExists(filepath.Join(dir, "mailrules.db")) {
		t.Error("the refused build opened the database")
	}
	withMail := loadCfg(t, dir, map[string]string{"MAILRULES_SMTP_HOST": "smtp.example.test", "MAILRULES_SMTP_FROM": "MailRules <noreply@example.test>"})
	for _, c := range []struct {
		name    string
		cfg     *config.Config
		modules []ext.Module
	}{
		{"plans with a mail server", withMail, []ext.Module{billing}},
		{"no plans, no mail server", cfg, []ext.Module{{Name: "free"}}},
	} {
		if err := checkPlanMail(c.cfg, c.modules); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}
