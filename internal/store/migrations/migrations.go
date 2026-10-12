// Package migrations holds the store's schema migrations, one set per SQL dialect, embedded
// into the binary and applied by goose.
//
// The sets are numbered alike: a change is one file in each, under the same version, so the
// version a database reports means the same schema on either. SQLite's set is the store's
// history from 0001; Postgres's starts at the baseline 0023, the SQLite schema as of 0023 with
// tenant isolation added (MAI-150), since no Postgres database predates it. A file must
// never be renumbered or removed once released: goose knows a migration by its number alone.
package migrations

import (
	"embed"
	"io/fs"
)

//go:embed sqlite/*.sql
var sqlite embed.FS

//go:embed postgres/*.sql
var postgres embed.FS

// SQLite is the SQLite set.
func SQLite() fs.FS { return sub(sqlite, "sqlite") }

// Postgres is the Postgres set.
func Postgres() fs.FS { return sub(postgres, "postgres") }

func sub(f embed.FS, dir string) fs.FS {
	s, err := fs.Sub(f, dir)
	if err != nil {
		panic(err) // dir is a literal that go:embed has checked exists
	}
	return s
}
