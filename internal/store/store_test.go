package store

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/TurtleByte-IN/mailrules/internal/crypto"
	"github.com/TurtleByte-IN/mailrules/internal/store/migrations"
	"github.com/TurtleByte-IN/mailrules/internal/store/pgtest"
)

// open gives the test a fresh, migrated store and the raw handle to its database: on
// Postgres when MAILRULES_TEST_POSTGRES_DSN is set, where the store runs as the app role and
// the handle is the superuser (which takes SQLite's ? placeholders; see pgtest), and on
// SQLite otherwise.
func open(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	if pgtest.DSN() != "" {
		// The test's database is a copy of a template the owner role migrated.
		db := pgtest.Open(t)
		return keyed(db.App), db.Admin
	}
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
	return keyed(db), db
}

// keyed is the store over db with testMaster as its key-encryption key.
func keyed(db *sql.DB) *Store { return New(db).WithKeys(crypto.MasterKey(testMaster)) }

// sqliteOnly skips the test when it would run on Postgres; why says what it needs SQLite
// for.
func sqliteOnly(t *testing.T, why string) {
	t.Helper()
	if pgtest.DSN() != "" {
		t.Skip("needs SQLite: " + why)
	}
}

func TestOpenAndMigrate(t *testing.T) {
	sqliteOnly(t, "checks SQLite pragmas and sqlite_master")
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
	want := []string{"accounts", "actions", "ai_emails", "audit_log", "batches", "contacts", "corrections", "decisions", "draft_jobs", "first_cleanup", "folders", "forgotten",
		"identities", "invitations", "messages", "move_questions", "moves", "notices", "notify_deliveries", "notify_destinations", "plans", "removed_mailboxes",
		"rules", "sender_rules", "sessions", "settings", "summaries", "tenants", "unsubscribes", "usage_daily", "users"}
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

// A database an earlier build migrated opens without running anything again: goose knows a
// migration by its version number alone, so every embedded migration must keep its number
// for as long as there are databases that ran it, wherever its file lives.
func TestMigrateAgainRunsNothing(t *testing.T) {
	sqliteOnly(t, "replays the SQLite migration set with goose's SQLite dialect")
	_, db := open(t) // migrated twice already
	ctx := t.Context()
	fsys := migrations.SQLite()
	p, err := goose.NewProvider(goose.DialectSQLite3, db, fsys)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := p.Up(ctx); err != nil || len(res) != 0 {
		t.Fatalf("a third run applied %v, %v; want nothing", res, err)
	}
	var files []int64
	for _, src := range p.ListSources() {
		files = append(files, src.Version)
	}
	rows, err := db.QueryContext(ctx, `SELECT version_id FROM goose_db_version WHERE version_id > 0 AND is_applied ORDER BY version_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var applied []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		applied = append(applied, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(applied, files) {
		t.Fatalf("applied versions %v, embedded %v", applied, files)
	}
	if files[0] != 1 || files[len(files)-1] != int64(len(files)) {
		t.Fatalf("embedded versions %v are not 1 to %d", files, len(files))
	}
}

// Every schema change is written once per dialect under the same version, so a version
// means the same schema on either: from the Postgres baseline (0023) on, the two sets list
// the same versions.
func TestMigrationSetsAgree(t *testing.T) {
	versions := func(fsys fs.FS) map[string]bool {
		names, err := fs.Glob(fsys, "*.sql")
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, n := range names {
			if v, _, ok := strings.Cut(n, "_"); ok && v >= "0023" {
				out[v] = true
			}
		}
		return out
	}
	lite, pg := versions(migrations.SQLite()), versions(migrations.Postgres())
	if len(pg) == 0 || !maps.Equal(lite, pg) {
		t.Fatalf("versions from 0023: SQLite %v, Postgres %v", slices.Sorted(maps.Keys(lite)), slices.Sorted(maps.Keys(pg)))
	}
}

// The database is opened in the data directory given, whatever characters its path holds:
// SQLite reads a "file:" name as a URI, where "#" would end the path and "?" start the
// parameters, so a data dir such as "Mail #2" put the database in another file (MAI-172).
func TestOpenInAnyDataDir(t *testing.T) {
	sqliteOnly(t, "opens the SQLite database file in a data dir")
	for _, tc := range []struct {
		name, dir string
		relative  bool
	}{
		{name: "plain", dir: "data"},
		{name: "hash", dir: "Mail #2"},
		{name: "question mark", dir: "who?mode=memory"},
		{name: "percent", dir: "100%41"},
		{name: "relative", dir: "rel #1/data", relative: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, tc.dir)
			given := dir
			if tc.relative {
				t.Chdir(root)
				given = tc.dir
			}
			db, err := Open(t.Context(), given)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if err := Migrate(t.Context(), db); err != nil {
				t.Fatal(err)
			}
			var seq int
			var schema, file, mode string
			if err := db.QueryRowContext(t.Context(), "PRAGMA database_list").Scan(&seq, &schema, &file); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
				t.Errorf("journal_mode = %q, %v; want wal", mode, err)
			}
			want, err := filepath.EvalSymlinks(filepath.Join(dir, "mailrules.db"))
			if err != nil {
				t.Fatalf("no database in the data dir: %v", err)
			}
			if got, err := filepath.EvalSymlinks(file); err != nil || got != want {
				t.Fatalf("opened %q (%v), want %q", file, err, want)
			}
			if _, err := os.Stat(filepath.Join(root, "Mail ")); err == nil {
				t.Error("a database was made in a cut-short path")
			}
			// The connection settings survive the escaping: a transaction holds the write lock
			// from its BEGIN (_txlock=immediate), so another connection cannot take it.
			tx, err := db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			other, err := db.Conn(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = other.Close() }()
			if _, err := other.ExecContext(t.Context(), "PRAGMA busy_timeout = 0"); err != nil {
				t.Fatal(err)
			}
			if _, err := other.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err == nil {
				_, _ = other.ExecContext(t.Context(), "ROLLBACK")
				t.Error("a transaction began without the write lock: _txlock=immediate was lost")
			}
			if _, err := other.ExecContext(t.Context(), "PRAGMA busy_timeout = 5000"); err != nil {
				t.Fatal(err)
			}
		})
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
	if _, err := s.Setting(ctx, SelfHostTenant, "dry_run"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing setting: got %v", err)
	}
	for _, v := range []string{"true", "false"} {
		if err := s.SetSetting(ctx, SelfHostTenant, "dry_run", v); err != nil {
			t.Fatal(err)
		}
		if got, err := s.Setting(ctx, SelfHostTenant, "dry_run"); err != nil || got != v {
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
		if err := s.AddUsage(ctx, SelfHostTenant, false, a.day, "openrouter", "typesafe/jev-1.13", a.purpose, a.calls, a.in, a.out, a.cost); err != nil {
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
	list, err := st.Batches(ctx, a1.Owner(), BatchCleanup, 0, 10)
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

// legacyUser inserts the admin user straight into a database whose schema is from before
// migration 0011 (no users.tenant_id) or 0028 (no users.role). It returns its id, which is
// 1, so after the full migration it is the self-host tenant's user.
func legacyUser(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	res, err := db.ExecContext(t.Context(),
		`INSERT INTO users (email, password_hash, created_at) VALUES ('me@icloud.com', 'hash', 1)`)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// legacyAccount inserts a mailbox straight into a pre-0011 schema (no tenant_id/shared) and
// returns its id.
func legacyAccount(t *testing.T, db *sql.DB, userID int64) int64 {
	t.Helper()
	res, err := db.ExecContext(t.Context(),
		`INSERT INTO accounts (user_id, label, preset, host, port, tls_mode, username, secret_enc, dek_enc, watch_folder, status, created_at)
		 VALUES (?, 'Personal', 'icloud', 'imap.mail.me.com', 993, 'implicit', 'me@icloud.com', x'', x'', 'INBOX', 'new', 1)`, userID)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}
