// Package store owns the SQLite database: opening it, migrating it and, later, the repositories.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

//go:embed migrations/*.sql
var migrations embed.FS

// Open opens (creating if needed) the database in dataDir, in WAL mode. Every transaction
// not begun read-only begins IMMEDIATE, holding the write lock from its first statement:
// a deferred one that read first could not write once another connection had committed
// meanwhile (SQLITE_BUSY_SNAPSHOT), which mailbox supervisors do all the time. Begin one
// that only reads with sql.TxOptions{ReadOnly: true}, so that it waits for no writer.
func Open(ctx context.Context, dataDir string) (*sql.DB, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	dsn := "file:" + filepath.Join(dataDir, "mailrules.db") +
		"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate"
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

// Migrate applies every embedded migration that has not run yet.
func Migrate(ctx context.Context, db *sql.DB) error {
	fsys, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, db, fsys)
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}
