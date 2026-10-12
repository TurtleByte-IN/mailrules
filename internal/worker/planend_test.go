package worker

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mailer"
	"github.com/TurtleByte-IN/mailrules/internal/mailer/mailertest"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// planWorld is the self-host admin with a mailbox, a hosted team of two that never buys a
// plan (its trial ends at end), and another hosted tenant on Pro, each with a mailbox.
type planWorld struct {
	st                 *store.Store
	m                  *Manager
	hub                *events.Hub
	team, other        int64
	adminBox, otherBox store.Account
	teamBoxes          []store.Account
	end                int64 // when the team's trial ended
	now                time.Time
	sender             *mailertest.Sender
	runCtx             context.Context // the supervisors run until it ends
}

const planT0 = 1_800_000_000

func newPlanWorld(t *testing.T) *planWorld {
	t.Helper()
	ctx := t.Context()
	st, db := openAt(t, t.TempDir())
	t.Cleanup(func() { _ = db.Close() })
	w := &planWorld{st: st, m: &Manager{}, hub: events.NewHub(), sender: &mailertest.Sender{}}
	box := func(u store.User, name string) store.Account {
		a, err := st.CreateAccount(ctx, make([]byte, 32), store.Account{UserID: u.ID, Label: name, Preset: "generic",
			Host: "h", Port: 993, TLSMode: "implicit", Username: name + "@example.com"}, "pw-"+name)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	signIn := func(sub, email, org string) store.User {
		u, err := st.SignInIdentity(ctx, store.Identity{Provider: "workos", Subject: sub, Email: email, Tenant: org}, planT0)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	admin, err := st.CreateFirstUser(ctx, "admin@self.test", "hash", 1)
	if err != nil {
		t.Fatal(err)
	}
	w.adminBox = box(admin, "admin")
	owner, mate := signIn("user_owner", "owner@example.com", "org_team"), signIn("user_mate", "mate@example.com", "org_team")
	w.team = owner.TenantID
	w.teamBoxes = []store.Account{box(owner, "owner"), box(mate, "mate")}
	if err := st.SetSetting(ctx, w.team, "key.anthropic_api_key", `{"sealed":"x"}`); err != nil {
		t.Fatal(err)
	}
	other := signIn("user_other", "other@example.com", "org_other")
	w.other = other.TenantID
	w.otherBox = box(other, "other")
	if _, _, err := st.SetPlan(ctx, w.other, store.Plan{Plan: store.PlanPro, Interval: store.IntervalMonth, Status: store.StatusActive}, planT0); err != nil {
		t.Fatal(err)
	}
	p, err := st.TenantPlan(ctx, w.team, planT0)
	if err != nil || p.Plan.Plan != store.PlanTrial {
		t.Fatalf("team plan = %+v, %v", p, err)
	}
	w.end = p.TrialEnd
	// Every mailbox runs.
	runCtx, cancel := context.WithCancel(ctx)
	w.runCtx = runCtx
	t.Cleanup(func() { cancel(); w.m.Wait() })
	for _, a := range append([]store.Account{w.adminBox, w.otherBox}, w.teamBoxes...) {
		supervise(runCtx, w.m, st, a)
	}
	for _, a := range append([]store.Account{w.adminBox, w.otherBox}, w.teamBoxes...) {
		eventually(t, "mailbox "+a.Label+" connected", func() bool { _, err := w.m.Mailbox(a.ID); return err == nil })
	}
	return w
}

func (w *planWorld) job(sender mailer.Sender) PlanEnd {
	return PlanEnd{Store: w.st, Stop: w.m.Stop, Hub: w.hub, Sender: sender, PublicURL: "https://mail.example/",
		Start: func(a store.Account) { supervise(w.runCtx, w.m, w.st, a) },
		Now:   func() time.Time { return w.now }}
}

// A plan bought at the moment the job stops the mailboxes (the mailboxes started by the
// purchase just before the stop) gets them running again, and nothing is sent.
func TestPlanEndBoughtWhileStopping(t *testing.T) {
	w := newPlanWorld(t)
	j := w.job(w.sender)
	stop := j.Stop
	bought := false
	j.Stop = func(id int64) {
		if !bought {
			bought = true
			if _, _, err := w.st.SetPlan(t.Context(), w.team, store.Plan{Plan: store.PlanPersonal, Interval: store.IntervalMonth,
				Status: store.StatusActive}, w.now.Unix()); err != nil {
				t.Error(err)
			}
		}
		stop(id)
	}
	w.at(t, j, 0)
	if !bought {
		t.Fatal("the job stopped nothing")
	}
	for _, a := range w.teamBoxes {
		eventually(t, "mailbox "+a.Label+" running again", func() bool { return w.running(a.ID) })
	}
	if n := len(w.sender.Sent()); n != 0 {
		t.Errorf("%d warnings sent for a plan bought", n)
	}
	if p := w.plan(t); p.Ended() || p.Warned != 0 {
		t.Errorf("plan = %+v, want it running and unwarned", p)
	}
}

// at runs the job at end+days, twice: a step is taken once however often the job runs.
func (w *planWorld) at(t *testing.T, j PlanEnd, days float64) {
	t.Helper()
	w.now = time.Unix(w.end+int64(days*day), 0)
	for range 2 {
		if _, err := j.Once(t.Context()); err != nil {
			t.Fatalf("day %v: %v", days, err)
		}
	}
}

func (w *planWorld) running(id int64) bool {
	_, err := w.m.Mailbox(id)
	return err == nil
}

func (w *planWorld) plan(t *testing.T) store.EndedPlan {
	t.Helper()
	p, err := w.st.EndedPlan(t.Context(), w.team, w.now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (w *planWorld) teamExists(t *testing.T) bool {
	t.Helper()
	_, err := w.st.TenantPlan(t.Context(), w.team, w.now.Unix())
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

func (w *planWorld) secretsGone(t *testing.T) bool {
	t.Helper()
	gone := 0
	for _, a := range w.teamBoxes {
		if _, err := w.st.AccountSecret(t.Context(), make([]byte, 32), a.ID); errors.Is(err, store.ErrNoSecret) {
			gone++
		}
	}
	_, keyErr := w.st.Setting(t.Context(), w.team, "key.anthropic_api_key")
	if (gone == len(w.teamBoxes)) != errors.Is(keyErr, store.ErrNotFound) {
		t.Fatalf("%d of %d mailbox secrets gone, model key: %v", gone, len(w.teamBoxes), keyErr)
	}
	return gone == len(w.teamBoxes)
}

// subjects are the subjects sent so far, each once per member.
func subjects(s *mailertest.Sender) []string {
	var out []string
	for _, m := range s.Sent() {
		out = append(out, m.To+": "+m.Subject)
	}
	return out
}

// sent is what both members of the team get for a warning.
func sent(subject string) []string {
	return []string{"owner@example.com: " + subject, "mate@example.com: " + subject}
}

// The whole schedule: warning 1 and every mailbox stopped on the day the trial ends,
// warning 2 on day 23, the secrets on day 30, warning 3 on day 83, the removal on day 90.
// The self-host tenant and the tenant on Pro are never touched.
func TestPlanEndSchedule(t *testing.T) {
	w := newPlanWorld(t)
	j := w.job(w.sender)
	_, live, cancel := w.hub.Subscribe(0)
	defer cancel()
	d := func(days int64) string { return time.Unix(w.end+days*day, 0).UTC().Format("2 January 2006") }
	w1 := "Your MailRules trial has ended"
	w2 := "MailRules will delete your mailbox passwords on " + d(30)
	w3 := "MailRules will delete your data on " + d(90)
	for _, c := range []struct {
		day     float64
		sent    []string
		warned  int
		secrets bool
		exists  bool
	}{
		{-1, nil, 0, false, true},
		{0, sent(w1), 1, false, true},
		{1, sent(w1), 1, false, true},
		{23, append(sent(w1), sent(w2)...), 2, false, true},
		{29.9, append(sent(w1), sent(w2)...), 2, false, true},
		{30, append(sent(w1), sent(w2)...), 2, true, true},
		{83, append(append(sent(w1), sent(w2)...), sent(w3)...), 3, true, true},
		{89.9, append(append(sent(w1), sent(w2)...), sent(w3)...), 3, true, true},
		{90, append(append(sent(w1), sent(w2)...), sent(w3)...), 3, true, false},
		{120, append(append(sent(w1), sent(w2)...), sent(w3)...), 3, true, false},
	} {
		w.at(t, j, c.day)
		if got := subjects(w.sender); !slices.Equal(got, c.sent) {
			t.Errorf("day %v: sent %q, want %q", c.day, got, c.sent)
		}
		if exists := w.teamExists(t); exists != c.exists {
			t.Fatalf("day %v: team exists = %v, want %v", c.day, exists, c.exists)
		}
		if c.exists {
			if p := w.plan(t); p.Warned != c.warned || p.SecretsDeleted != c.secrets || (c.day >= 0 && p.EndedAt != w.end) {
				t.Errorf("day %v: plan = %+v, want warned %d, secrets deleted %v", c.day, p, c.warned, c.secrets)
			}
			if gone := w.secretsGone(t); gone != c.secrets {
				t.Errorf("day %v: secrets gone = %v, want %v", c.day, gone, c.secrets)
			}
		}
		for _, a := range w.teamBoxes {
			if running := w.running(a.ID); running != (c.day < 0) {
				t.Errorf("day %v: team mailbox %s running = %v", c.day, a.Label, running)
			}
		}
		for _, a := range []store.Account{w.adminBox, w.otherBox} {
			if !w.running(a.ID) {
				t.Fatalf("day %v: %s's mailbox was stopped", c.day, a.Label)
			}
			if _, err := w.st.AccountSecret(t.Context(), make([]byte, 32), a.ID); err != nil {
				t.Fatalf("day %v: %s's secret: %v", c.day, a.Label, err)
			}
		}
	}
	// The open screens learn the mailboxes lost their secrets.
	var statuses []int64
	for len(live) > 0 {
		if ev := <-live; ev.Name == events.AccountStatus && ev.TenantID == w.team && ev.Data.(store.Account).SecretGone {
			statuses = append(statuses, ev.AccountID)
		}
	}
	if want := []int64{w.teamBoxes[0].ID, w.teamBoxes[1].ID}; !slices.Equal(statuses, want) {
		t.Errorf("secret-gone status events for %v, want %v", statuses, want)
	}
	// The self-host tenant has no plan: TenantPlan would call its trial over by now, but the
	// job never writes a step for it.
	if p, err := w.st.EndedPlan(t.Context(), store.SelfHostTenant, w.now.Unix()); err != nil || p.Warned != 0 || p.SecretsDeleted {
		t.Errorf("self-host plan = %+v, %v; want no step taken", p, err)
	}
}

// A plan bought again before day 90 stops the schedule: nothing more is sent or deleted,
// whatever the time.
func TestPlanEndBoughtAgain(t *testing.T) {
	w := newPlanWorld(t)
	j := w.job(w.sender)
	w.at(t, j, 0)
	w.at(t, j, 23)
	w.at(t, j, 30)
	if !w.secretsGone(t) {
		t.Fatal("secrets kept on day 30")
	}
	if _, _, err := w.st.SetPlan(t.Context(), w.team, store.Plan{Plan: store.PlanPersonal, Interval: store.IntervalMonth,
		Status: store.StatusActive}, w.now.Unix()); err != nil {
		t.Fatal(err)
	}
	before := len(w.sender.Sent())
	for _, day := range []float64{40, 83, 90, 200} {
		w.at(t, j, day)
	}
	if !w.teamExists(t) || len(w.sender.Sent()) != before {
		t.Errorf("after buying again: team exists %v, %d more emails", w.teamExists(t), len(w.sender.Sent())-before)
	}
	if p := w.plan(t); p.Ended() || p.Warned != 0 {
		t.Errorf("plan = %+v, want it running", p)
	}
}

// With no mail server the warnings are logged and the schedule goes on all the same.
func TestPlanEndWithoutSMTP(t *testing.T) {
	w := newPlanWorld(t)
	j := w.job(nil)
	for _, c := range []struct {
		day     float64
		warned  int
		secrets bool
	}{{0, 1, false}, {23, 2, false}, {30, 2, true}, {83, 3, true}} {
		w.at(t, j, c.day)
		if p := w.plan(t); p.Warned != c.warned || p.SecretsDeleted != c.secrets {
			t.Errorf("day %v: plan = %+v, want warned %d, secrets deleted %v", c.day, p, c.warned, c.secrets)
		}
	}
	w.at(t, j, 90)
	if w.teamExists(t) {
		t.Error("the team was not removed on day 90")
	}
}

// A tenant the job first sees late gets only the latest warning due, and no deletion comes
// sooner than NoticeDays after the warning announcing it, at the dates the warning gives.
func TestPlanEndFirstSeenLate(t *testing.T) {
	w := newPlanWorld(t)
	j := w.job(w.sender)
	w.at(t, j, 85)
	got := w.sender.Sent()
	if len(got) != 2 {
		t.Fatalf("sent %q, want warning 3 to both members", subjects(w.sender))
	}
	notice := time.Unix(w.end+92*day, 0).UTC().Format("2 January 2006")
	if got[0].Subject != "MailRules will delete your data on "+notice || !strings.Contains(got[0].Text, "On "+notice+", the passwords") {
		t.Errorf("late warning = %q\n%s", got[0].Subject, got[0].Text)
	}
	w.at(t, j, 91.9)
	if w.secretsGone(t) || !w.teamExists(t) {
		t.Fatal("a deletion came sooner than the notice the warning gave")
	}
	w.at(t, j, 92)
	if w.teamExists(t) {
		t.Error("the team was not removed after the notice")
	}
	if n := len(w.sender.Sent()); n != 2 {
		t.Errorf("%d emails in all, want only warning 3's 2", n)
	}
}

// A warning the mail server does not take is tried again on the next tick, and the
// deletions wait for it.
func TestPlanEndFailedSendIsRetried(t *testing.T) {
	w := newPlanWorld(t)
	j := w.job(w.sender)
	w.sender.SetErr(errors.New("connection refused"))
	w.now = time.Unix(w.end, 0)
	if _, err := j.Once(t.Context()); err == nil {
		t.Fatal("a failed warning was not reported")
	}
	if p := w.plan(t); p.Warned != 0 {
		t.Fatalf("a warning nobody got was recorded: %+v", p)
	}
	w.sender.SetErr(nil)
	w.at(t, j, 0.01)
	if p := w.plan(t); p.Warned != 1 || len(w.sender.Sent()) != 2 {
		t.Errorf("after the server recovered: %+v, %d sent", p, len(w.sender.Sent()))
	}
}

func TestPlanEndWarnings(t *testing.T) {
	const end = 1_800_000_000
	date := func(days int64) string { return time.Unix(end+days*day, 0).UTC().Format("2 January 2006") }
	trial := store.PlanState{TrialEnd: end, EndedAt: end}
	paid := store.PlanState{TrialEnd: end - 100*day, EndedAt: end}
	for _, c := range []struct {
		name    string
		p       store.PlanState
		n       int
		at      int64
		subject string
		has     []string
		hasNot  []string
	}{
		{"trial, warning 1", trial, 1, end, "Your MailRules trial has ended",
			[]string{"ended on " + date(0), "stopped sorting", "read Activity, your rules and your settings",
				"On " + date(30) + ", the passwords", "On " + date(90) + ", everything else", "before " + date(90),
				"After " + date(30) + " you will need", "https://mail.example/#/plan"}, nil},
		{"paid plan, warning 1", paid, 1, end, "Your MailRules plan has ended", []string{"Your MailRules plan ended on " + date(0)}, nil},
		{"warning 2", trial, 2, end + 23*day, "MailRules will delete your mailbox passwords on " + date(30),
			[]string{"In 7 days, on " + date(30)}, nil},
		{"warning 3, secrets gone", func() store.PlanState { p := trial; p.SecretsDeleted = true; return p }(), 3, end + 83*day,
			"MailRules will delete your data on " + date(90),
			[]string{"In 7 days, on " + date(90), "were deleted already", "enter your mailbox passwords again"},
			[]string{"the passwords and sign-ins of your mailboxes and your saved AI model keys.\n"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, err := warning(c.p, c.n, time.Unix(c.at, 0), "https://mail.example/")
			if err != nil {
				t.Fatal(err)
			}
			if m.Subject != c.subject {
				t.Errorf("subject = %q, want %q", m.Subject, c.subject)
			}
			for _, s := range c.has {
				if !strings.Contains(m.Text, s) {
					t.Errorf("text lacks %q:\n%s", s, m.Text)
				}
			}
			for _, s := range c.hasNot {
				if strings.Contains(m.Text, s) {
					t.Errorf("text has %q:\n%s", s, m.Text)
				}
			}
			if !strings.Contains(m.HTML, `href="https://mail.example/#/plan"`) || !strings.Contains(m.HTML, "<title>"+c.subject+"</title>") {
				t.Errorf("html = %s", m.HTML)
			}
		})
	}
}

// A supervisor whose tenant's plan ends stops at the next email without sorting it or
// moving past it, rather than reconnecting over and over; started again with a plan, it
// sorts what arrived meanwhile.
func TestSupervisorStopsWhenThePlanEnds(t *testing.T) {
	e := newEnv(t)
	var ended atomic.Bool
	e.sup.Pipeline.Halted = func(context.Context, int64, time.Time) (bool, error) { return ended.Load(), nil }
	var opens atomic.Int32
	open := e.sup.Open
	e.sup.Open = func(ctx context.Context) (mail.Mailbox, error) { opens.Add(1); return open(ctx) }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { e.sup.Run(ctx); close(done) }()
	eventually(t, "status live", func() bool {
		a, _ := e.st.Account(t.Context(), e.sup.Account.ID)
		return a.Status == StatusLive
	})
	first := e.mb.Deliver("INBOX", eml("before the end"))
	eventually(t, "the first email sorted", func() bool { return e.state(t, first) == store.StateActed })

	ended.Store(true)
	held := e.mb.Deliver("INBOX", eml("after the end"))
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor kept running after the plan ended")
	}
	if n := opens.Load(); n != 1 {
		t.Errorf("connected %d times, want once: no reconnecting while the plan has ended", n)
	}
	if got := e.state(t, held); got != "" {
		t.Errorf("the email after the end was recorded as %q", got)
	}
	if f, _ := e.st.Folder(t.Context(), e.sup.Account.ID, "INBOX"); f.LastUID != first.UID {
		t.Errorf("last_uid = %d, want %d: the position must not pass the email held", f.LastUID, first.UID)
	}

	ended.Store(false)
	stop := e.run(t)
	defer stop()
	eventually(t, "the held email sorted after a plan is bought", func() bool { return e.state(t, held) == store.StateActed })
}
