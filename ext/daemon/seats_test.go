package daemon

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/api"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/store/storetest"
)

// The seat worker gives back the seats a team freed (MAI-247): it asks the billing module
// for the seats then used, never below the plan's least, keeps a check the module refused
// for later, and drops one in a build that sells no plans.
func TestSeatWorker(t *testing.T) {
	ctx := t.Context()
	st, _ := storetest.New(t)
	sign := func(sub string, at int64) store.User {
		t.Helper()
		u, err := st.SignInIdentity(ctx, store.Identity{Provider: "workos", Subject: sub, Email: sub + "@team.test", Tenant: "org_team"}, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	tenant := sign("owner", 1).TenantID
	for _, sub := range []string{"m1", "m2", "m3"} {
		sign(sub, 2)
	}
	if _, _, err := st.SetPlan(ctx, tenant, store.Plan{Plan: store.PlanTeam, Interval: store.IntervalMonth, Status: store.StatusActive,
		Record: "sub_t", Seats: 4}, 3); err != nil {
		t.Fatal(err)
	}
	var asked []int
	refuse := 1
	billing := &ext.Billing{
		Plans: map[string]ext.PlanTerms{store.PlanTeam: {Name: "Team", PerSeat: true, MinSeats: 2}},
		ChangePlan: func(_ context.Context, c ext.PlanChange) error {
			if c.TenantID != tenant || c.Record != "sub_t" || c.Plan != store.PlanTeam || c.Interval != store.IntervalMonth {
				t.Errorf("change = %+v", c)
			}
			asked = append(asked, c.Seats)
			if refuse > 0 {
				refuse--
				return errors.New("billing service down")
			}
			return nil
		},
	}
	now := time.Unix(1000, 0)
	w := &seatWorker{store: st, lower: api.LowerSeats(st, []ext.Module{{Name: "plans", Billing: billing}}), now: func() time.Time { return now }}
	waiting := func(at time.Time) []store.SeatCheck {
		t.Helper()
		cs, err := st.WaitingSeatChecks(ctx, at.Unix(), 10)
		if err != nil {
			t.Fatal(err)
		}
		return cs
	}

	if _, err := st.RemoveMember(ctx, "workos", "m1", now.Unix()); err != nil {
		t.Fatal(err)
	}
	if err := w.once(ctx); err != nil {
		t.Fatal(err)
	}
	if cs := waiting(now); !slices.Equal(asked, []int{3}) || len(cs) != 0 {
		t.Fatalf("asked %v; waiting now %+v; want 3 asked, refused, and the check kept for later", asked, cs)
	}
	now = now.Add(time.Minute)
	if cs := waiting(now); len(cs) != 1 || cs[0].Attempts != 1 {
		t.Fatalf("waiting after the refusal = %+v", cs)
	}
	if err := w.once(ctx); err != nil {
		t.Fatal(err)
	}
	if cs := waiting(now.Add(time.Hour)); !slices.Equal(asked, []int{3, 3}) || len(cs) != 0 {
		t.Fatalf("asked %v; waiting %+v; want 3 asked again and the check gone", asked, cs)
	}

	// Down to one person: never below the plan's least.
	for _, sub := range []string{"m2", "m3"} {
		if _, err := st.RemoveMember(ctx, "workos", sub, now.Unix()); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.once(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(asked, []int{3, 3, 2}) {
		t.Errorf("asked %v; want 2 last, the plan's least", asked)
	}

	// A build that sells no plans drops what is queued.
	if err := st.QueueSeatCheck(ctx, tenant, now.Unix()); err != nil {
		t.Fatal(err)
	}
	if err := (&seatWorker{store: st, now: w.now}).once(ctx); err != nil {
		t.Fatal(err)
	}
	if cs := waiting(now.Add(time.Hour)); len(cs) != 0 || len(asked) != 3 {
		t.Errorf("waiting %+v, asked %v; want the check dropped unasked", cs, asked)
	}
}
