// Package storetest gives tests outside the store a fresh, migrated database, so that the
// backend a test runs against is chosen in one place rather than in every test file.
package storetest

import (
	"database/sql"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// New opens an empty, migrated database for the test and returns the store over it, with
// the raw handle for tests that seed or inspect rows directly. Both close when the test
// ends.
func New(t testing.TB) (*store.Store, *sql.DB) {
	t.Helper()
	return InDir(t, t.TempDir())
}

// InDir is New for a test that needs the database in a given data directory: one that
// later hands the directory to the daemon, or reopens it. The directory's database is
// migrated if it is not yet, and closed when the test ends.
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
