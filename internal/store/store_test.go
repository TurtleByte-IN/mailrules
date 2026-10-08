package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func open(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	db, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// Running twice must be harmless.
	for range 2 {
		if err := Migrate(t.Context(), db); err != nil {
			t.Fatal(err)
		}
	}
	return New(db), db
}

func TestOpenAndMigrate(t *testing.T) {
	_, db := open(t)
	ctx := context.Background()
	for _, q := range []struct{ pragma, want string }{{"journal_mode", "wal"}, {"foreign_keys", "1"}} {
		var got string
		if err := db.QueryRowContext(ctx, "PRAGMA "+q.pragma).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != q.want {
			t.Errorf("%s = %s, want %s", q.pragma, got, q.want)
		}
	}
	want := []string{"accounts", "actions", "batches", "contacts", "corrections", "decisions", "folders",
		"messages", "rules", "sender_rules", "sessions", "settings", "summaries", "usage_daily", "users"}
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'goose%' AND name NOT LIKE 'sqlite%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		got = append(got, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("tables = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tables = %v, want %v", got, want)
		}
	}
}

func TestUsers(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	if has, err := s.HasUsers(ctx); err != nil || has {
		t.Fatalf("fresh db HasUsers = %v, %v", has, err)
	}
	u, err := s.CreateFirstUser(ctx, "me@icloud.com", "hash", 100)
	if err != nil || u.ID == 0 {
		t.Fatalf("create: %+v %v", u, err)
	}
	if _, err := s.CreateFirstUser(ctx, "other@icloud.com", "hash", 101); !errors.Is(err, ErrAlreadySetUp) {
		t.Fatalf("second user: want ErrAlreadySetUp, got %v", err)
	}
	if has, _ := s.HasUsers(ctx); !has {
		t.Fatal("HasUsers false after create")
	}
	got, err := s.UserByEmail(ctx, "ME@iCloud.com")
	if err != nil || got != u {
		t.Fatalf("by email: %+v %v", got, err)
	}
	if _, err := s.UserByEmail(ctx, "nobody@icloud.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing user: got %v", err)
	}
}

func TestSessions(t *testing.T) {
	s, db := open(t)
	ctx := t.Context()
	u, _ := s.CreateFirstUser(ctx, "me@icloud.com", "hash", 100)
	if err := s.CreateSession(ctx, "h1", u.ID, 100, 200); err != nil {
		t.Fatal(err)
	}
	got, exp, err := s.SessionUser(ctx, "h1", 150)
	if err != nil || got.ID != u.ID || exp != 200 {
		t.Fatalf("live session: %+v %d %v", got, exp, err)
	}
	if _, _, err := s.SessionUser(ctx, "h1", 200); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session: got %v", err)
	}
	if err := s.ExtendSession(ctx, "h1", 300); err != nil {
		t.Fatal(err)
	}
	if _, exp, err := s.SessionUser(ctx, "h1", 250); err != nil || exp != 300 {
		t.Fatalf("extended session: %d %v", exp, err)
	}
	// A new login sweeps sessions that have expired.
	if err := s.CreateSession(ctx, "h2", u.ID, 400, 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("sessions after sweep = %d, %v", n, err)
	}
	if err := s.DeleteSession(ctx, "h2"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SessionUser(ctx, "h2", 401); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted session: got %v", err)
	}
	if err := s.DeleteSession(ctx, "h2"); err != nil {
		t.Fatalf("deleting twice: %v", err)
	}
}

func TestSetPasswordEndsSessions(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	u, _ := s.CreateFirstUser(ctx, "me@icloud.com", "old-hash", 100)
	for _, h := range []string{"h1", "h2"} {
		if err := s.CreateSession(ctx, h, u.ID, 100, 1000); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetPassword(ctx, u.ID, "new-hash"); err != nil {
		t.Fatal(err)
	}
	got, err := s.UserByEmail(ctx, "me@icloud.com")
	if err != nil || got.PasswordHash != "new-hash" {
		t.Fatalf("user after change = %+v, %v", got, err)
	}
	for _, h := range []string{"h1", "h2"} {
		if _, _, err := s.SessionUser(ctx, h, 150); !errors.Is(err, ErrNotFound) {
			t.Errorf("session %s after the change: got %v, want ErrNotFound", h, err)
		}
	}
	if err := s.SetPassword(ctx, u.ID+1, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user: got %v, want ErrNotFound", err)
	}
}

func TestSettings(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	if _, err := s.Setting(ctx, "dry_run"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing setting: got %v", err)
	}
	for _, v := range []string{"true", "false"} {
		if err := s.SetSetting(ctx, "dry_run", v); err != nil {
			t.Fatal(err)
		}
		if got, err := s.Setting(ctx, "dry_run"); err != nil || got != v {
			t.Fatalf("setting = %q, %v", got, err)
		}
	}
}

func TestAddUsage(t *testing.T) {
	s, db := open(t)
	ctx := t.Context()
	adds := []struct {
		day, purpose   string
		calls, in, out int
		cost           float64
	}{
		{"2026-10-06", "decide", 1, 400, 0, 0.00002},
		{"2026-10-06", "decide", 1, 600, 0, 0.00003},
		{"2026-10-06", "escalate", 1, 900, 60, 0.0012},
		{"2026-10-07", "decide", 2, 100, 0, 0.00001},
	}
	for _, a := range adds {
		if err := s.AddUsage(ctx, a.day, "openrouter", "typesafe/jev-1.13", a.purpose, a.calls, a.in, a.out, a.cost); err != nil {
			t.Fatal(err)
		}
	}
	want := []struct {
		day, purpose   string
		calls, in, out int
		cost           float64
	}{
		{"2026-10-06", "decide", 2, 1000, 0, 0.00005},
		{"2026-10-06", "escalate", 1, 900, 60, 0.0012},
		{"2026-10-07", "decide", 2, 100, 0, 0.00001},
	}
	rows, err := db.QueryContext(ctx, `SELECT day, purpose, calls, tokens_in, tokens_out, cost_usd FROM usage_daily ORDER BY day, purpose`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	i := 0
	for ; rows.Next(); i++ {
		if i >= len(want) {
			t.Fatalf("more than %d rows", len(want))
		}
		w, got := want[i], want[i]
		if err := rows.Scan(&got.day, &got.purpose, &got.calls, &got.in, &got.out, &got.cost); err != nil {
			t.Fatal(err)
		}
		if got.day != w.day || got.purpose != w.purpose || got.calls != w.calls || got.in != w.in || got.out != w.out ||
			got.cost < w.cost-1e-12 || got.cost > w.cost+1e-12 {
			t.Errorf("row %d = %+v, want %+v", i, got, w)
		}
	}
	if err := rows.Err(); err != nil || i != len(want) {
		t.Fatalf("rows = %d, want %d (%v)", i, len(want), err)
	}
}

// A manual run over several mailboxes is one cleanup batch per mailbox, created together and
// joined by a RunID (the id of the first); a run of one mailbox is a plain cleanup batch.
func TestCreateCleanupRun(t *testing.T) {
	st, _ := open(t)
	ctx := t.Context()
	a1, a2 := newAccount(t, st, "one.com", "abcd-efgh-ijkl-mnop"), newAccount(t, st, "two.com", "qrst-uvwx-yzab-cdef")
	one := CleanupBatch{AccountID: a1.ID, Folder: "INBOX", ScanLimit: 2000, ScanMatched: 5, Total: 5}
	two := CleanupBatch{AccountID: a2.ID, Folder: "INBOX", Since: 99, Total: 0}

	got, err := st.CreateCleanupRun(ctx, []CleanupBatch{one, two}, 1000)
	if err != nil || len(got) != 2 {
		t.Fatalf("CreateCleanupRun = %+v, %v", got, err)
	}
	if got[0].RunID != got[0].ID || got[1].RunID != got[0].ID || got[1].ID == got[0].ID {
		t.Errorf("run ids = %d, %d for batches %d, %d; want both %d", got[0].RunID, got[1].RunID, got[0].ID, got[1].ID, got[0].ID)
	}
	for i, want := range []CleanupBatch{one, two} {
		b := got[i]
		if b.Kind != BatchCleanup || b.Status != BatchRunning || b.AccountID != want.AccountID || b.Folder != want.Folder ||
			b.Since != want.Since || b.Total != want.Total || b.ScanLimit != want.ScanLimit || b.ScanMatched != want.ScanMatched || b.CreatedAt != 1000 {
			t.Errorf("batch %d = %+v, want %+v", i, b, want)
		}
	}
	// Read back through the listing, as the screen does, the run id is there.
	list, err := st.Batches(ctx, BatchCleanup, 0, 10)
	if err != nil || len(list) != 2 || list[0].RunID != got[0].ID || list[1].RunID != got[0].ID {
		t.Errorf("Batches = %+v, %v", list, err)
	}

	solo, err := st.CreateCleanupRun(ctx, []CleanupBatch{one}, 1001)
	if err != nil || len(solo) != 1 || solo[0].RunID != 0 {
		t.Errorf("a run of one mailbox = %+v, %v; want a batch with no run id", solo, err)
	}
	if b, err := st.CreateCleanupBatch(ctx, a1.ID, "INBOX", 0, 0, 0, 3, 1002); err != nil || b.RunID != 0 {
		t.Errorf("CreateCleanupBatch = %+v, %v; want no run id", b, err)
	}
}
