package store

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
)

func TestURLHint(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"https://hooks.slack.com/services/T000/B000/XXXXbX9z", "https://hooks.slack.com/…bX9z"},
		{"https://example.com/mailrules", "https://example.com/…ules"},
		{"http://10.0.0.5:8080/", "http://10.0.0.5:8080"},
		{"https://example.com", "https://example.com"},
		{"https://example.com?token=abcd1234", "https://example.com/…1234"},
		{"not a url", "…"},
	} {
		if got := URLHint(tt.in); got != tt.want {
			t.Errorf("URLHint(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// A destination's address and secret are sealed: the database holds neither in clear, and
// only DestinationSecret opens them.
func TestDestinations(t *testing.T) {
	s, db := open(t)
	ctx := t.Context()
	const url, secret = "https://hooks.slack.com/services/T000/B000/topsecretXYZ9", "whsec_abc"
	d, err := s.CreateDestination(ctx, testMaster, Destination{TenantID: SelfHostTenant, Name: "Team", Kind: DestinationSlack},
		DestinationSecret{URL: url, Secret: secret}, 100)
	if err != nil {
		t.Fatal(err)
	}
	want := Destination{ID: d.ID, TenantID: SelfHostTenant, Name: "Team", Kind: DestinationSlack, URLHint: "https://hooks.slack.com/…XYZ9", CreatedAt: 100}
	if d != want {
		t.Errorf("created %+v, want %+v", d, want)
	}
	if ds, err := s.Destinations(ctx, SelfHostTenant); err != nil || len(ds) != 1 || ds[0] != want {
		t.Errorf("Destinations = %+v, %v", ds, err)
	}
	if got, err := s.Destination(ctx, d.ID); err != nil || got != want {
		t.Errorf("Destination = %+v, %v", got, err)
	}
	if _, err := s.Destination(ctx, d.ID+1); !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing destination: %v", err)
	}
	if sec, err := s.DestinationSecret(ctx, testMaster, d.ID); err != nil || sec.URL != url || sec.Secret != secret {
		t.Errorf("DestinationSecret = %+v, %v", sec, err)
	}
	if _, err := s.DestinationSecret(ctx, testMaster, d.ID+1); !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing destination's secret: %v", err)
	}
	if _, err := s.DestinationSecret(ctx, make([]byte, 32), d.ID); err == nil {
		t.Error("opened with the wrong master key")
	}
	var dump string
	if err := db.QueryRowContext(ctx, `SELECT name || kind || url_hint || hex(secret_enc) || hex(dek_enc) FROM notify_destinations`).Scan(&dump); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dump, "topsecret") || strings.Contains(strings.ToUpper(dump), strings.ToUpper(fmt.Sprintf("%x", "topsecret"))) {
		t.Error("the address is stored in clear")
	}
}

// Removing a destination takes it off every rule that named it, as an edit, and its log
// and waiting notices go with it.
func TestDeleteDestination(t *testing.T) {
	s, db := open(t)
	ctx := t.Context()
	u, err := s.CreateFirstUser(ctx, "me@example.test", "hash", 1)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateAccount(ctx, testMaster, Account{UserID: u.ID, Label: "me", Preset: "generic", Host: "h", Port: 993, TLSMode: "implicit", Username: "me@example.test"}, "pw")
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, name := range []string{"A", "B"} {
		d, err := s.CreateDestination(ctx, testMaster, Destination{TenantID: u.TenantID, Name: name, Kind: DestinationWebhook}, DestinationSecret{URL: "https://x.test/" + name}, 1)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, d.ID)
	}
	both, err := s.CreateRule(ctx, u.TenantID, rules.Rule{UserID: u.ID, Name: "Both", Intent: "x", Enabled: true,
		Actions: []rules.Action{{Type: rules.ActKeep}}, Notify: ids}, 1)
	if err != nil {
		t.Fatal(err)
	}
	onlyB, err := s.CreateRule(ctx, u.TenantID, rules.Rule{UserID: u.ID, Name: "Only B", Intent: "y", Enabled: true,
		Actions: []rules.Action{{Type: rules.ActKeep}}, Notify: ids[1:]}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Rule(ctx, u.TenantID, both.ID); !slices.Equal(got.Notify, ids) {
		t.Fatalf("saved notify = %v, want %v", got.Notify, ids)
	}
	m, _, err := s.IngestMessage(ctx, mail.MsgRef{AccountID: a.ID, Folder: "INBOX", UIDValidity: 1, UID: 1}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO notices (tenant_id, destination_id, account_id, message_id, rule_name, created_at, next_at)
		VALUES (?, ?, ?, ?, 'Both', 1, 1)`, u.TenantID, ids[0], a.ID, m.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := s.DeleteDestination(ctx, u.TenantID+1, ids[0], 5); !errors.Is(err, ErrNotFound) {
		t.Errorf("another tenant's deletion: %v", err)
	}
	changed, err := s.DeleteDestination(ctx, u.TenantID, ids[0], 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0].ID != both.ID || !slices.Equal(changed[0].Notify, ids[1:]) || changed[0].Version != 2 || changed[0].UpdatedAt != 5 {
		t.Errorf("rules changed = %+v", changed)
	}
	if got, _ := s.Rule(ctx, u.TenantID, both.ID); !slices.Equal(got.Notify, ids[1:]) || got.Version != 2 {
		t.Errorf("the rule as saved = %+v", got)
	}
	if got, _ := s.Rule(ctx, u.TenantID, onlyB.ID); !slices.Equal(got.Notify, ids[1:]) || got.Version != 1 {
		t.Errorf("a rule not naming it = %+v", got)
	}
	if waiting, _ := s.WaitingNotices(ctx, 10, 10); len(waiting) != 0 {
		t.Errorf("notices left for the removed destination: %+v", waiting)
	}
	if _, err := s.DeleteDestination(ctx, u.TenantID, ids[0], 5); !errors.Is(err, ErrNotFound) {
		t.Errorf("a second deletion: %v", err)
	}
}

// The outbox: waiting notices come out oldest first and due; one handed over is not handed
// again and keeps no email details; a refused one waits until its retry; deliveries are
// kept per destination, newest first, and old ones go with the retention job.
func TestNoticeLifecycle(t *testing.T) {
	s, db := open(t)
	ctx := t.Context()
	u, _ := s.CreateFirstUser(ctx, "me@example.test", "hash", 1)
	a, err := s.CreateAccount(ctx, testMaster, Account{UserID: u.ID, Label: "me", Preset: "generic", Host: "h", Port: 993, TLSMode: "implicit", Username: "me@example.test"}, "pw")
	if err != nil {
		t.Fatal(err)
	}
	d, _ := s.CreateDestination(ctx, testMaster, Destination{TenantID: u.TenantID, Name: "A", Kind: DestinationWebhook}, DestinationSecret{URL: "https://x.test/a"}, 1)
	other, _ := s.CreateDestination(ctx, testMaster, Destination{TenantID: u.TenantID, Name: "B", Kind: DestinationWebhook}, DestinationSecret{URL: "https://x.test/b"}, 1)
	notice := func(dest, at int64) int64 {
		t.Helper()
		res, err := db.ExecContext(ctx, `INSERT INTO notices (tenant_id, destination_id, account_id, message_id, rule_id, rule_name, payload, created_at, next_at)
			VALUES (?, ?, ?, 7, NULL, 'Receipts', '{"mailbox":"me@example.test","actions":["move"],"folder":"Receipts","from":"a@b.test","subject":"Hi"}', ?, ?)`,
			u.TenantID, dest, a.ID, at, at)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	first, second, later := notice(d.ID, 100), notice(d.ID, 200), notice(other.ID, 900)

	waiting, err := s.WaitingNotices(ctx, 500, 10)
	if err != nil || len(waiting) != 2 || waiting[0].ID != first || waiting[1].ID != second {
		t.Fatalf("waiting at 500 = %+v, %v", waiting, err)
	}
	if n := waiting[0]; n.Subject != "Hi" || n.Folder != "Receipts" || !slices.Equal(n.Actions, []string{"move"}) || n.MessageID != 7 || n.RuleID != 0 {
		t.Errorf("notice = %+v", n)
	}
	if got, _ := s.WaitingNotices(ctx, 500, 1); len(got) != 1 {
		t.Errorf("limit 1 gave %d", len(got))
	}
	if err := s.NoticeHanded(ctx, first, 501); err != nil {
		t.Fatal(err)
	}
	if err := s.NoticeRetry(ctx, second, 600); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.WaitingNotices(ctx, 550, 10); len(got) != 0 {
		t.Errorf("waiting at 550 = %+v, want none: one taken, one waiting for its retry", got)
	}
	if got, _ := s.WaitingNotices(ctx, 1000, 10); len(got) != 2 || got[0].ID != second || got[0].Attempts != 1 || got[1].ID != later {
		t.Errorf("waiting at 1000 = %+v", got)
	}
	var payload *string
	if err := db.QueryRowContext(ctx, `SELECT payload FROM notices WHERE id = ?`, first).Scan(&payload); err != nil || payload != nil {
		t.Errorf("a handed-over notice keeps %v, %v", payload, err)
	}

	// The module reports; Settings reads the last ones per destination, newest first.
	for i, r := range []struct {
		id     int64
		sent   bool
		reason string
	}{{first, false, "Timed out."}, {first, true, "ignored when sent"}, {second, false, ""}, {later, true, ""}} {
		if tenant, err := s.RecordDelivery(ctx, r.id, r.sent, r.reason, int64(1000+i)); err != nil || tenant != u.TenantID {
			t.Fatalf("RecordDelivery = %d, %v", tenant, err)
		}
	}
	if _, err := s.RecordDelivery(ctx, 999, true, "", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("a delivery of no notice: %v", err)
	}
	recent, err := s.RecentDeliveries(ctx, u.TenantID, 2)
	if err != nil {
		t.Fatal(err)
	}
	got := recent[d.ID]
	if len(got) != 2 || got[0].NoticeID != second || got[0].Status != DeliveryFailed ||
		got[1].NoticeID != first || got[1].Status != DeliverySent || got[1].Reason != "" || got[1].RuleName != "Receipts" {
		t.Errorf("recent deliveries of A = %+v", got)
	}
	if len(recent[other.ID]) != 1 {
		t.Errorf("recent deliveries of B = %+v", recent[other.ID])
	}
	if other, _ := s.RecentDeliveries(ctx, u.TenantID+1, 20); len(other) != 0 {
		t.Errorf("another tenant sees %+v", other)
	}

	// NoticeDays on, the retention job deletes the older ones, handed over or not.
	young := notice(other.ID, 5000)
	if _, _, err := s.Retain(ctx, u.TenantID, 1002+NoticeDays*day, 30); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.WaitingNotices(ctx, 2_000_000_000, 10); len(got) != 1 || got[0].ID != young {
		t.Errorf("after retention, waiting = %+v; want only the young one", got)
	}
	var kept int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices`).Scan(&kept); err != nil || kept != 1 {
		t.Errorf("after retention, %d notices kept (%v), want 1", kept, err)
	}
	recent, _ = s.RecentDeliveries(ctx, u.TenantID, 20)
	if n := len(recent[d.ID]) + len(recent[other.ID]); n != 2 {
		t.Errorf("after retention, %d deliveries left, want the two from 1002 on", n)
	}
}
