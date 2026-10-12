package daemon

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/pressly/goose/v3"

	"github.com/TurtleByte-IN/mailrules/internal/config"
	"github.com/TurtleByte-IN/mailrules/internal/mail/imap/imaptest"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/store/migrations"
	"github.com/TurtleByte-IN/mailrules/internal/store/pgtest"
)

func postgresOnly(t *testing.T) *pgtest.DB {
	t.Helper()
	if pgtest.DSN() == "" {
		t.Skip("needs Postgres: set " + pgtest.EnvDSN)
	}
	return pgtest.Open(t)
}

// The hosted build end to end on Postgres: the daemon runs as the app role with the mailbox
// workers on the worker role, connects a mailbox, and sorts an email, and what it stored is
// its tenant's alone.
func TestPostgresEndToEnd(t *testing.T) {
	db := postgresOnly(t)
	srv := imaptest.Start(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapMove: {}, imap.CapUIDPlus: {}})
	if err := srv.User.Create("Receipts", nil); err != nil {
		t.Fatal(err)
	}
	d := serveWithEnv(t, srv.TLS, map[string]string{
		"MAILRULES_DATABASE_URL":        db.URL(pgtest.AsApp),
		"MAILRULES_WORKER_DATABASE_URL": db.URL(pgtest.AsWorker),
	})
	c := d.c
	c.do(http.MethodPost, "/api/accounts", map[string]any{"preset": "generic", "host": srv.Host, "port": srv.Port, "tls_mode": "implicit",
		"username": imaptest.Username, "password": imaptest.Password}, http.StatusCreated)
	c.do(http.MethodPatch, "/api/settings", map[string]any{"dry_run": false}, http.StatusOK)
	c.do(http.MethodPost, "/api/rules/batch", map[string]any{"rules": []map[string]any{{"name": "Receipts",
		"conditions": map[string]any{"field": "subject", "op": "contains_any", "value": []string{"Receipt"}},
		"actions":    []map[string]any{{"type": "move", "folder": "Receipts"}}}}}, http.StatusCreated)
	eventually(t, "the mailbox goes live", func() (bool, string) { a := c.account(1); return a.Status == "live", a.Status })
	srv.Append(t, "INBOX", receipt(1))
	eventually(t, "the receipt is sorted", func() (bool, string) {
		n := count(t, srv, "Receipts")
		return n == 1, strconv.Itoa(int(n)) + " in Receipts"
	})

	// The workers ran as the worker role, the rest as the app role.
	ctx := t.Context()
	for _, role := range []string{pgtest.AsApp, pgtest.AsWorker} {
		var n int
		if err := db.Admin.QueryRowContext(ctx, `SELECT COUNT(*) FROM pg_stat_activity WHERE datname = $1 AND usename = $2`, db.Name, role).Scan(&n); err != nil || n == 0 {
			t.Errorf("no connection as %s (%d, %v)", role, n, err)
		}
	}
	// The rows are the self-host tenant's: the app role sees them in its scope and none in
	// another tenant's or in none.
	messages := func(tenant string) int {
		tx, err := db.App.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if tenant != "" {
			if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
				t.Fatal(err)
			}
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := messages(strconv.FormatInt(store.SelfHostTenant, 10)); got != 1 {
		t.Errorf("the tenant's own messages: %d, want 1", got)
	}
	if got := messages("2"); got != 0 {
		t.Errorf("another tenant sees %d messages", got)
	}
	if got := messages(""); got != 0 {
		t.Errorf("no tenant sees %d messages", got)
	}
}

// Migrations run as the owner, only through `mailrules migrate` and
// MAILRULES_MIGRATE_DATABASE_URL; a daemon on Postgres refuses a schema it was not built for.
func TestPostgresMigrateAndSchemaCheck(t *testing.T) {
	db := postgresOnly(t)
	ctx := t.Context()
	load := func(env map[string]string) *config.Config {
		cfg, err := config.Load(nil, func(k string) string { return env[k] })
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	app := map[string]string{"MAILRULES_DATA_DIR": t.TempDir(), "MAILRULES_DATABASE_URL": db.URL(pgtest.AsApp)}
	if err := migrate(ctx, load(app)); err == nil {
		t.Error("migrate ran with the app's URL alone")
	}
	if err := migrate(ctx, load(map[string]string{"MAILRULES_MIGRATE_DATABASE_URL": db.URL(pgtest.AsOwner)})); err != nil {
		t.Fatalf("migrate as the owner: %v", err)
	}
	dbs, err := openDatabases(ctx, load(app))
	if err != nil {
		t.Fatalf("open a migrated database: %v", err)
	}
	dbs.Close()
	// One migration short: as a database migrated by an older build would be. The newest
	// migration is rolled back rather than only struck from the history, which would leave
	// its changes in place for migrating again to run into.
	owner, err := store.OpenPostgres(ctx, db.URL(pgtest.AsOwner))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	p, err := goose.NewProvider(goose.DialectPostgres, owner, migrations.Postgres())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Down(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := openDatabases(ctx, load(app)); !errors.Is(err, store.ErrSchema) {
		t.Fatalf("a schema one migration behind: %v, want ErrSchema", err)
	}
	if err := migrate(context.Background(), load(map[string]string{"MAILRULES_MIGRATE_DATABASE_URL": db.URL(pgtest.AsOwner)})); err != nil {
		t.Fatalf("migrate it again: %v", err)
	}
	if dbs, err := openDatabases(ctx, load(app)); err != nil {
		t.Errorf("after migrating: %v", err)
	} else {
		dbs.Close()
	}
	// A migration ahead: as a newer build would leave it.
	if _, err := db.Admin.ExecContext(ctx, `INSERT INTO goose_db_version (version_id, is_applied) VALUES (9999, TRUE)`); err != nil {
		t.Fatal(err)
	}
	if _, err := openDatabases(ctx, load(app)); !errors.Is(err, store.ErrSchema) {
		t.Errorf("a schema from a newer build: %v, want ErrSchema", err)
	}
}
