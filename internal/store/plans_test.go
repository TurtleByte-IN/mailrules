package store

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// The company sign-in add-on is kept with the Team plan as the module reports it, refused
// with any other plan, and its coming and going is in the audit log.
func TestSetPlanCompanySignInAddOn(t *testing.T) {
	s, _ := open(t)
	ctx := WithActor(t.Context(), Actor{Name: "Billing service"})
	id, err := s.CreateTenant(context.Background(), "workos", "org", 0)
	if err != nil {
		t.Fatal(err)
	}
	team := Plan{Plan: PlanTeam, Interval: IntervalMonth, Status: StatusActive, Record: "sub", Seats: 3, CompanySignInAddOn: true}
	if _, after, err := s.SetPlan(ctx, id, team, 10); err != nil || !after.CompanySignInAddOn {
		t.Fatalf("SetPlan = %+v, %v; want the add-on", after, err)
	}
	if got, err := s.TenantPlan(ctx, id, 11); err != nil || !got.CompanySignInAddOn || got.Seats != 3 {
		t.Errorf("TenantPlan = %+v, %v", got, err)
	}
	if _, _, err := s.SetPlan(ctx, id, Plan{Plan: PlanPro, Interval: IntervalMonth, Status: StatusActive, CompanySignInAddOn: true}, 12); !errors.Is(err, ErrBadPlan) {
		t.Errorf("the add-on on Pro = %v, want ErrBadPlan", err)
	}
	team.CompanySignInAddOn = false
	if _, after, err := s.SetPlan(ctx, id, team, 13); err != nil || after.CompanySignInAddOn {
		t.Fatalf("SetPlan = %+v, %v; want no add-on", after, err)
	}
	if err := s.Audit(WithActor(ctx, Actor{UserID: 1, Name: "owner@team.test"}), id, AuditCompanySignInAddOn, "Company sign-in add-on", "asked by owner@team.test"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"plan.changed team: trial to team, month, active, 3 seats, company sign-in add-on (Billing service)",
		"plan.changed team: team, month, active, 3 seats, company sign-in add-on to team, month, active, 3 seats (Billing service)",
		"company_sign_in.add_on Company sign-in add-on: asked by an address (owner@team.test)",
	}
	if got := audited(t, s, id); !slices.Equal(got, want) {
		t.Errorf("audit log = %q, want %q", got, want)
	}
}

func TestTenantPlan(t *testing.T) {
	const made, day0 = int64(1_000_000), int64(1_000_000 + TrialDays*day) // the tenant's creation, and its trial's end
	tests := []struct {
		name    string
		reports []Plan // what the billing module reported, in order, at made+1
		now     int64
		plan    string
		endedAt int64
	}{
		{"in the trial", nil, day0 - 1, PlanTrial, 0},
		{"the trial over, nothing bought", nil, day0, PlanEnded, day0},
		{"bought", []Plan{{Plan: PlanPro, Interval: IntervalYear, Status: StatusActive}}, day0 + 100*day, PlanPro, 0},
		{"past due keeps the plan", []Plan{{Plan: PlanPersonal, Status: StatusPastDue, PeriodEnd: day0}}, day0 + day, PlanPersonal, 0},
		{"cancelled, before the period ends", []Plan{{Plan: PlanPro, Status: StatusCancelsAtPeriodEnd, PeriodEnd: day0}}, day0 - 1, PlanPro, 0},
		{"cancelled, the period over", []Plan{{Plan: PlanPro, Status: StatusCancelsAtPeriodEnd, PeriodEnd: day0}}, day0, PlanEnded, day0},
		{"ended by the module", []Plan{{Plan: PlanPersonal}, {Plan: PlanEnded}}, day0 + day, PlanEnded, made + 1},
		{"bought again", []Plan{{Plan: PlanEnded}, {Plan: PlanPersonal, Status: StatusActive}}, day0 + day, PlanPersonal, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := open(t)
			ctx := t.Context()
			id, err := s.CreateTenant(ctx, "workos", "org", made)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range tt.reports {
				if _, _, err := s.SetPlan(ctx, id, p, made+1); err != nil {
					t.Fatal(err)
				}
			}
			got, err := s.TenantPlan(ctx, id, tt.now)
			if err != nil || got.Plan.Plan != tt.plan || got.EndedAt != tt.endedAt || got.TrialEnd != day0 {
				t.Errorf("TenantPlan = %+v, %v; want %s ended at %d", got, err, tt.plan, tt.endedAt)
			}
		})
	}
}

// Reporting the end again, or a cancelled plan whose period is over again, keeps when it
// ended and how far the end-of-plan job got; buying a plan clears them.
func TestSetPlanKeepsTheEndOfAPlan(t *testing.T) {
	s, db := open(t)
	ctx := t.Context()
	id, err := s.CreateTenant(ctx, "workos", "org", 0)
	if err != nil {
		t.Fatal(err)
	}
	now := int64(TrialDays*day + 5*day) // the trial ended 5 days ago
	progress := func(warned int, deleted bool) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `UPDATE plans SET warned = ?, warned_at = ?, secrets_deleted = ? WHERE tenant_id = ?`, warned, now, deleted, id); err != nil {
			t.Fatal(err)
		}
	}
	before, after, err := s.SetPlan(ctx, id, Plan{Plan: PlanEnded}, now)
	if err != nil || !before.Ended() || before.EndedAt != TrialDays*day || after.EndedAt != TrialDays*day {
		t.Fatalf("ending an ended trial = %+v → %+v, %v; want the trial's end kept", before, after, err)
	}
	progress(2, true)
	for _, p := range []Plan{{Plan: PlanEnded}, {Plan: PlanPro, Status: StatusCancelsAtPeriodEnd, PeriodEnd: now - day}} {
		if _, after, err = s.SetPlan(ctx, id, p, now+day); err != nil {
			t.Fatal(err)
		}
		if !after.Ended() || after.Warned != 2 || !after.SecretsDeleted {
			t.Errorf("after %+v: %+v; want still ended with its progress", p, after)
		}
	}
	if _, after, err = s.SetPlan(ctx, id, Plan{Plan: PlanPersonal, Status: StatusActive, Record: "sub_1"}, now+2*day); err != nil {
		t.Fatal(err)
	}
	if after.Ended() || after.Warned != 0 || after.SecretsDeleted || after.Record != "sub_1" {
		t.Errorf("bought again: %+v", after)
	}
	var warnedAt *int64
	if err := db.QueryRowContext(ctx, `SELECT warned_at FROM plans WHERE tenant_id = ?`, id).Scan(&warnedAt); err != nil || warnedAt != nil {
		t.Errorf("warned_at = %v, %v; want cleared", warnedAt, err)
	}
	for _, bad := range []Plan{{Plan: PlanTrial}, {Plan: "team"}, {Plan: PlanPro, Interval: "week"}, {Plan: PlanPro, Status: "paused"}} {
		if _, _, err := s.SetPlan(ctx, id, bad, now); !errors.Is(err, ErrBadPlan) {
			t.Errorf("SetPlan(%+v) = %v, want ErrBadPlan", bad, err)
		}
	}
	if _, _, err := s.SetPlan(ctx, id+100, Plan{Plan: PlanPro}, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetPlan of no tenant = %v", err)
	}
}
