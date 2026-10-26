package worker

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/mailer/mailertest"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/store/storetest"
)

// The trial reminder goes once to the owners of a tenant in its trial, store.TrialReminderDays
// before the trial ends: not sooner, not again however many sweeps and restarts follow, not
// to a tenant on a paid plan or the self-host one, and not to a team's other members.
func TestTrialReminder(t *testing.T) {
	const reminder = store.TrialReminderDays * day
	tests := []struct {
		name   string
		at     []int64 // seconds from the team's trial end; one new job (a restart) each
		failAt int     // index of the sweep whose sends fail; -1 = none
		want   int     // reminders the owner got in all
	}{
		{"before the mark", []int64{-reminder - 1}, -1, 0},
		{"at the mark", []int64{-reminder}, -1, 1},
		{"two sweeps and a restart", []int64{-reminder, -reminder + 60, -day}, -1, 1},
		{"first seen on the last day", []int64{-1}, -1, 1},
		{"after the trial ended", []int64{0}, -1, 0},
		{"a failed send is tried again", []int64{-reminder, -reminder + 60, -reminder + 120}, 0, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			st, _ := storetest.New(t)
			if _, err := st.CreateFirstUser(ctx, "admin@self.test", "hash", planT0); err != nil {
				t.Fatal(err)
			}
			signIn := func(sub, email, org string) store.User {
				u, err := st.SignInIdentity(ctx, store.Identity{Provider: "workos", Subject: sub, Email: email, Tenant: org}, planT0, nil)
				if err != nil {
					t.Fatal(err)
				}
				return u
			}
			owner := signIn("user_owner", "owner@example.com", "org_team")
			if mate := signIn("user_mate", "mate@example.com", "org_team"); mate.Role == store.RoleOwner {
				t.Fatalf("mate is an owner")
			}
			paid := signIn("user_paid", "paid@example.com", "org_paid")
			if _, _, err := st.SetPlan(ctx, paid.TenantID, store.Plan{Plan: store.PlanPro, Interval: store.IntervalMonth, Status: store.StatusActive}, planT0); err != nil {
				t.Fatal(err)
			}
			p, err := st.TenantPlan(ctx, owner.TenantID, planT0)
			if err != nil {
				t.Fatal(err)
			}
			sender := &mailertest.Sender{}
			for i, at := range tt.at {
				now := time.Unix(p.TrialEnd+at, 0)
				if i == tt.failAt {
					sender.SetErr(errors.New("connection refused"))
				} else {
					sender.SetErr(nil)
				}
				j := PlanEnd{Store: st, Stop: func(int64) {}, Start: func(store.Account) {}, Sender: sender,
					PublicURL: "https://mail.example/", Now: func() time.Time { return now }}
				_, err := j.Once(ctx)
				if (err != nil) != (i == tt.failAt) {
					t.Fatalf("sweep %d: %v", i, err)
				}
			}
			var got []string
			for _, m := range sender.Sent() {
				if strings.HasPrefix(m.Subject, "Your MailRules trial ends on ") {
					got = append(got, m.To)
				}
			}
			if want := slices.Repeat([]string{"owner@example.com"}, tt.want); !slices.Equal(got, want) {
				t.Errorf("reminders went to %v; want %v", got, want)
			}
		})
	}
}

func TestTrialReminderMessage(t *testing.T) {
	const end = 1_800_000_000
	m, err := trialReminder(end, time.Unix(end-3*day, 0), "https://mail.example/")
	if err != nil {
		t.Fatal(err)
	}
	when := time.Unix(end, 0).UTC().Format(dateLayout)
	for _, part := range []string{m.Text, m.HTML} {
		for _, s := range []string{when, "in 3 days", "https://mail.example/#/plan", "Plan and billing"} {
			if !strings.Contains(part, s) {
				t.Errorf("message lacks %q:\n%s", s, part)
			}
		}
	}
	if m.Subject != "Your MailRules trial ends on "+when {
		t.Errorf("subject = %q", m.Subject)
	}
}
