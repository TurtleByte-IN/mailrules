package store

import (
	"context"
	"testing"
)

// CheckCompanySignIn saves the loss date once, keeps it, clears it when company sign-in
// comes back, and gives a tenant without a stored plan none.
func TestCheckCompanySignIn(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	id, err := s.CreateTenant(context.Background(), "workos", "org", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.CheckCompanySignIn(ctx, id, false, 50); err != nil || got != 0 {
		t.Fatalf("no plan stored = %d, %v; want 0", got, err)
	}
	if _, _, err := s.SetPlan(ctx, id, Plan{Plan: PlanTeam, Interval: IntervalMonth, Status: StatusActive, Record: "sub", Seats: 3}, 10); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		allowed bool
		lossEnd int64
		want    int64
	}{
		{"never held, not allowed", false, 100, 0},
		{"allowed", true, 100, 0},
		{"allowed again", true, 100, 0},
		{"lost", false, 200, 200},
		{"still lost, a later period", false, 300, 200},
		{"back", true, 300, 0},
		{"lost again", false, 400, 400},
	} {
		got, err := s.CheckCompanySignIn(ctx, id, c.allowed, c.lossEnd)
		if err != nil || got != c.want {
			t.Errorf("%s: = %d, %v; want %d", c.name, got, err, c.want)
		}
		if p, err := s.TenantPlan(ctx, id, 20); err != nil || p.CompanySignInEndsAt != c.want {
			t.Errorf("%s: TenantPlan ends at %d, %v; want %d", c.name, p.CompanySignInEndsAt, err, c.want)
		}
	}
	// A report keeps the date.
	if _, after, err := s.SetPlan(ctx, id, Plan{Plan: PlanTeam, Interval: IntervalMonth, Status: StatusActive, Record: "sub", Seats: 4}, 30); err != nil || after.CompanySignInEndsAt != 400 {
		t.Errorf("after a report: ends at %d, %v; want 400", after.CompanySignInEndsAt, err)
	}
}
