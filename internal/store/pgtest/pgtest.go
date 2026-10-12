// Package pgtest gives a test its own Postgres database, migrated to the store's current
// Postgres schema, with connections as the roles the hosted build runs as. Tests run on
// Postgres when MAILRULES_TEST_POSTGRES_DSN names a superuser connection, and on SQLite
// otherwise.
//
// The schema is migrated once into a template database named after a hash of the migration
// files, so a changed migration gets a new template; each test's database is a copy of it,
// dropped when the test ends. The package must not import the store: the store's own tests
// use it.
package pgtest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/TurtleByte-IN/mailrules/internal/store/migrations"
)

// EnvDSN names the environment variable holding the superuser DSN.
const EnvDSN = "MAILRULES_TEST_POSTGRES_DSN"

// The login roles the harness creates. Roles are cluster-wide and outlive the test run;
// mailrules_app, mailrules_worker and mailrules_definer are created by the baseline
// migration.
const (
	ownerRole  = "mailrules_owner"
	appRole    = "mailrules_test_app"
	workerRole = "mailrules_test_worker"
	password   = "mailrules_test"
	// lockKey serialises setup across the test processes `go test ./...` runs at once.
	lockKey = 0x6d61696c72756c65 // "mailrule"
)

// DSN is MAILRULES_TEST_POSTGRES_DSN, a superuser DSN; empty means tests run on SQLite.
func DSN() string { return os.Getenv(EnvDSN) }

// DB is one test's own database, made from a template migrated to the current Postgres set.
type DB struct {
	// App is connected as mailrules_test_app, a member of mailrules_app: what the store runs on.
	App *sql.DB
	// Worker is connected as mailrules_test_worker, a member of mailrules_worker.
	Worker *sql.DB
	// Admin is connected as the DSN's superuser, which row-level security does not apply
	// to. Its queries may use SQLite's ? and ?N placeholders: they are rewritten to $n
	// (outside quotes and comments), so tests' raw SQL written for SQLite runs unchanged.
	Admin *sql.DB
	// Name is the database's name.
	Name string
}

var setup struct {
	once  sync.Once
	base  *pgx.ConnConfig // the parsed DSN
	super *sql.DB         // the superuser, on the DSN's database
	tpl   string
	err   error
}

// Open makes the test's database and connects to it; it is dropped when the test ends.
// Callers check DSN first: Open fails the test when it is empty.
func Open(t testing.TB) *DB {
	t.Helper()
	if DSN() == "" {
		t.Fatalf("pgtest: %s is not set", EnvDSN)
	}
	setup.once.Do(func() { setup.err = prepare(context.Background()) })
	if setup.err != nil {
		t.Fatalf("pgtest: %v", setup.err)
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	name := "mailrules_t_" + hex.EncodeToString(b[:])
	ctx := t.Context()
	if _, err := setup.super.ExecContext(ctx, "CREATE DATABASE "+name+" TEMPLATE "+setup.tpl+" OWNER "+ownerRole); err != nil {
		t.Fatalf("pgtest: create database: %v", err)
	}
	// Cleanups run last first: the connections close before the database is dropped.
	t.Cleanup(func() {
		if _, err := setup.super.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("pgtest: drop database %s: %v", name, err)
		}
	})
	db := &DB{
		App:    stdlib.OpenDB(as(appRole, password, name)),
		Worker: stdlib.OpenDB(as(workerRole, password, name)),
		Admin:  sql.OpenDB(rebinding{stdlib.GetConnector(as(setup.base.User, setup.base.Password, name))}),
		Name:   name,
	}
	t.Cleanup(func() {
		_ = db.App.Close()
		_ = db.Worker.Close()
		_ = db.Admin.Close()
	})
	return db
}

// as is the DSN's connection settings with another user and database.
func as(user, pass, database string) pgx.ConnConfig {
	c := setup.base.Copy()
	c.User, c.Password, c.Database = user, pass, database
	return *c
}

// Roles a test can sign in to its database as, by URL.
const (
	AsApp    = appRole
	AsWorker = workerRole
	AsOwner  = ownerRole
)

// URL is a postgres:// URL to the test's database signed in as role (AsApp, AsWorker or
// AsOwner): what the daemon is given in MAILRULES_*DATABASE_URL.
func (d *DB) URL(role string) string {
	u := url.URL{Scheme: "postgres", User: url.UserPassword(role, password),
		Host: net.JoinHostPort(setup.base.Host, strconv.Itoa(int(setup.base.Port))), Path: "/" + d.Name}
	if setup.base.TLSConfig == nil {
		u.RawQuery = "sslmode=disable"
	}
	return u.String()
}

// prepare creates the login roles and the template database, once per process. Several
// processes may run it at once: an advisory lock on the DSN's database serialises them, and
// the template is built under another name and renamed when complete, so a failed build
// never leaves a template behind.
func prepare(ctx context.Context) error {
	base, err := pgx.ParseConfig(DSN())
	if err != nil {
		return fmt.Errorf("parse %s: %w", EnvDSN, err)
	}
	setup.base = base
	setup.super = stdlib.OpenDB(*base)
	conn, err := setup.super.Conn(ctx)
	if err != nil {
		return fmt.Errorf("connect as superuser: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", int64(lockKey)); err != nil {
		return fmt.Errorf("lock: %w", err)
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", int64(lockKey)) }()

	if err := loginRole(ctx, conn, ownerRole, "LOGIN CREATEROLE"); err != nil {
		return err
	}
	sum, err := migrationsHash()
	if err != nil {
		return err
	}
	setup.tpl = "mailrules_tpl_" + sum
	var exists bool
	if err := conn.QueryRowContext(ctx, "SELECT EXISTS (SELECT FROM pg_database WHERE datname = $1)", setup.tpl).Scan(&exists); err != nil {
		return fmt.Errorf("look for the template: %w", err)
	}
	if !exists {
		if err := buildTemplate(ctx, conn, setup.tpl); err != nil {
			return err
		}
	}
	// The group roles exist now: the baseline migration created them.
	for role, group := range map[string]string{appRole: "mailrules_app", workerRole: "mailrules_worker"} {
		if err := loginRole(ctx, conn, role, "LOGIN"); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "GRANT "+group+" TO "+role); err != nil {
			return fmt.Errorf("grant %s to %s: %w", group, role, err)
		}
	}
	return nil
}

// loginRole creates role if it is missing and gives it attrs and the test password.
func loginRole(ctx context.Context, conn *sql.Conn, role, attrs string) error {
	var exists bool
	if err := conn.QueryRowContext(ctx, "SELECT EXISTS (SELECT FROM pg_roles WHERE rolname = $1)", role).Scan(&exists); err != nil {
		return fmt.Errorf("look for role %s: %w", role, err)
	}
	verb := "ALTER"
	if !exists {
		verb = "CREATE"
	}
	if _, err := conn.ExecContext(ctx, verb+" ROLE "+role+" "+attrs+" PASSWORD '"+password+"'"); err != nil {
		return fmt.Errorf("%s role %s: %w", verb, role, err)
	}
	return nil
}

// buildTemplate migrates a new database as mailrules_owner and renames it to tpl.
func buildTemplate(ctx context.Context, conn *sql.Conn, tpl string) error {
	build := tpl + "_build"
	if _, err := conn.ExecContext(ctx, "DROP DATABASE IF EXISTS "+build+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("drop an unfinished template: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "CREATE DATABASE "+build+" OWNER "+ownerRole); err != nil {
		return fmt.Errorf("create the template: %w", err)
	}
	owner := stdlib.OpenDB(as(ownerRole, password, build))
	p, err := goose.NewProvider(goose.DialectPostgres, owner, migrations.Postgres())
	if err == nil {
		_, err = p.Up(ctx)
	}
	// No session may be connected to a database while it is renamed or copied.
	_ = owner.Close()
	if err != nil {
		return fmt.Errorf("migrate the template: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()", build); err != nil {
		return fmt.Errorf("disconnect from the template: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "ALTER DATABASE "+build+" RENAME TO "+tpl); err != nil {
		return fmt.Errorf("rename the template: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "ALTER DATABASE "+tpl+" IS_TEMPLATE true"); err != nil {
		return fmt.Errorf("mark the template: %w", err)
	}
	return nil
}

// migrationsHash is the first 12 hex digits of a SHA-256 over the Postgres migration files'
// names and contents, in name order.
func migrationsHash() (string, error) {
	fsys := migrations.Postgres()
	var names []string
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, path)
		}
		return err
	})
	if err != nil {
		return "", fmt.Errorf("list the migrations: %w", err)
	}
	if len(names) == 0 {
		return "", errors.New("no Postgres migrations")
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		b, err := fs.ReadFile(fsys, n)
		if err != nil {
			return "", fmt.Errorf("read migration %s: %w", n, err)
		}
		fmt.Fprintf(h, "%s\x00%d\x00", n, len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}
