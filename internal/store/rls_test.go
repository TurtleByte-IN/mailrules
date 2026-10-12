package store

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store/pgtest"
)

// The row-level security of the Postgres schema (MAI-150). These tests talk to the database
// as the roles themselves, not through the store, so they hold whatever the store does.

func pgOnly(t *testing.T) *pgtest.DB {
	t.Helper()
	if pgtest.DSN() == "" {
		t.Skip("needs Postgres: set " + pgtest.EnvDSN)
	}
	return pgtest.Open(t)
}

// appTables lists the tables of the schema, goose's own excepted.
func appTables(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind = 'r' AND relname <> 'goose_db_version' ORDER BY relname`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Every table is kept to its tenant: row security on and forced (the owner too), the tenant
// policy, and a tenant_id column. A migration that adds a table without them fails here.
func TestEveryTableIsTenantScoped(t *testing.T) {
	db := pgOnly(t)
	tables := appTables(t, db.Admin)
	if len(tables) < 27 {
		t.Fatalf("only %d tables: %v", len(tables), tables)
	}
	for _, table := range tables {
		var enabled, forced, policy, column bool
		if err := db.Admin.QueryRowContext(t.Context(), `SELECT c.relrowsecurity, c.relforcerowsecurity,
			EXISTS (SELECT 1 FROM pg_policy p WHERE p.polrelid = c.oid AND p.polname = 'mailrules_tenant'),
			EXISTS (SELECT 1 FROM information_schema.columns k WHERE k.table_schema = 'public' AND k.table_name = c.relname
			        AND k.column_name = CASE c.relname WHEN 'tenants' THEN 'id' ELSE 'tenant_id' END)
			FROM pg_class c WHERE c.relname = $1 AND c.relnamespace = 'public'::regnamespace`, table).Scan(&enabled, &forced, &policy, &column); err != nil {
			t.Fatal(err)
		}
		if !enabled || !forced || !policy || !column {
			t.Errorf("%s: row security %v, forced %v, tenant policy %v, tenant column %v", table, enabled, forced, policy, column)
		}
	}
}

// Two tenants with a row in every table they share a schema with: scoped to one, the app
// and worker roles see only its rows, and with no tenant none at all; a write into the
// other tenant is refused.
func TestTenantsAreApart(t *testing.T) {
	db := pgOnly(t)
	ctx := t.Context()
	s := keyed(db.App)
	var tenants [2]int64
	for i, org := range []string{"org_a", "org_b"} {
		u, err := s.SignInIdentity(ctx, Identity{Provider: "workos", Subject: "user_" + org, Email: org + "@example.test", Tenant: org}, 1)
		if err != nil {
			t.Fatal(err)
		}
		tenants[i] = u.TenantID
		if _, err := s.CreateAccount(ctx, Account{UserID: u.ID, Label: org, Preset: "generic", Host: "imap.example.test", Port: 993, Username: org}, "pw"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateRule(ctx, u.TenantID, rules.Rule{UserID: u.ID, Name: "Rule " + org, Intent: "x", Enabled: true,
			Actions: []rules.Action{{Type: rules.ActKeep}}}, 1); err != nil {
			t.Fatal(err)
		}
		if err := s.SetSetting(ctx, u.TenantID, "dry_run", "true"); err != nil {
			t.Fatal(err)
		}
	}
	tables := appTables(t, db.Admin)
	for _, role := range []struct {
		name string
		db   *sql.DB
	}{{"app", db.App}, {"worker", db.Worker}} {
		for _, table := range tables {
			if role.name == "worker" && table == "sessions" {
				continue // the worker has no access at all (below)
			}
			col := "tenant_id"
			if table == "tenants" {
				col = "id"
			}
			if n := countAs(t, role.db, 0, `SELECT COUNT(*) FROM `+table); n != 0 {
				t.Errorf("%s with no tenant sees %d rows of %s", role.name, n, table)
			}
			if n := countAs(t, role.db, tenants[0], `SELECT COUNT(*) FROM `+table+` WHERE `+col+` <> $1`, tenants[0]); n != 0 {
				t.Errorf("%s in tenant %d sees %d rows of %s of another tenant", role.name, tenants[0], n, table)
			}
		}
		if n := countAs(t, role.db, tenants[0], `SELECT COUNT(*) FROM accounts`); n != 1 {
			t.Errorf("%s in tenant %d sees %d of its accounts, want 1", role.name, tenants[0], n)
		}
		err := asTenant(t, role.db, tenants[0], func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO settings (tenant_id, key, value) VALUES ($1, 'x', '1')`, tenants[1])
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "row-level security") {
			t.Errorf("%s wrote into another tenant: %v", role.name, err)
		}
	}
}

// The roles cannot step around the policies or read what is not theirs to read.
func TestRolesCannotBypass(t *testing.T) {
	db := pgOnly(t)
	ctx := t.Context()
	for _, tc := range []struct {
		name, query string
		db          *sql.DB
	}{
		{"app disables row security", `ALTER TABLE accounts DISABLE ROW LEVEL SECURITY`, db.App},
		{"app drops a policy", `DROP POLICY mailrules_tenant ON accounts`, db.App},
		{"app reads the migration history", `SELECT COUNT(*) FROM goose_db_version`, db.App},
		{"worker reads password hashes", `SELECT password_hash FROM users`, db.Worker},
		{"worker reads sessions", `SELECT COUNT(*) FROM sessions`, db.Worker},
		{"worker sets a password", `UPDATE users SET password_hash = ''`, db.Worker},
	} {
		if _, err := tc.db.ExecContext(ctx, tc.query); err == nil {
			t.Errorf("%s: allowed", tc.name)
		}
	}
	for _, role := range []string{"mailrules_app", "mailrules_worker", "mailrules_definer"} {
		var super, bypass, login bool
		if err := db.Admin.QueryRowContext(ctx, `SELECT rolsuper, rolbypassrls, rolcanlogin FROM pg_roles WHERE rolname = $1`, role).Scan(&super, &bypass, &login); err != nil {
			t.Fatal(err)
		}
		if super || bypass || login {
			t.Errorf("%s: superuser %v, bypasses row security %v, logs in %v", role, super, bypass, login)
		}
	}
}

// countAs runs a count as db's role in a transaction scoped to tenant (none for 0).
func countAs(t *testing.T, db *sql.DB, tenant int64, query string, args ...any) int {
	t.Helper()
	var n int
	if err := asTenant(t, db, tenant, func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), query, args...).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func asTenant(t *testing.T, db *sql.DB, tenant int64, fn func(*sql.Tx) error) error {
	t.Helper()
	tx, err := db.BeginTx(context.WithoutCancel(t.Context()), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if tenant != 0 {
		if _, err := tx.ExecContext(t.Context(), `SELECT set_config('app.tenant_id', $1, true)`, strconv.FormatInt(tenant, 10)); err != nil {
			t.Fatal(err)
		}
	}
	return fn(tx)
}
