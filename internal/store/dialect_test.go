package store

import (
	"database/sql"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestRebindPlaceholders(t *testing.T) {
	dollar := func(n int) string { return "$" + strconv.Itoa(n) }
	vt, vu := sql.Named("viewer_tenant", int64(7)), sql.Named("viewer_user", int64(9))
	for _, tt := range []struct {
		name     string
		query    string
		args     []any
		want     string
		wantArgs []any
		err      string
	}{
		{name: "bare", query: `a = ? AND b = ?`, args: []any{1, 2}, want: `a = $1 AND b = $2`, wantArgs: []any{1, 2}},
		{name: "numbered, reused", query: `a = ?1 OR b = ?1 AND c = ?2`, args: []any{1, 2}, want: `a = $1 OR b = $1 AND c = $2`, wantArgs: []any{1, 2}},
		// As in SQLite, a bare ? is one more than the highest number before it.
		{name: "bare after numbered", query: `a = ?2 AND b = ?1 AND c = ?`, args: []any{1, 2, 3}, want: `a = $2 AND b = $1 AND c = $3`, wantArgs: []any{1, 2, 3}},
		{
			name:  "named after the positional ones, in first use",
			query: `t = @viewer_tenant AND id = ? AND (u = @viewer_user OR t2 = @viewer_tenant) AND x = ?`,
			args:  []any{vu, 1, vt, 2}, want: `t = $3 AND id = $1 AND (u = $4 OR t2 = $3) AND x = $2`, wantArgs: []any{1, 2, int64(7), int64(9)},
		},
		{name: "named with numbered", query: `@viewer_tenant = ?2 AND ?1`, args: []any{1, 2, vt}, want: `$3 = $2 AND $1`, wantArgs: []any{1, 2, int64(7)}},
		{
			name:  "quotes and comments are left alone",
			query: "a = '?it''s @x' AND \"col?\" = ? -- what? @y\n AND /* ?1 @z */ b = ?",
			args:  []any{1, 2}, want: "a = '?it''s @x' AND \"col?\" = $1 -- what? @y\n AND /* ?1 @z */ b = $2", wantArgs: []any{1, 2},
		},
		{name: "an @ that starts no name", query: `a = ? @ 1`, args: []any{1}, want: `a = $1 @ 1`, wantArgs: []any{1}},
		{name: "numbered past the arguments", query: `a = ?3`, args: []any{1, 2}, err: "missing argument with index 3"},
		{name: "positional left over", query: `a = ?`, args: []any{1, 2}, err: "2 positional arguments for 1 placeholders"},
		{name: "unbound name", query: `a = @viewer_user`, args: []any{vt}, err: `missing named argument "viewer_user"`},
		{name: "unused name", query: `a = ?`, args: []any{1, vt}, err: `named argument "viewer_tenant" is not used`},
		{name: "name given twice", query: `a = @viewer_tenant`, args: []any{vt, vt}, err: "given twice"},
		{name: "unterminated quote", query: `a = 'x`, args: []any{vt}, err: "unterminated quote"},
		{name: "unterminated comment", query: `a = /* x`, args: []any{vt}, err: "unterminated comment"},
		{name: "number zero", query: `a = ?0`, args: []any{1}, err: "bad placeholder"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, gotArgs, err := rebind(tt.query, tt.args, dollar)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want || !slices.Equal(gotArgs, tt.wantArgs) {
				t.Fatalf("rebind = %q %v, want %q %v", got, gotArgs, tt.want, tt.wantArgs)
			}
		})
	}
}

// SQLite takes the store's SQL as it is; only a named argument makes bind rewrite it.
func TestBindSQLite(t *testing.T) {
	q := `SELECT ? WHERE ?1 = 1`
	got, args, err := dialectSQLite.bind(q, []any{1})
	if err != nil || got != q || len(args) != 1 {
		t.Fatalf("bind = %q %v %v, want the query unchanged", got, args, err)
	}
	got, args, err = dialectSQLite.bind(`SELECT ?, @n, ?1`, []any{1, sql.Named("n", 2)})
	if err != nil || got != `SELECT ?1, ?2, ?1` || !slices.Equal(args, []any{1, 2}) {
		t.Fatalf("bind = %q %v %v", got, args, err)
	}
}

// A named argument binds where the query puts it, through the driver, in a query that also
// numbers its own parameters; a statement that cannot be bound fails before it runs.
func TestNamedArgumentsRun(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	var a, b, c int64
	if err := s.db.QueryRowContext(ctx, `SELECT CAST(?2 AS BIGINT), CAST(@x AS BIGINT), CAST(?1 AS BIGINT) + CAST(@x AS BIGINT)`,
		int64(1), int64(2), sql.Named("x", int64(40))).Scan(&a, &b, &c); err != nil {
		t.Fatal(err)
	}
	if a != 2 || b != 40 || c != 41 {
		t.Fatalf("got %d %d %d, want 2 40 41", a, b, c)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT @missing`, sql.Named("x", 1)).Scan(&a); err == nil {
		t.Fatal("an unbound name ran")
	}
	if _, err := s.db.ExecContext(ctx, `SELECT @missing`, sql.Named("x", 1)); err == nil {
		t.Fatal("an unbound name ran")
	}
	if _, err := s.db.QueryContext(ctx, `SELECT @missing`, sql.Named("x", 1)); err == nil {
		t.Fatal("an unbound name ran")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := tx.QueryRowContext(ctx, `SELECT CAST(@x AS BIGINT)`, sql.Named("x", int64(5))).Scan(&a); err != nil || a != 5 {
		t.Fatalf("in a transaction: %d, %v", a, err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT @missing`, sql.Named("x", 1)); err == nil {
		t.Fatal("an unbound name ran in a transaction")
	}
	if err := tx.QueryRowContext(ctx, `SELECT @missing`, sql.Named("x", 1)).Scan(&a); err == nil {
		t.Fatal("an unbound name ran in a transaction")
	}
	if _, err := tx.QueryContext(ctx, `SELECT @missing`, sql.Named("x", 1)); err == nil {
		t.Fatal("an unbound name ran in a transaction")
	}
}
