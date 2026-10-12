package store

import (
	"errors"
	"slices"
	"testing"
)

// teamEnd is when the team tenant's trial ended (forgetWorld buys it no plan).
func teamEnd(t *testing.T, w fworld) int64 {
	t.Helper()
	st, err := w.s.TenantPlan(t.Context(), w.leaver.TenantID, 1<<40)
	if err != nil || !st.Ended() || st.EndedAt != st.TrialEnd {
		t.Fatalf("team plan = %+v, %v; want its trial ended", st, err)
	}
	return st.EndedAt
}

func TestPlanEndTenants(t *testing.T) {
	w := forgetWorld(t)
	s, ctx := w.s, t.Context()
	end := teamEnd(t, w)
	// Solo buys a plan; Other buys one cancelled at a period's end a day after the trial's.
	if _, _, err := s.SetPlan(ctx, w.solo.TenantID, Plan{Plan: PlanPersonal, Interval: IntervalMonth, Status: StatusActive}, 100); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SetPlan(ctx, w.other.TenantID, Plan{Plan: PlanPro, Interval: IntervalYear, Status: StatusCancelsAtPeriodEnd,
		PeriodEnd: end + 2*day}, 100); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		now  int64
		want []int64
	}{
		{"every trial runs", end - 2*day, nil},
		{"the team's trial is over", end, []int64{w.leaver.TenantID}},
		{"the cancelled plan's period is over too", end + 2*day, []int64{w.leaver.TenantID, w.other.TenantID}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := s.PlanEndTenants(ctx, c.now)
			if err != nil || !slices.Equal(got, c.want) {
				t.Errorf("PlanEndTenants = %v, %v; want %v", got, err, c.want)
			}
		})
	}
}

// A trial's end is written as a plans row once, at the trial's own end, and never over a
// plan bought or a trial that runs.
func TestEndTrial(t *testing.T) {
	w := forgetWorld(t)
	s, ctx := w.s, t.Context()
	end := teamEnd(t, w)
	if _, _, err := s.SetPlan(ctx, w.solo.TenantID, Plan{Plan: PlanPersonal, Status: StatusActive}, 100); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		tenant int64
		now    int64
		wrote  bool
	}{
		{"a trial that runs", w.leaver.TenantID, end - 1, false},
		{"a trial that is over", w.leaver.TenantID, end + 5*day, true},
		{"a second time", w.leaver.TenantID, end + 6*day, false},
		{"a plan bought", w.solo.TenantID, end + 5*day, false},
		{"the self-host tenant, whose made-up trial is long over", SelfHostTenant, 1 << 40, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			wrote, err := s.EndTrial(ctx, c.tenant, c.now)
			if err != nil || wrote != c.wrote {
				t.Errorf("EndTrial = %v, %v; want %v", wrote, err, c.wrote)
			}
		})
	}
	var plan string
	var endedAt, warned, updated int64
	if err := w.db.QueryRowContext(ctx, `SELECT plan, ended_at, warned, updated_at FROM plans WHERE tenant_id = ?`, w.leaver.TenantID).
		Scan(&plan, &endedAt, &warned, &updated); err != nil || plan != PlanEnded || endedAt != end || warned != 0 || updated != end+5*day {
		t.Errorf("row = %s ended %d warned %d updated %d, %v; want ended at the trial's end %d, unwarned", plan, endedAt, warned, updated, err, end)
	}
	if st, _ := s.TenantPlan(ctx, w.solo.TenantID, end+5*day); st.Plan.Plan != PlanPersonal {
		t.Errorf("the plan bought became %+v", st)
	}
}

func TestPlanWarned(t *testing.T) {
	w := forgetWorld(t)
	s, ctx := w.s, t.Context()
	end := teamEnd(t, w)
	team := w.leaver.TenantID
	for _, c := range []struct {
		name       string
		endedAt    int64
		n          int
		at         int64
		err        error
		wantWarned int
		wantAt     int64
	}{
		// No row yet: the warning writes the trial's end with it.
		{"warning 1 on the day", end, 1, end + 60, nil, 1, end + 60},
		{"warning 2", end, 2, end + 23*day, nil, 2, end + 23*day},
		{"warning 1 again changes nothing", end, 1, end + 24*day, nil, 2, end + 23*day},
		{"another end", end + 1, 3, end + 83*day, ErrPlanNotEnded, 2, end + 23*day},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := s.PlanWarned(ctx, team, c.endedAt, c.n, c.at); !errors.Is(err, c.err) {
				t.Fatalf("PlanWarned = %v, want %v", err, c.err)
			}
			p, err := s.EndedPlan(ctx, team, c.at)
			if err != nil || p.EndedAt != end || p.Warned != c.wantWarned || p.WarnedAt != c.wantAt {
				t.Errorf("EndedPlan = %+v, %v; want warned %d at %d", p, err, c.wantWarned, c.wantAt)
			}
		})
	}
	// A plan bought again clears the warnings, and none can be recorded while it runs.
	if _, _, err := s.SetPlan(ctx, team, Plan{Plan: PlanPersonal, Status: StatusActive}, end+30*day); err != nil {
		t.Fatal(err)
	}
	if err := s.PlanWarned(ctx, team, end, 3, end+83*day); !errors.Is(err, ErrPlanNotEnded) {
		t.Errorf("warning a running plan: %v, want ErrPlanNotEnded", err)
	}
	if p, _ := s.EndedPlan(ctx, team, end+83*day); p.Warned != 0 || p.WarnedAt != 0 || p.Ended() {
		t.Errorf("after buying again = %+v", p)
	}
}

// Thirty days after the team's plan ended, every one of its mailboxes loses its secret (the
// ones a module connected are handed to it first), its model keys and notification
// addresses go, and the plan records it. Nobody else's are touched.
func TestDeleteTenantSecrets(t *testing.T) {
	w := forgetWorld(t)
	s, ctx := w.s, t.Context()
	end := teamEnd(t, w)
	team := w.leaver.TenantID
	// The leaver's shared mailbox is a one-click one, their private one a module's sign-in
	// form added; the teammate's is an ordinary one.
	if _, err := w.db.ExecContext(ctx, `UPDATE accounts SET oauth = TRUE WHERE id = ?`, w.leaverShared); err != nil {
		t.Fatal(err)
	}
	if _, err := w.db.ExecContext(ctx, `UPDATE accounts SET module = TRUE WHERE id = ?`, w.leaverPriv); err != nil {
		t.Fatal(err)
	}
	secretsAt := end + EndedSecretsDays*day
	for _, c := range []struct {
		name    string
		tenant  int64
		endedAt int64
		now     int64
	}{
		{"too early", team, end, secretsAt - 1},
		{"another end", team, end - 1, secretsAt},
		{"a tenant whose plan runs", w.solo.TenantID, end, secretsAt},
		{"the self-host tenant", SelfHostTenant, end, secretsAt},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.tenant == w.solo.TenantID {
				if _, _, err := s.SetPlan(ctx, c.tenant, Plan{Plan: PlanPersonal, Status: StatusActive}, 100); err != nil {
					t.Fatal(err)
				}
			}
			if ids, err := s.DeleteTenantSecrets(ctx, c.tenant, c.endedAt, c.now); err == nil {
				t.Errorf("DeleteTenantSecrets = %v, want refused", ids)
			}
		})
	}
	for _, id := range []int64{w.adminBox, w.soloBox, w.mateBox, w.leaverPriv, w.leaverShared, w.otherBox} {
		if a, _ := s.Account(ctx, id); a.SecretGone {
			t.Fatalf("a refused deletion took mailbox %d's secret", id)
		}
	}

	ids, err := s.DeleteTenantSecrets(ctx, team, end, secretsAt)
	if want := []int64{w.mateBox, w.leaverPriv, w.leaverShared}; err != nil || !slices.Equal(ids, want) {
		t.Fatalf("DeleteTenantSecrets = %v, %v; want %v", ids, err, want)
	}
	for _, c := range []struct {
		id          int64
		status, msg string
	}{
		{w.mateBox, "auth_failed", planEndedMessage},
		{w.leaverPriv, "reconnect_needed", planEndedOAuthMessage},
		{w.leaverShared, "reconnect_needed", planEndedOAuthMessage},
	} {
		a, err := s.Account(ctx, c.id)
		if err != nil || !a.SecretGone || a.Status != c.status || a.LastError != c.msg || a.LastEventAt != secretsAt {
			t.Errorf("mailbox %d = %+v, %v; want its secret gone and %s", c.id, a, err, c.status)
		}
		if _, err := s.AccountSecret(ctx, c.id); !errors.Is(err, ErrNoSecret) {
			t.Errorf("mailbox %d secret: %v, want ErrNoSecret", c.id, err)
		}
	}
	queued, err := s.WaitingRemovedMailboxes(ctx, secretsAt, 10)
	if err != nil {
		t.Fatal(err)
	}
	var queuedIDs []int64
	for _, q := range queued {
		queuedIDs = append(queuedIDs, q.AccountID)
	}
	if want := []int64{w.leaverPriv, w.leaverShared}; !slices.Equal(queuedIDs, want) {
		t.Errorf("handed to the module: %v, want %v", queuedIDs, want)
	}
	if _, err := s.Setting(ctx, team, "key.anthropic_api_key"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the team's model key: %v, want it gone", err)
	}
	if _, err := s.Setting(ctx, team, "seed_mate"); err != nil {
		t.Errorf("an ordinary setting went: %v", err)
	}
	ds, err := s.Destinations(ctx, team)
	if err != nil || len(ds) == 0 {
		t.Fatalf("team destinations = %v, %v", ds, err)
	}
	for _, d := range ds {
		if !d.AddressDeleted {
			t.Errorf("team destination %+v kept its address", d)
		}
	}
	if p, err := s.EndedPlan(ctx, team, secretsAt); err != nil || !p.SecretsDeleted || p.EndedAt != end {
		t.Errorf("plan = %+v, %v; want its secrets recorded deleted", p, err)
	}
	if rows := w.tenantRows(t, team); rows["accounts"] != 3 || rows["rules"] == 0 || rows["messages"] == 0 {
		t.Errorf("the team's data went with its secrets: %v", rows)
	}
	// Nobody else's.
	for _, id := range []int64{w.adminBox, w.soloBox, w.otherBox} {
		if _, err := s.AccountSecret(ctx, id); err != nil {
			t.Errorf("someone else's secret %d: %v", id, err)
		}
	}
	for _, tenant := range []int64{w.solo.TenantID, w.other.TenantID} {
		if _, err := s.Setting(ctx, tenant, "key.anthropic_api_key"); err != nil {
			t.Errorf("tenant %d's model key: %v", tenant, err)
		}
		if ds, _ := s.Destinations(ctx, tenant); len(ds) != 1 || ds[0].AddressDeleted {
			t.Errorf("tenant %d's destinations = %+v", tenant, ds)
		}
	}
	// A second time hands nothing more over and keeps the statuses.
	if _, err := s.DeleteTenantSecrets(ctx, team, end, secretsAt+day); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.WaitingRemovedMailboxes(ctx, secretsAt+day, 10); len(again) != 2 {
		t.Errorf("a second deletion queued %d mailboxes, want the same 2", len(again))
	}
	if a, _ := s.Account(ctx, w.mateBox); a.LastEventAt != secretsAt {
		t.Errorf("a second deletion changed the mailbox: %+v", a)
	}
}

// Ninety days after the team's plan ended, everything of the team goes; every other tenant
// keeps every row, and the self-host tenant is never removed.
func TestRemoveEndedTenant(t *testing.T) {
	w := forgetWorld(t)
	s, ctx := w.s, t.Context()
	end := teamEnd(t, w)
	team := w.leaver.TenantID
	removeAt := end + EndedRemovalDays*day
	if err := s.PlanWarned(ctx, team, end, 3, end+83*day); err != nil {
		t.Fatal(err)
	}
	others := map[int64]map[string]int{}
	for _, tenant := range []int64{SelfHostTenant, w.solo.TenantID, w.other.TenantID} {
		others[tenant] = w.tenantRows(t, tenant)
	}
	for _, c := range []struct {
		name    string
		tenant  int64
		endedAt int64
		now     int64
		err     error
	}{
		{"too early", team, end, removeAt - 1, ErrPlanNotEnded},
		{"another end", team, end + day, removeAt + day, ErrPlanNotEnded},
		{"the self-host tenant", SelfHostTenant, end, removeAt, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.RemoveEndedTenant(ctx, c.tenant, c.endedAt, c.now)
			if err == nil || (c.err != nil && !errors.Is(err, c.err)) {
				t.Errorf("RemoveEndedTenant = %v, want refused (%v)", err, c.err)
			}
		})
	}
	if rows := w.tenantRows(t, team); rows["tenants"] != 1 || rows["accounts"] != 3 {
		t.Fatalf("a refused removal removed: %v", rows)
	}
	gone, err := s.RemoveEndedTenant(ctx, team, end, removeAt)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{w.mateBox, w.leaverPriv, w.leaverShared}; !gone.Tenant || gone.TenantID != team || !slices.Equal(gone.Accounts, want) {
		t.Errorf("removal = %+v, want the whole tenant with %v", gone, want)
	}
	for table, n := range w.tenantRows(t, team) {
		if n != 0 {
			t.Errorf("%s: %d rows of the removed tenant left", table, n)
		}
	}
	for tenant, want := range others {
		got := w.tenantRows(t, tenant)
		for table, n := range want {
			if got[table] != n {
				t.Errorf("tenant %d %s: %d rows, want %d", tenant, table, got[table], n)
			}
		}
	}
	if _, err := s.TenantPlan(ctx, team, removeAt); !errors.Is(err, ErrNotFound) {
		t.Errorf("the removed tenant's plan: %v, want ErrNotFound", err)
	}
	if _, err := s.RemoveEndedTenant(ctx, team, end, removeAt); !errors.Is(err, ErrNotFound) {
		t.Errorf("a second removal: %v, want ErrNotFound", err)
	}
}

func TestTenantEmails(t *testing.T) {
	w := forgetWorld(t)
	s, ctx := w.s, t.Context()
	if _, err := s.ForgetIdentity(ctx, "workos", "user_leaver", fNow); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		tenant int64
		want   []string
	}{
		{w.leaver.TenantID, []string{"mate@example.com"}}, // the forgotten leaver's placeholder is not one
		{w.solo.TenantID, []string{"solo@example.com"}},
		{9999, nil},
	} {
		if got, err := s.TenantEmails(ctx, c.tenant); err != nil || !slices.Equal(got, c.want) {
			t.Errorf("TenantEmails(%d) = %v, %v; want %v", c.tenant, got, err, c.want)
		}
	}
}
