package store

import (
	"context"
	"testing"
)

func TestOpenAndMigrate(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, q := range []struct{ pragma, want string }{{"journal_mode", "wal"}, {"foreign_keys", "1"}} {
		var got string
		if err := db.QueryRowContext(ctx, "PRAGMA "+q.pragma).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != q.want {
			t.Errorf("%s = %s, want %s", q.pragma, got, q.want)
		}
	}
	// Running twice must be harmless.
	for range 2 {
		if err := Migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
}
