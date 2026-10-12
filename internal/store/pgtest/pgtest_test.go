package pgtest

import (
	"database/sql"
	"testing"
)

func TestRebind(t *testing.T) {
	for _, tt := range []struct{ name, in, want string }{
		{"none", `SELECT 1`, `SELECT 1`},
		{"dollar already", `a = $1`, `a = $1`},
		{"bare", `a = ? AND b = ?`, `a = $1 AND b = $2`},
		{"numbered, reused", `a = ?1 OR b = ?1 AND c = ?2`, `a = $1 OR b = $1 AND c = $2`},
		{"bare after numbered", `a = ?2 AND b = ?1 AND c = ?`, `a = $2 AND b = $1 AND c = $3`},
		{"multi-digit", `a = ?12, ?`, `a = $12, $13`},
		{"single quotes", `a = '?it''s ?1' AND b = ?`, `a = '?it''s ?1' AND b = $1`},
		{"double quotes", `"col?" = ? AND "a""?" = ?`, `"col?" = $1 AND "a""?" = $2`},
		{"line comment", "a = ? -- what?\n AND b = ?", "a = $1 -- what?\n AND b = $2"},
		{"line comment at the end", "a = ? -- what?", "a = $1 -- what?"},
		{"block comment", `a = ? /* ?1 ? */ AND b = ?`, `a = $1 /* ?1 ? */ AND b = $2`},
		{"unclosed quote", `a = ? AND b = '?`, `a = $1 AND b = '?`},
		{"unclosed comment", `a = ? /* ?`, `a = $1 /* ?`},
		{"minus and slash alone", `a - ? / ?`, `a - $1 / $2`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := rebind(tt.in); got != tt.want {
				t.Errorf("rebind(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestOpen(t *testing.T) {
	if DSN() == "" {
		t.Skip(EnvDSN + " is not set")
	}
	ctx := t.Context()
	var names [2]string
	t.Run("open", func(t *testing.T) {
		a, b := Open(t), Open(t)
		if a.Name == b.Name {
			t.Fatalf("two Opens share database %s", a.Name)
		}
		names = [2]string{a.Name, b.Name}
		for _, c := range []struct {
			name string
			db   *DB
		}{{"a", a}, {"b", b}} {
			check := func(who, wantUser string, super bool, q interface {
				Scan(...any) error
			}) {
				t.Helper()
				var user, database string
				var isSuper bool
				if err := q.Scan(&user, &database, &isSuper); err != nil {
					t.Fatalf("%s %s: %v", c.name, who, err)
				}
				if (wantUser != "" && user != wantUser) || database != c.db.Name || isSuper != super {
					t.Errorf("%s %s: user %s, database %s, superuser %v; want %s, %s, %v",
						c.name, who, user, database, isSuper, wantUser, c.db.Name, super)
				}
			}
			const who = `SELECT current_user::text, current_database()::text, rolsuper FROM pg_roles WHERE rolname = current_user`
			check("app", appRole, false, c.db.App.QueryRowContext(ctx, who))
			check("worker", workerRole, false, c.db.Worker.QueryRowContext(ctx, who))
			check("admin", setup.base.User, true, c.db.Admin.QueryRowContext(ctx, who))
		}

		// The admin connection takes SQLite's placeholders.
		var n int
		var s string
		if err := a.Admin.QueryRowContext(ctx, `SELECT ?::int + ?1::int, ?2::text`, 20, "x").Scan(&n, &s); err != nil || n != 40 || s != "x" {
			t.Errorf("admin rebind: %d, %q, %v", n, s, err)
		}
		// The database is migrated, and row-level security hides the self-host tenant from
		// the app until a transaction names it; the superuser sees it.
		var version int64
		if err := a.Admin.QueryRowContext(ctx, `SELECT max(version_id) FROM goose_db_version`).Scan(&version); err != nil || version < 23 {
			t.Errorf("goose version %d, %v; want at least 23", version, err)
		}
		for _, c := range []struct {
			who  string
			db   *sql.DB
			want int
		}{{"admin", a.Admin, 1}, {"app", a.App, 0}} {
			var got int
			if err := c.db.QueryRowContext(ctx, `SELECT count(*) FROM tenants`).Scan(&got); err != nil || got != c.want {
				t.Errorf("%s sees %d tenants, %v; want %d", c.who, got, err, c.want)
			}
		}
	})
	// The subtest's cleanup dropped both databases.
	var left int
	if err := setup.super.QueryRowContext(ctx, `SELECT count(*) FROM pg_database WHERE datname = ANY($1)`, names[:]).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d of the test databases %v are left after cleanup", left, names)
	}
}
