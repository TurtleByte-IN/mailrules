package summary

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/actions"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mailer"
	"github.com/TurtleByte-IN/mailrules/internal/mailer/mailertest"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/settings"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// Tue 6 Oct 2026, 08:00 UTC.
var tuesday8 = time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC)

// fixture is a store with an admin, one mailbox and two rules.
type fixture struct {
	st       *store.Store
	user     store.User
	acct     store.Account
	receipts int64 // rule ids
	scams    int64
	nextUID  uint32
}

func newFixture(t *testing.T) *fixture {
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
	f := &fixture{st: store.New(db)}
	if f.user, err = f.st.CreateFirstUser(ctx, "admin@example.com", "hash", 1); err != nil {
		t.Fatal(err)
	}
	if f.acct, err = f.st.CreateAccount(ctx, make([]byte, 32), store.Account{UserID: f.user.ID, Label: "Personal", Preset: "generic",
		Host: "imap.example.com", Port: 993, TLSMode: "implicit", Username: "me@example.com", CreatedAt: 1}, "pw"); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct {
		name string
		dst  *int64
	}{{"Receipts", &f.receipts}, {"Scams", &f.scams}} {
		saved, err := f.st.CreateRule(ctx, 1, rules.Rule{UserID: f.user.ID, Name: r.name, Actions: []rules.Action{{Type: rules.ActMove, Folder: r.name}}, Priority: 1, Enabled: true}, 1)
		if err != nil {
			t.Fatal(err)
		}
		*r.dst = saved.ID
	}
	return f
}

// email is one email as MailRules handled it.
type email struct {
	from, name, subject string
	state               string    // the message's state; "" = acted
	rule                int64     // 0 = none
	at                  time.Time // when it was decided
	cost                float64
	model               string
	act, status         string // its action's kind and status; "" = no action
	folder              string // the action's folder
}

func (f *fixture) add(t *testing.T, e email) int64 {
	t.Helper()
	ctx := t.Context()
	f.nextUID++
	ref := mail.MsgRef{AccountID: f.acct.ID, Folder: "INBOX", UIDValidity: 1, UID: f.nextUID}
	m, _, err := f.st.IngestMessage(ctx, ref, e.at.Unix())
	if err != nil {
		t.Fatal(err)
	}
	m.FromAddr, m.FromName, m.Subject = e.from, e.name, e.subject
	if err := f.st.SaveMessageSummary(ctx, m); err != nil {
		t.Fatal(err)
	}
	state := e.state
	if state == "" {
		state = store.StateActed
	}
	stage := "condition"
	if e.model != "" {
		stage = "decider"
	}
	d, err := f.st.AddDecision(ctx, store.Decision{MessageID: m.ID, Stage: stage, RuleID: e.rule, Model: e.model, CostUSD: e.cost, CreatedAt: e.at.Unix()}, state)
	if err != nil {
		t.Fatal(err)
	}
	if e.act != "" {
		if _, err := f.st.InsertAction(ctx, store.Action{DecisionID: d, MessageID: m.ID, AccountID: f.acct.ID, Kind: e.act, Folder: e.folder,
			Status: e.status, CreatedAt: e.at.Unix(), Before: store.Snapshot{Folder: "INBOX", UIDValidity: 1, UID: f.nextUID}}); err != nil {
			t.Fatal(err)
		}
	}
	return m.ID
}

func TestBuild(t *testing.T) {
	in := tuesday8.Add(-2 * time.Hour) // inside the day before tuesday8
	before := tuesday8.Add(-30 * time.Hour)
	tests := []struct {
		name        string
		emails      func(f *fixture) []email
		wantRules   string // "name:count" joined by ", "
		wantTrashed string // "sender|subject|destination|dry" joined by ", "
		wantReview  string // "sender|subject" joined by ", "
		wantSorted  int
		wantCost    float64
		wantCalls   int
	}{
		{name: "nothing happened", emails: func(*fixture) []email { return nil }},
		{name: "sorted per rule, most first", emails: func(f *fixture) []email {
			return []email{
				{from: "a@shop.example", subject: "Receipt 1", rule: f.receipts, at: in, act: "move", status: store.ActionDone, folder: "Receipts"},
				{from: "b@shop.example", subject: "Receipt 2", rule: f.receipts, at: in, act: "move", status: store.ActionDone, folder: "Receipts"},
				{from: "c@bad.example", name: "HDFC Bank", subject: "Account locked", rule: f.scams, at: in, act: "trash", status: store.ActionDone, folder: actions.TrashFolder},
			}
		}, wantRules: "Receipts:2, Scams:1", wantTrashed: "HDFC Bank|Account locked|MailRules Trash|false", wantSorted: 3},
		{name: "outside the period is left out", emails: func(f *fixture) []email {
			return []email{
				{from: "a@shop.example", subject: "Old receipt", rule: f.receipts, at: before, act: "move", status: store.ActionDone, folder: "Receipts"},
				{from: "c@bad.example", subject: "Old scam", rule: f.scams, at: before, act: "trash", status: store.ActionDone},
			}
		}},
		{name: "an undone action does not count", emails: func(f *fixture) []email {
			return []email{
				{from: "a@shop.example", subject: "Receipt", rule: f.receipts, at: in, act: "move", status: store.ActionUndone, folder: "Receipts"},
				{from: "c@bad.example", subject: "Not a scam", rule: f.scams, at: in, act: "trash", status: store.ActionUndone},
			}
		}},
		{name: "dry-run is reported as what would have happened", emails: func(f *fixture) []email {
			return []email{
				{from: "a@shop.example", subject: "Receipt", rule: f.receipts, at: in, act: "move", status: store.ActionDryRun, folder: "Receipts"},
				{from: "c@bad.example", subject: "Scam", rule: f.scams, at: in, act: "trash", status: store.ActionDryRun},
			}
		}, wantRules: "Receipts:1, Scams:1", wantTrashed: "c@bad.example|Scam|Trash|true", wantSorted: 2},
		{name: "Needs review lists what waits now, whenever it arrived", emails: func(f *fixture) []email {
			return []email{
				{from: "jobs@alerts.example", name: "JobAlerts", subject: "12 new jobs", state: store.StateReview, at: before},
				{from: "x@cloud.example", subject: "", state: store.StateReview, at: in},
			}
		}, wantReview: "x@cloud.example|(no subject), JobAlerts|12 new jobs"},
		{name: "model cost of the period", emails: func(f *fixture) []email {
			return []email{
				{from: "a@shop.example", subject: "R", rule: f.receipts, at: in, model: "jev-1", cost: 0.004, act: "move", status: store.ActionDone, folder: "Receipts"},
				{from: "b@shop.example", subject: "R", rule: f.receipts, at: in, model: "jev-1", cost: 0.003, act: "move", status: store.ActionDone, folder: "Receipts"},
				{from: "c@shop.example", subject: "R", rule: f.receipts, at: before, model: "jev-1", cost: 1, act: "move", status: store.ActionDone, folder: "Receipts"},
			}
		}, wantRules: "Receipts:2", wantSorted: 2, wantCost: 0.007, wantCalls: 2},
		{name: "left in the inbox counts nowhere", emails: func(f *fixture) []email {
			return []email{{from: "a@x.example", subject: "Hi", state: store.StateSkipped, at: in}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			for _, e := range tt.emails(f) {
				f.add(t, e)
			}
			c, err := build(t.Context(), f.st, f.user.Viewer(), tuesday8.Add(-24*time.Hour), tuesday8, false)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, r := range c.Rules {
				got = append(got, r.RuleName+":"+strconv.Itoa(r.Emails))
			}
			if s := strings.Join(got, ", "); s != tt.wantRules {
				t.Errorf("rules = %q, want %q", s, tt.wantRules)
			}
			got = nil
			for _, e := range c.Trashed {
				got = append(got, sender(e)+"|"+subject(e)+"|"+destination(e)+"|"+map[bool]string{true: "true", false: "false"}[e.DryRun])
			}
			if s := strings.Join(got, ", "); s != tt.wantTrashed {
				t.Errorf("trashed = %q, want %q", s, tt.wantTrashed)
			}
			got = nil
			for _, e := range c.Review {
				got = append(got, sender(e)+"|"+subject(e))
			}
			if s := strings.Join(got, ", "); s != tt.wantReview {
				t.Errorf("review = %q, want %q", s, tt.wantReview)
			}
			if c.Sorted != tt.wantSorted || c.TrashedTotal != len(c.Trashed) || c.ReviewTotal != len(c.Review) {
				t.Errorf("sorted %d, trashed %d of %d, review %d of %d; want sorted %d", c.Sorted, len(c.Trashed), c.TrashedTotal, len(c.Review), c.ReviewTotal, tt.wantSorted)
			}
			if c.ModelCalls != tt.wantCalls || c.CostUSD < tt.wantCost-1e-9 || c.CostUSD > tt.wantCost+1e-9 {
				t.Errorf("cost %v for %d calls, want %v for %d", c.CostUSD, c.ModelCalls, tt.wantCost, tt.wantCalls)
			}
		})
	}
}

func TestBuildCapsTheLists(t *testing.T) {
	f := newFixture(t)
	in := tuesday8.Add(-time.Hour)
	for range maxTrashed + 3 {
		f.add(t, email{from: "s@bad.example", subject: "Scam", rule: f.scams, at: in, act: "trash", status: store.ActionDone})
	}
	for range maxReview + 2 {
		f.add(t, email{from: "r@x.example", subject: "Which?", state: store.StateReview, at: in})
	}
	c, err := build(t.Context(), f.st, f.user.Viewer(), tuesday8.Add(-24*time.Hour), tuesday8, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Trashed) != maxTrashed || c.TrashedTotal != maxTrashed+3 || len(c.Review) != maxReview || c.ReviewTotal != maxReview+2 {
		t.Fatalf("trashed %d of %d, review %d of %d", len(c.Trashed), c.TrashedTotal, len(c.Review), c.ReviewTotal)
	}
	e, err := render(c, "https://mail.example.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"- and 3 more", "- and 2 more", "Everything trashed: https://mail.example.com/#/activity?outcome=trashed"} {
		if !strings.Contains(e.Text, want) {
			t.Errorf("text lacks %q", want)
		}
	}
}

func TestRender(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	full := Content{
		Start: tuesday8.Add(-24 * time.Hour).In(berlin), End: tuesday8.In(berlin),
		Rules:   []store.RuleUse{{RuleID: 3, RuleName: "Receipts", Emails: 40}, {RuleName: "Old rule", Emails: 3}},
		Sorted:  43,
		Trashed: []store.SummaryEmail{{MessageID: 77, FromName: "HDFC <Bank>", FromAddr: "x@bad.example", Subject: "Locked\r\nBcc: you", Folder: actions.TrashFolder}}, TrashedTotal: 1,
		Review: []store.SummaryEmail{{MessageID: 9, FromAddr: "jobs@alerts.example", Subject: "<script>alert(1)</script>"}}, ReviewTotal: 3,
		CostUSD: 0.004, ModelCalls: 2,
	}
	tests := []struct {
		name        string
		c           Content
		wantSubject string
		wantText    []string
		wantHTML    []string
		notInHTML   []string
	}{
		{"a full day", full, "MailRules: 43 sorted, 3 to review",
			[]string{"MailRules summary, Mon 5 Oct 10:00 to Tue 6 Oct 10:00 (Europe/Berlin)", "Needs review (3)", "- jobs@alerts.example: <script>alert(1)</script>",
				"- and 2 more", "Review them: https://mail.example.com/#/review", "- Receipts: 40", "- Old rule: 3",
				"- HDFC <Bank>: Locked Bcc: you, went to MailRules Trash", "Wrong? Restore it: https://mail.example.com/#/activity?id=77",
				"Model cost: under $0.01 for 2 model decisions.", "https://mail.example.com/#/settings"},
			[]string{`href="https://mail.example.com/#/rules?id=3"`, `href="https://mail.example.com/#/activity" style="color:#1a4fd6;">Old rule`, `href="https://mail.example.com/#/activity?id=77"`,
				"&lt;script&gt;alert(1)&lt;/script&gt;", "HDFC &lt;Bank&gt;", "background:#ffffff"},
			[]string{"<script>", "Dry-run is on"}},
		{"a quiet day", Content{Start: tuesday8.Add(-24 * time.Hour), End: tuesday8}, "MailRules: 0 sorted, nothing to review",
			[]string{"Nothing is waiting for you.", "No rule was applied.", "Nothing was trashed.", "Model cost: none, no model was asked."},
			[]string{"Nothing is waiting for you."}, []string{"Review them"}},
		{"dry-run", Content{Start: tuesday8.Add(-24 * time.Hour), End: tuesday8, DryRun: true, Sorted: 5, Rules: []store.RuleUse{{RuleID: 1, RuleName: "R", Emails: 5}},
			Trashed: []store.SummaryEmail{{MessageID: 1, FromAddr: "a@b.example", Subject: "S", DryRun: true}}, TrashedTotal: 1},
			"MailRules dry-run: 5 would be sorted, nothing to review",
			[]string{dryRunNote, "- a@b.example: S, would have gone to Trash"}, []string{dryRunNote, "would have gone to"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, err := render(tt.c, "https://mail.example.com/")
			if err != nil {
				t.Fatal(err)
			}
			if e.Subject != tt.wantSubject {
				t.Errorf("subject %q, want %q", e.Subject, tt.wantSubject)
			}
			for _, w := range tt.wantText {
				if !strings.Contains(e.Text, w) {
					t.Errorf("text lacks %q:\n%s", w, e.Text)
				}
			}
			for _, w := range tt.wantHTML {
				if !strings.Contains(e.HTML, w) {
					t.Errorf("HTML lacks %q", w)
				}
			}
			for _, w := range tt.notInHTML {
				if strings.Contains(e.HTML, w) {
					t.Errorf("HTML holds %q", w)
				}
			}
		})
	}
}

func TestSlots(t *testing.T) {
	at := func(zone string, y int, mo time.Month, d, h, mi int) time.Time {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatal(err)
		}
		return time.Date(y, mo, d, h, mi, 0, 0, loc)
	}
	tests := []struct {
		name      string
		s         Settings
		now       time.Time
		due, next time.Time
	}{
		{"daily, just after", Settings{Frequency: Daily, Time: "08:00", TimeZone: "UTC"}, tuesday8.Add(time.Minute), tuesday8, tuesday8.AddDate(0, 0, 1)},
		{"daily, on the minute", Settings{Frequency: Daily, Time: "08:00", TimeZone: "UTC"}, tuesday8, tuesday8, tuesday8.AddDate(0, 0, 1)},
		{"daily, just before", Settings{Frequency: Daily, Time: "08:00", TimeZone: "UTC"}, tuesday8.Add(-time.Minute), tuesday8.AddDate(0, 0, -1), tuesday8},
		{"daily in the user's zone", Settings{Frequency: Daily, Time: "07:30", TimeZone: "Asia/Kolkata"}, tuesday8, // 13:30 in Kolkata
			at("Asia/Kolkata", 2026, 10, 6, 7, 30), at("Asia/Kolkata", 2026, 10, 7, 7, 30)},
		{"weekly on its day", Settings{Frequency: Weekly, Weekday: "tuesday", Time: "08:00", TimeZone: "UTC"}, tuesday8.Add(time.Hour), tuesday8, tuesday8.AddDate(0, 0, 7)},
		{"weekly, another day", Settings{Frequency: Weekly, Weekday: "monday", Time: "09:00", TimeZone: "UTC"}, tuesday8,
			time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC), time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)},
		{"weekly, its day but before its time", Settings{Frequency: Weekly, Weekday: "tuesday", Time: "09:00", TimeZone: "UTC"}, tuesday8,
			time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC), time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)},
		{"across the clocks going forward", Settings{Frequency: Daily, Time: "08:00", TimeZone: "America/New_York"}, at("America/New_York", 2026, 3, 8, 12, 0),
			at("America/New_York", 2026, 3, 8, 8, 0), at("America/New_York", 2026, 3, 9, 8, 0)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			due, next, err := tt.s.slots(tt.now)
			if err != nil {
				t.Fatal(err)
			}
			if !due.Equal(tt.due) || !next.Equal(tt.next) {
				t.Errorf("slots(%v) = %v, %v; want %v, %v", tt.now, due, next, tt.due, tt.next)
			}
		})
	}
	if _, _, err := (Settings{Frequency: Daily, Time: "08:00", TimeZone: "Mars/Olympus"}).slots(tuesday8); err == nil {
		t.Error("an unknown zone gave slots")
	}
}

func TestApply(t *testing.T) {
	ptr := func(s string) *string { return &s }
	on, off := true, false
	cur := defaults()
	tests := []struct {
		name     string
		cur      Settings
		p        Patch
		missing  []string
		wantPath string // "" = valid
		noSMTP   bool
		check    func(Settings) bool
	}{
		{"weekly on friday at 18:30 in Berlin", cur, Patch{Frequency: ptr("weekly"), Weekday: ptr("friday"), Time: ptr("18:30"), TimeZone: ptr("Europe/Berlin")}, nil, "", false,
			func(s Settings) bool {
				return s.Frequency == Weekly && s.Weekday == "friday" && s.Time == "18:30" && s.TimeZone == "Europe/Berlin"
			}},
		{"unknown frequency", cur, Patch{Frequency: ptr("hourly")}, nil, "summary.frequency", false, nil},
		{"weekday in another language", cur, Patch{Weekday: ptr("lundi")}, nil, "summary.weekday", false, nil},
		{"12-hour time", cur, Patch{Time: ptr("8:00 AM")}, nil, "summary.time", false, nil},
		{"24:00", cur, Patch{Time: ptr("24:00")}, nil, "summary.time", false, nil},
		{"unknown zone", cur, Patch{TimeZone: ptr("Europe/Atlantis")}, nil, "summary.time_zone", false, nil},
		{"Local is not a zone", cur, Patch{TimeZone: ptr("Local")}, nil, "summary.time_zone", false, nil},
		{"an address", cur, Patch{To: Optional{Set: true, Value: " Neha <neha@example.com> "}}, nil, "", false, func(s Settings) bool { return s.To == "neha@example.com" }},
		{"two addresses", cur, Patch{To: Optional{Set: true, Value: "a@example.com, b@example.com"}}, nil, "summary.to", false, nil},
		{"a header smuggled in", cur, Patch{To: Optional{Set: true, Value: "a@example.com\r\nBcc: b@example.com"}}, nil, "summary.to", false, nil},
		{"null puts the admin's back", Settings{To: "x@example.com"}, Patch{To: Optional{Set: true}}, nil, "", false, func(s Settings) bool { return s.To == "" }},
		{"switched on", cur, Patch{Enabled: &on}, nil, "", false, func(s Settings) bool { return s.Enabled && s.EnabledAt == tuesday8.Unix() }},
		{"switched on without a mail server", cur, Patch{Enabled: &on}, []string{"MAILRULES_SMTP_HOST"}, "", true, nil},
		{"switched off without a mail server", Settings{Enabled: true}, Patch{Enabled: &off}, []string{"MAILRULES_SMTP_HOST"}, "", false, func(s Settings) bool { return !s.Enabled }},
		{"left on keeps when it was switched on", Settings{Enabled: true, EnabledAt: 5}, Patch{Enabled: &on}, nil, "", false, func(s Settings) bool { return s.EnabledAt == 5 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := apply(tt.cur, tt.p, tt.missing, tuesday8)
			var bad *settings.Invalid
			var noSMTP *NoSMTP
			switch {
			case tt.noSMTP:
				if !errors.As(err, &noSMTP) || !strings.Contains(noSMTP.Message(), "MAILRULES_SMTP_HOST") {
					t.Fatalf("err = %v, want NoSMTP naming the setting", err)
				}
			case tt.wantPath != "":
				if !errors.As(err, &bad) || bad.Path != tt.wantPath || !strings.HasSuffix(bad.Message, ".") {
					t.Fatalf("err = %v, want Invalid at %s", err, tt.wantPath)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				if !tt.check(got) {
					t.Errorf("settings = %+v", got)
				}
			}
		})
	}
}

// clockAt is a settable clock.
type clockAt struct{ t time.Time }

func (c *clockAt) now() time.Time { return c.t }

func newService(f *fixture, sender mailer.Sender, ck *clockAt) *Service {
	return &Service{Store: f.st, Sender: sender, PublicURL: "http://127.0.0.1:8080", Now: ck.now}
}

func (f *fixture) switchOn(t *testing.T, s Settings) {
	t.Helper()
	if err := save(t.Context(), f.st, 1, s); err != nil {
		t.Fatal(err)
	}
}

var dailyAt8 = Settings{Enabled: true, Frequency: Daily, Weekday: "monday", Time: "08:00", TimeZone: "UTC", EnabledAt: 1}

func TestTickSendsWhenDue(t *testing.T) {
	tests := []struct {
		name  string
		s     Settings
		now   time.Time
		ready bool
		want  int
	}{
		{"due", dailyAt8, tuesday8, true, 1},
		{"due a few minutes ago", dailyAt8, tuesday8.Add(3 * time.Minute), true, 1},
		{"not yet", dailyAt8, tuesday8.Add(-time.Minute), true, 0},
		{"switched off", Settings{Frequency: Daily, Time: "08:00", TimeZone: "UTC"}, tuesday8, true, 0},
		{"too late after a long stop", dailyAt8, tuesday8.Add(late + time.Minute), true, 0},
		{"switched on after it was due", Settings{Enabled: true, Frequency: Daily, Time: "08:00", TimeZone: "UTC", EnabledAt: tuesday8.Add(time.Hour).Unix()}, tuesday8.Add(2 * time.Hour), true, 0},
		{"no mail server", dailyAt8, tuesday8, false, 0},
		{"weekly, not its day", Settings{Enabled: true, Frequency: Weekly, Weekday: "monday", Time: "08:00", TimeZone: "UTC", EnabledAt: 1}, tuesday8, true, 0},
		{"in the user's zone", Settings{Enabled: true, Frequency: Daily, Time: "10:00", TimeZone: "Europe/Berlin", EnabledAt: 1}, tuesday8, true, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.switchOn(t, tt.s)
			fake := &mailertest.Sender{}
			svc := newService(f, fake, &clockAt{tt.now})
			if !tt.ready {
				svc.Missing = []string{"MAILRULES_SMTP_HOST"}
			}
			if err := svc.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := len(fake.Sent()); got != tt.want {
				t.Fatalf("sent %d, want %d", got, tt.want)
			}
			if tt.want == 1 && fake.Sent()[0].To != "admin@example.com" {
				t.Errorf("sent to %q, want the admin's address", fake.Sent()[0].To)
			}
		})
	}
}

// A restart, or a second loop, finds the summary sent and does not send it again; the next
// day's covers the time since.
func TestTickNeverSendsTwice(t *testing.T) {
	f := newFixture(t)
	f.switchOn(t, dailyAt8)
	f.add(t, email{from: "a@shop.example", subject: "R", rule: f.receipts, at: tuesday8.Add(-time.Hour), act: "move", status: store.ActionDone, folder: "Receipts"})
	fake := &mailertest.Sender{}
	ck := &clockAt{tuesday8.Add(time.Minute)}
	if err := newService(f, fake, ck).Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Two loops at once, as after a restart that overlaps the old process's last tick.
	done := make(chan error, 2)
	for range 2 {
		go func() { done <- newService(f, fake, ck).Tick(t.Context()) }()
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	ck.t = tuesday8.Add(2 * time.Hour)
	if err := newService(f, fake, ck).Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.Sent()); n != 1 {
		t.Fatalf("sent %d, want 1", n)
	}
	if s := fake.Sent()[0].Subject; s != "MailRules: 1 sorted, nothing to review" {
		t.Errorf("subject %q", s)
	}
	last, err := f.st.LastSentSummary(t.Context(), f.user.ID)
	if err != nil || last.PeriodEnd != tuesday8.Add(time.Minute).Unix() || last.PeriodStart != tuesday8.Add(time.Minute-24*time.Hour).Unix() {
		t.Fatalf("last summary = %+v, %v", last, err)
	}

	// The next day: it covers from where the last one ended.
	f.add(t, email{from: "b@shop.example", subject: "R", rule: f.receipts, at: tuesday8.Add(time.Hour), act: "move", status: store.ActionDone, folder: "Receipts"})
	ck.t = tuesday8.Add(24 * time.Hour)
	if err := newService(f, fake, ck).Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.Sent()); n != 2 {
		t.Fatalf("sent %d, want 2", n)
	}
	if s := fake.Sent()[1].Subject; s != "MailRules: 1 sorted, nothing to review" {
		t.Errorf("the next day's subject %q: it must count only what came after the last one", s)
	}
}

// A send that fails is tried again after a minute, then after two, four and eight, five
// times in all; one that gets through on a retry is the only one.
func TestTickRetries(t *testing.T) {
	f := newFixture(t)
	f.switchOn(t, dailyAt8)
	fake := &mailertest.Sender{Err: errors.New("421 try later")}
	ck := &clockAt{tuesday8}
	svc := newService(f, fake, ck)
	tick := func(at time.Duration) {
		t.Helper()
		ck.t = tuesday8.Add(at)
		if err := svc.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	for _, at := range []time.Duration{0, 30 * time.Second, time.Minute, 2 * time.Minute, 3 * time.Minute, 5 * time.Minute, 7 * time.Minute} {
		tick(at)
	}
	// Attempts at 0, 1m, 3m and 7m; 30s, 2m and 5m are inside the back-off.
	if n := fake.Tries(); n != 4 {
		t.Fatalf("tried %d times, want 4", n)
	}
	tick(15 * time.Minute) // the fifth and last
	tick(40 * time.Minute) // no more
	if n := fake.Tries(); n != maxAttempts {
		t.Fatalf("tried %d times, want %d", n, maxAttempts)
	}

	// Another day, the server is back on the second try.
	ck.t = tuesday8.AddDate(0, 0, 1)
	tick(24 * time.Hour)
	fake.SetErr(nil)
	tick(24*time.Hour + time.Minute)
	tick(24*time.Hour + 5*time.Minute)
	if n := len(fake.Sent()); n != 1 {
		t.Fatalf("sent %d, want 1", n)
	}
}

// An attempt cut off by a stop may have gone out, so it is not tried again.
func TestTickLeavesACutOffSend(t *testing.T) {
	f := newFixture(t)
	f.switchOn(t, dailyAt8)
	if _, _, ok, err := f.st.ClaimSummary(t.Context(), f.user.ID, tuesday8.Unix(), tuesday8.Unix(), maxAttempts); err != nil || !ok {
		t.Fatal(ok, err)
	}
	fake := &mailertest.Sender{}
	if err := newService(f, fake, &clockAt{tuesday8.Add(30 * time.Minute)}).Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := fake.Tries(); n != 0 {
		t.Fatalf("tried %d times", n)
	}
}

// Moving the time after today's went out does not send a second one today.
func TestTickAfterTheScheduleMoved(t *testing.T) {
	f := newFixture(t)
	f.switchOn(t, dailyAt8)
	fake := &mailertest.Sender{}
	ck := &clockAt{tuesday8}
	if err := newService(f, fake, ck).Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	moved := dailyAt8
	moved.Time = "12:00"
	f.switchOn(t, moved)
	ck.t = tuesday8.Add(4 * time.Hour)
	if err := newService(f, fake, ck).Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	ck.t = tuesday8.Add(28 * time.Hour) // Wednesday 12:00
	if err := newService(f, fake, ck).Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.Sent()); n != 2 {
		t.Fatalf("sent %d, want 2: Tuesday 08:00 and Wednesday 12:00", n)
	}
}

func TestSendTest(t *testing.T) {
	f := newFixture(t)
	f.add(t, email{from: "a@shop.example", subject: "R", rule: f.receipts, at: tuesday8.Add(-time.Hour), act: "move", status: store.ActionDone, folder: "Receipts"})
	f.add(t, email{from: "b@shop.example", subject: "R", rule: f.receipts, at: tuesday8.Add(-25 * time.Hour), act: "move", status: store.ActionDone, folder: "Receipts"})
	f.switchOn(t, Settings{Frequency: Weekly, Weekday: "monday", Time: "08:00", TimeZone: "UTC", To: "neha@example.com"})
	fake := &mailertest.Sender{}
	svc := newService(f, fake, &clockAt{tuesday8})

	svc.Missing = []string{"MAILRULES_SMTP_HOST", "MAILRULES_SMTP_FROM"}
	if _, _, err := svc.SendTest(t.Context(), f.user); !errors.Is(err, ErrNoSMTP) {
		t.Fatalf("without a mail server: %v", err)
	}
	svc.Missing = nil
	email, to, err := svc.SendTest(t.Context(), f.user)
	if err != nil {
		t.Fatal(err)
	}
	if to != "neha@example.com" || email.Subject != "MailRules: 1 sorted, nothing to review" || len(fake.Sent()) != 1 {
		t.Errorf("sent %q to %q; %d messages", email.Subject, to, len(fake.Sent()))
	}
	if _, err := f.st.LastSentSummary(t.Context(), f.user.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a test was recorded as a scheduled summary: %v", err)
	}
	fake.SetErr(mailer.ErrAuth)
	var notSent *SendError
	if _, _, err := svc.SendTest(t.Context(), f.user); !errors.As(err, &notSent) || !errors.Is(err, mailer.ErrAuth) {
		t.Errorf("a refused send: %v", err)
	}
}

func TestPreview(t *testing.T) {
	f := newFixture(t)
	f.switchOn(t, dailyAt8)
	fake := &mailertest.Sender{}
	ck := &clockAt{tuesday8}
	svc := newService(f, fake, ck)
	if err := svc.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	ck.t = tuesday8.Add(5 * time.Hour)
	_, c, to, err := svc.Preview(t.Context(), f.user)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Start.Equal(tuesday8) || !c.End.Equal(ck.t) || to != "admin@example.com" {
		t.Errorf("preview covers %v to %v for %q; want from the last summary to now", c.Start, c.End, to)
	}
	if len(fake.Sent()) != 1 {
		t.Error("the preview sent something")
	}
}

func TestView(t *testing.T) {
	f := newFixture(t)
	f.switchOn(t, dailyAt8)
	svc := newService(f, &mailertest.Sender{}, &clockAt{tuesday8.Add(time.Hour)})
	v, err := svc.View(t.Context(), f.user)
	if err != nil {
		t.Fatal(err)
	}
	if v.To != "admin@example.com" || v.ToDefault != "admin@example.com" || !v.Configured || len(v.Missing) != 0 || v.NextAt != tuesday8.AddDate(0, 0, 1).Unix() || v.LastSentAt != 0 {
		t.Errorf("view = %+v", v)
	}
	svc.Sender, svc.Missing = nil, []string{"MAILRULES_SMTP_HOST"}
	if v, _ = svc.View(t.Context(), f.user); v.Configured || v.NextAt != 0 || strings.Join(v.Missing, ",") != "MAILRULES_SMTP_HOST" {
		t.Errorf("without a mail server: %+v", v)
	}
}

func TestPrepareStoresNothingUntilCommit(t *testing.T) {
	f := newFixture(t)
	svc := newService(f, &mailertest.Sender{}, &clockAt{tuesday8})
	on := true
	commit, err := svc.Prepare(t.Context(), 1, Patch{Enabled: &on})
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := load(t.Context(), f.st, 1); s.Enabled {
		t.Fatal("stored before commit")
	}
	if err := commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s, _ := load(context.Background(), f.st, 1); !s.Enabled || s.EnabledAt != tuesday8.Unix() {
		t.Fatalf("after commit: %+v", s)
	}
}
