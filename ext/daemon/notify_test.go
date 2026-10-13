package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/imap/imaptest"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/store/storetest"
)

// fakeNotifier is a module that takes notices: it refuses the first `refuse` hand-overs,
// then keeps every notice it is given and reports it sent.
type fakeNotifier struct {
	mu      sync.Mutex
	refuse  int
	calls   []int64 // the notice of every hand-over, refused or not
	got     []ext.Notice
	started bool
}

func (f *fakeNotifier) notify(ctx context.Context, n ext.Notice, d ext.Deliveries) error {
	f.mu.Lock()
	f.calls = append(f.calls, n.ID)
	if f.refuse > 0 {
		f.refuse--
		f.mu.Unlock()
		return errors.New("queue full")
	}
	f.got = append(f.got, n)
	f.mu.Unlock()
	return d.Record(ctx, ext.Delivery{NoticeID: n.ID, Sent: true})
}

func (f *fakeNotifier) notices() []ext.Notice {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.got)
}

func (f *fakeNotifier) module() ext.Module {
	return ext.Module{Name: "notifytest", Notifications: &ext.Notifications{
		Kinds:  []string{ext.DestinationWebhook, ext.DestinationSlack},
		Notify: f.notify,
		Start: func(ctx context.Context, _ ext.Deliveries) {
			f.mu.Lock()
			f.started = true
			f.mu.Unlock()
			<-ctx.Done()
		},
	}}
}

// noticeWorld is a store with a mailbox, a rule and one destination, and a way to queue
// notices the way the executor does.
type noticeWorld struct {
	db   *sql.DB
	st   *store.Store
	acct store.Account
	rule rules.Rule
	dest store.Destination
}

func newNoticeWorld(t *testing.T) noticeWorld {
	t.Helper()
	ctx := t.Context()
	st, db := storetest.New(t)
	w := noticeWorld{db: db, st: st}
	u, err := w.st.CreateFirstUser(ctx, "me@example.test", "hash", 1)
	if err != nil {
		t.Fatal(err)
	}
	if w.acct, err = w.st.CreateAccount(ctx, store.Account{UserID: u.ID, Label: "me", Preset: "generic", Host: "h", Port: 993,
		TLSMode: "implicit", Username: "me@example.test"}, "pw"); err != nil {
		t.Fatal(err)
	}
	if w.dest, err = w.st.CreateDestination(ctx, store.Destination{TenantID: u.TenantID, Name: "Ops", Kind: store.DestinationWebhook},
		store.DestinationSecret{URL: "https://hooks.test/ops", Secret: "whsec_test"}, 1); err != nil {
		t.Fatal(err)
	}
	if w.rule, err = w.st.CreateRule(ctx, u.TenantID, rules.Rule{UserID: u.ID, Name: "Receipts", Intent: "receipts", Enabled: true,
		Actions: []rules.Action{{Type: rules.ActMove, Folder: "Receipts"}}, Notify: []int64{w.dest.ID}}, 1); err != nil {
		t.Fatal(err)
	}
	return w
}

// queue records a live run of the rule on a new email, which queues its notice, at now.
func (w noticeWorld) queue(t *testing.T, uid uint32, now int64) {
	t.Helper()
	ctx := t.Context()
	m, _, err := w.st.IngestMessage(ctx, mail.MsgRef{AccountID: w.acct.ID, Folder: "INBOX", UIDValidity: 1, UID: uid}, now)
	if err != nil {
		t.Fatal(err)
	}
	m.FromAddr, m.FromName, m.Subject = "orders@shop.test", "Shop", fmt.Sprintf("Receipt %d", uid)
	if err := w.st.SaveMessageSummary(ctx, m); err != nil {
		t.Fatal(err)
	}
	dec, err := w.st.AddDecision(ctx, store.Decision{MessageID: m.ID, Stage: "condition", RuleID: w.rule.ID, RuleName: w.rule.Name, CreatedAt: now}, store.StateActed)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := w.st.LiveBatch(ctx, w.acct.TenantID, time.Unix(now, 0))
	if err != nil {
		t.Fatal(err)
	}
	a := store.Action{DecisionID: dec, BatchID: batch, MessageID: m.ID, AccountID: w.acct.ID, Kind: rules.ActMove, Folder: "Receipts",
		Before: store.Snapshot{Folder: "INBOX", UIDValidity: 1, UID: uid}, Status: store.ActionFailed, CreatedAt: now}
	if a.ID, err = w.st.InsertAction(ctx, a); err != nil {
		t.Fatal(err)
	}
	a.Status, a.After = store.ActionDone, &store.Snapshot{Folder: "Receipts", UIDValidity: 1, UID: uid}
	if err := w.st.FinishAction(ctx, a, store.FollowUps{Notices: true}); err != nil {
		t.Fatal(err)
	}
}

// The worker hands every notice over until the module takes it, waiting longer after each
// refusal; what the module reports is listed and announced.
func TestNoticeWorkerHandsOverAtLeastOnce(t *testing.T) {
	w := newNoticeWorld(t)
	ctx := t.Context()
	f := &fakeNotifier{refuse: 1}
	hub := events.NewHub()
	now := time.Unix(1_800_000_000, 0)
	nw := newNoticeWorker(w.st, f.module().Notifications, hub, "https://mail.example.test/mailrules/")
	nw.now = func() time.Time { return now }
	w.queue(t, 1, now.Unix())

	if err := nw.once(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || len(f.notices()) != 0 {
		t.Fatalf("after a refusal: calls %v, taken %v", f.calls, f.notices())
	}
	// Not again before its retry is due.
	now = now.Add(noticeRetryFirst - time.Second)
	if err := nw.once(ctx); err != nil || len(f.calls) != 1 {
		t.Fatalf("handed over again too soon: %v, %v", f.calls, err)
	}
	_, live, cancel := hub.Subscribe(0)
	defer cancel()
	now = now.Add(time.Second)
	if err := nw.once(ctx); err != nil {
		t.Fatal(err)
	}
	got := f.notices()
	if len(f.calls) != 2 || f.calls[0] != f.calls[1] || len(got) != 1 {
		t.Fatalf("calls %v, taken %+v; want the same notice handed over again and taken", f.calls, got)
	}
	n := got[0]
	want := ext.Notice{ID: f.calls[0], TenantID: w.acct.TenantID, Mailbox: "me@example.test", RuleID: w.rule.ID, RuleName: "Receipts",
		Actions: []string{"move"}, Folder: "Receipts", From: "orders@shop.test", FromName: "Shop", Subject: "Receipt 1",
		At: time.Unix(1_800_000_000, 0).UTC(), ActivityID: n.ActivityID,
		Link:        fmt.Sprintf("https://mail.example.test/mailrules/#/activity?id=%d", n.ActivityID),
		Destination: ext.Destination{ID: w.dest.ID, Name: "Ops", Kind: "webhook", URL: "https://hooks.test/ops", Secret: "whsec_test"}}
	if n.ActivityID == 0 || !equalNotice(n, want) {
		t.Errorf("notice = %+v\nwant     %+v", n, want)
	}
	// As a webhook body it carries no destination.
	body, _ := json.Marshal(n)
	if strings.Contains(string(body), "hooks.test") || strings.Contains(string(body), "whsec_") || !strings.Contains(string(body), `"rule_name":"Receipts"`) {
		t.Errorf("JSON = %s", body)
	}
	select {
	case ev := <-live:
		if ev.Name != events.NotificationsChanged || ev.TenantID != w.acct.TenantID {
			t.Errorf("event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Error("no notifications.changed after the delivery was recorded")
	}
	recent, _ := w.st.RecentDeliveries(ctx, w.acct.TenantID, 20)
	if d := recent[w.dest.ID]; len(d) != 1 || d[0].Status != store.DeliverySent {
		t.Errorf("deliveries = %+v", d)
	}
	// Taken: never handed over again.
	now = now.Add(time.Hour)
	if err := nw.once(ctx); err != nil || len(f.calls) != 2 {
		t.Errorf("handed over after it was taken: %v, %v", f.calls, err)
	}
}

func equalNotice(a, b ext.Notice) bool {
	return a.ID == b.ID && a.TenantID == b.TenantID && a.Destination == b.Destination && a.Mailbox == b.Mailbox &&
		a.RuleID == b.RuleID && a.RuleName == b.RuleName && slices.Equal(a.Actions, b.Actions) && a.Folder == b.Folder &&
		a.From == b.From && a.FromName == b.FromName && a.Subject == b.Subject && a.At.Equal(b.At) &&
		a.ActivityID == b.ActivityID && a.Link == b.Link
}

// A destination whose address was deleted with the sign-in takes no notices: each is
// listed as failed, and the module is never called.
func TestNoticeWorkerAddressDeleted(t *testing.T) {
	w := newNoticeWorld(t)
	ctx := t.Context()
	f := &fakeNotifier{}
	nw := newNoticeWorker(w.st, f.module().Notifications, events.NewHub(), "http://127.0.0.1:8080")
	w.queue(t, 1, time.Now().Unix())
	// What ForgetIdentity does to the destinations of a tenant's only member.
	if _, err := w.db.ExecContext(ctx, `UPDATE notify_destinations SET secret_enc = ?, dek_enc = ?`, []byte{}, []byte{}); err != nil {
		t.Fatal(err)
	}
	if err := nw.once(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Errorf("the module was handed %v", f.calls)
	}
	recent, _ := w.st.RecentDeliveries(ctx, w.acct.TenantID, 20)
	if d := recent[w.dest.ID]; len(d) != 1 || d[0].Status != store.DeliveryFailed || !strings.Contains(d[0].Reason, "address was deleted") {
		t.Errorf("deliveries = %+v", d)
	}
	if waiting, _ := w.st.WaitingNotices(ctx, 2_000_000_000, 10); len(waiting) != 0 {
		t.Errorf("still waiting: %+v", waiting)
	}
}

func TestDeliveriesRecord(t *testing.T) {
	w := newNoticeWorld(t)
	ctx := t.Context()
	d := deliveries{store: w.st, hub: events.NewHub(), now: func() time.Time { return time.Unix(1_800_000_500, 0) }}
	if err := d.Record(ctx, ext.Delivery{NoticeID: 42, Sent: true}); !errors.Is(err, ext.ErrNoNotice) {
		t.Errorf("a notice that is not kept: %v", err)
	}
	w.queue(t, 1, 1_800_000_000)
	waiting, _ := w.st.WaitingNotices(ctx, 2_000_000_000, 1)
	for _, r := range []ext.Delivery{
		{NoticeID: waiting[0].ID},
		{NoticeID: waiting[0].ID, Reason: "Slack answered\n404:   " + strings.Repeat("x", 400)},
		{NoticeID: waiting[0].ID, Sent: true, At: time.Unix(1_800_000_900, 0)},
	} {
		if err := d.Record(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	recent, _ := w.st.RecentDeliveries(ctx, w.acct.TenantID, 20)
	got := recent[w.dest.ID]
	if len(got) != 3 || got[0].CreatedAt != 1_800_000_900 || got[0].Status != store.DeliverySent ||
		got[1].CreatedAt != 1_800_000_500 || !strings.HasPrefix(got[1].Reason, "Slack answered 404: xxx") || len([]rune(got[1].Reason)) != maxReason ||
		got[2].Reason != "The notification module gave no reason." {
		t.Errorf("deliveries = %+v", got)
	}
}

func TestRetryAfter(t *testing.T) {
	for _, c := range []struct {
		attempts int
		want     time.Duration
	}{{0, 5 * time.Second}, {1, 10 * time.Second}, {4, 80 * time.Second}, {10, time.Hour}, {1000, time.Hour}} {
		if got := retryAfter(c.attempts); got != c.want {
			t.Errorf("retryAfter(%d) = %v, want %v", c.attempts, got, c.want)
		}
	}
}

// End to end: a webhook added in Settings and ticked on a rule receives a notice through
// the module within seconds of the rule sorting an email live, and Settings lists the
// delivery. In dry-run nothing is sent.
func TestNotificationsEndToEnd(t *testing.T) {
	srv := imaptest.Start(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapMove: {}, imap.CapUIDPlus: {}})
	if err := srv.User.Create("Receipts", nil); err != nil {
		t.Fatal(err)
	}
	oauth := &fakeOAuth{srv: srv, rotate: map[string]string{}, revoked: map[string]bool{}}
	f := &fakeNotifier{}
	d := serveForTest(t, srv.TLS, oauth.module(), f.module())
	c := d.c

	if _, body := c.do(http.MethodGet, "/api/settings", nil, http.StatusOK); !strings.Contains(string(body), `"notifications":true`) {
		t.Fatalf("features.notifications is not on: %s", body)
	}
	_, body := c.do(http.MethodPost, "/api/notifications/destinations", map[string]string{"name": "Ops", "kind": "webhook", "url": "https://hooks.test/mailrules"}, http.StatusCreated)
	var created struct {
		Destination struct{ ID int64 }
		Secret      string
	}
	if err := json.Unmarshal(body, &created); err != nil || !strings.HasPrefix(created.Secret, "whsec_") {
		t.Fatalf("created = %s, %v", body, err)
	}

	hdr, _ := c.do(http.MethodGet, "/api/oauthtest/connect?provider=gmail", nil, http.StatusSeeOther)
	var account int64
	if _, err := fmt.Sscanf(hdr.Get("Location"), "/#/accounts?added=%d", &account); err != nil {
		t.Fatalf("added: %q", hdr.Get("Location"))
	}
	eventually(t, "the mailbox goes live", func() (bool, string) { a := c.account(account); return a.Status == "live", a.Status })
	c.do(http.MethodPost, "/api/rules/batch", map[string]any{"rules": []map[string]any{{"name": "Receipts",
		"conditions": map[string]any{"field": "subject", "op": "contains_any", "value": []string{"Receipt"}},
		"actions":    []map[string]any{{"type": "move", "folder": "Receipts"}}, "notify": []int64{created.Destination.ID}}}}, http.StatusCreated)

	// Dry-run (on until the person turns it off): recorded, not done, and nobody told.
	srv.Append(t, "INBOX", receipt(1))
	eventually(t, "the first receipt is recorded", func() (bool, string) {
		_, body := c.do(http.MethodGet, "/api/activity", nil, http.StatusOK)
		return strings.Contains(string(body), "Receipt 1"), string(body)
	})
	time.Sleep(2*noticeEvery + 200*time.Millisecond)
	if got := f.notices(); len(got) != 0 {
		t.Fatalf("dry-run sent %+v", got)
	}

	c.do(http.MethodPatch, "/api/settings", map[string]any{"dry_run": false}, http.StatusOK)
	sent := time.Now()
	srv.Append(t, "INBOX", receipt(2))
	eventually(t, "the module gets the notice", func() (bool, string) { return len(f.notices()) == 1, fmt.Sprint(f.notices()) })
	if took := time.Since(sent); took > 10*time.Second {
		t.Errorf("the notice took %v", took)
	}
	n := f.notices()[0]
	if n.RuleName != "Receipts" || n.Subject != "Receipt 2" || n.From != "orders@shop.example" || n.Folder != "Receipts" ||
		!slices.Equal(n.Actions, []string{"move"}) || n.Mailbox != imaptest.Username || n.Destination.URL != "https://hooks.test/mailrules" ||
		n.Destination.Secret != created.Secret || !strings.HasSuffix(n.Link, fmt.Sprintf("/#/activity?id=%d", n.ActivityID)) {
		t.Errorf("notice = %+v", n)
	}
	eventually(t, "Settings lists the delivery", func() (bool, string) {
		_, body := c.do(http.MethodGet, "/api/notifications", nil, http.StatusOK)
		return strings.Contains(string(body), `"status":"sent"`) && strings.Contains(string(body), `"rule_name":"Receipts"`), string(body)
	})
	_, body = c.do(http.MethodGet, "/api/notifications", nil, http.StatusOK)
	if strings.Contains(string(body), "hooks.test/mailrules") || strings.Contains(string(body), created.Secret) {
		t.Errorf("the list shows the address or secret: %s", body)
	}
	f.mu.Lock()
	started := f.started
	f.mu.Unlock()
	if !started {
		t.Error("the module's Start was not called")
	}
}
