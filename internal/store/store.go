// Package store owns the database: opening it, migrating it and the repositories over it.
// Self-host runs on SQLite, one file in the data directory; the hosted build can run on
// Postgres, where row-level security keeps each tenant's rows to that tenant (MAI-150).
package store

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/TurtleByte-IN/mailrules/internal/store/migrations"
)

// Open opens (creating if needed) the SQLite database in dataDir, in WAL mode. Every
// transaction not begun read-only begins IMMEDIATE, holding the write lock from its first
// statement: a deferred one that read first could not write once another connection had
// committed meanwhile (SQLITE_BUSY_SNAPSHOT), which mailbox supervisors do all the time.
// Begin one that only reads with sql.TxOptions{ReadOnly: true}, so that it waits for no
// writer.
func Open(ctx context.Context, dataDir string) (*sql.DB, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	dsn, err := dataSource(filepath.Join(dataDir, "mailrules.db"))
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	return db, nil
}

// OpenPostgres connects to the Postgres database at dsn (a postgres:// URL or key=value
// string). The role it signs in as must not own the tables nor bypass row-level security:
// the store relies on the policies to keep tenants apart. Migrations run as the owner.
func OpenPostgres(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	return db, nil
}

// dataSource is the SQLite URI of the database file at path, with the connection settings.
// The path is made absolute and escaped: in a URI "#" would end it, "?" would start SQLite's
// own parameters and "%" would start an escape, so a data dir holding one would open (or
// make) another database (MAI-172).
func dataSource(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("database path: %w", err)
	}
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // a Windows drive: file:///C:/...
	}
	u := url.URL{Scheme: "file", Path: p, RawQuery: "_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate"}
	return u.String(), nil
}

// dialectOf is the SQL dialect of the database behind db, told by its driver.
func dialectOf(db *sql.DB) dialect {
	if _, ok := db.Driver().(*stdlib.Driver); ok {
		return dialectPostgres
	}
	return dialectSQLite
}

// Migrate applies every embedded migration of the database's dialect that has not run yet.
// On Postgres it runs as the role that owns the tables, and holds a session lock so two
// processes starting at once do not both migrate.
func Migrate(ctx context.Context, db *sql.DB) error {
	var (
		d    goose.Dialect
		fsys fs.FS
		opts []goose.ProviderOption
	)
	switch dialectOf(db) {
	case dialectPostgres:
		locker, err := lock.NewPostgresSessionLocker()
		if err != nil {
			return fmt.Errorf("load migrations: %w", err)
		}
		d, fsys, opts = goose.DialectPostgres, migrations.Postgres(), []goose.ProviderOption{goose.WithSessionLocker(locker)}
	default:
		d, fsys = goose.DialectSQLite3, migrations.SQLite()
	}
	p, err := goose.NewProvider(d, db, fsys, opts...)
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}
