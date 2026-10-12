// Package storetest gives tests outside the store a fresh, migrated database, so that the
// backend a test runs against is chosen in one place rather than in every test file:
// Postgres when MAILRULES_TEST_POSTGRES_DSN is set (see pgtest), SQLite otherwise.
package storetest

import (
	"database/sql"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/store/pgtest"
)

// New opens an empty, migrated database for the test and returns the store over it, with
// the raw handle for tests that seed or inspect rows directly. Both close when the test
// ends.
//
// On Postgres the store runs as the app role, under row-level security, and the raw handle
// is the superuser, which row-level security does not apply to; it takes SQLite's ? and ?N
// placeholders, so seeding SQL runs on either backend.
func New(t testing.TB) (*store.Store, *sql.DB) {
	t.Helper()
	if pgtest.DSN() != "" {
		db := pgtest.Open(t)
		return store.New(db.App), db.Admin
	}
	return InDir(t, t.TempDir())
}

// InDir is New for a test that needs the database in a given data directory: one that
// later hands the directory to the daemon, or reopens it. The directory's database is
// migrated if it is not yet, and closed when the test ends.
//
// InDir is always SQLite, whatever the DSN: the daemon opens the database in its data
// directory itself, and does not yet take a Postgres DSN.
func InDir(t testing.TB, dir string) (*store.Store, *sql.DB) {
	t.Helper()
	ctx := t.Context()
	db, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	return store.New(db), db
}

// SQLiteOnly skips the test when it would run on Postgres; why says what it needs SQLite
// for.
func SQLiteOnly(t testing.TB, why string) {
	t.Helper()
	if pgtest.DSN() != "" {
		t.Skip("needs SQLite: " + why)
	}
}
