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
		"messages", "rules", "sender_rules", "sessions", "settings", "usage_daily", "users"}
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
