package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Every statement the store runs goes through conn (the database) or txn (a transaction on
// it), never through *sql.DB or *sql.Tx directly. They are the one place that turns the
// store's SQL into what the database's driver takes (bind), so that a second SQL dialect
// (Postgres, MAI-150) changes the placeholders, and scopes each statement to its tenant,
// here rather than at some 300 call sites.
//
// The store's SQL uses SQLite's placeholders: ? (the next argument), ?N (argument N, for a
// value used more than once) and @name, bound by a sql.Named argument. A name is how a
// condition built apart from its query (visibleAccounts) brings its own values without
// knowing their position in the query.

// dialect is the SQL flavour of the database behind a store.
type dialect int

const dialectSQLite dialect = iota

// querier is the database or a transaction on it: what a statement shared by both runs on.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *row
}

// rows is the result of a query that returns rows; close it when done.
type rows struct{ *sql.Rows }

// row is the result of a query for one row; Scan reports sql.ErrNoRows when there was none,
// and the error when the query could not be bound.
type row struct {
	r   *sql.Row
	err error
}

// Scan copies the row's columns into dest.
func (r *row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return r.r.Scan(dest...)
}

// conn is the store's database.
type conn struct {
	db *sql.DB
	d  dialect
}

var _ querier = (*conn)(nil)

func (c *conn) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	q, a, err := c.d.bind(query, args)
	if err != nil {
		return nil, err
	}
	return c.db.ExecContext(ctx, q, a...)
}

func (c *conn) QueryContext(ctx context.Context, query string, args ...any) (*rows, error) {
	q, a, err := c.d.bind(query, args)
	if err != nil {
		return nil, err
	}
	r, err := c.db.QueryContext(ctx, q, a...)
	if err != nil {
		return nil, err
	}
	return &rows{r}, nil
}

func (c *conn) QueryRowContext(ctx context.Context, query string, args ...any) *row {
	q, a, err := c.d.bind(query, args)
	if err != nil {
		return &row{err: err}
	}
	return &row{r: c.db.QueryRowContext(ctx, q, a...)}
}

// PingContext reports whether the database answers.
func (c *conn) PingContext(ctx context.Context) error { return c.db.PingContext(ctx) }

// BeginTx begins a transaction; see Open for how SQLite's lock is taken.
func (c *conn) BeginTx(ctx context.Context, opts *sql.TxOptions) (*txn, error) {
	tx, err := c.db.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &txn{tx: tx, d: c.d}, nil
}

// txn is a transaction on the store's database.
type txn struct {
	tx *sql.Tx
	d  dialect
}

var _ querier = (*txn)(nil)

func (t *txn) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	q, a, err := t.d.bind(query, args)
	if err != nil {
		return nil, err
	}
	return t.tx.ExecContext(ctx, q, a...)
}

func (t *txn) QueryContext(ctx context.Context, query string, args ...any) (*rows, error) {
	q, a, err := t.d.bind(query, args)
	if err != nil {
		return nil, err
	}
	r, err := t.tx.QueryContext(ctx, q, a...)
	if err != nil {
		return nil, err
	}
	return &rows{r}, nil
}

func (t *txn) QueryRowContext(ctx context.Context, query string, args ...any) *row {
	q, a, err := t.d.bind(query, args)
	if err != nil {
		return &row{err: err}
	}
	return &row{r: t.tx.QueryRowContext(ctx, q, a...)}
}

// Commit commits the transaction.
func (t *txn) Commit() error { return t.tx.Commit() }

// Rollback abandons the transaction; after Commit it is a no-op that reports sql.ErrTxDone.
func (t *txn) Rollback() error { return t.tx.Rollback() }

// bind turns the store's SQL and arguments into the driver's. With no named argument the
// SQL is already SQLite's and passes unchanged. Named arguments are rewritten to numbered
// placeholders after the positional ones, so the driver sees positions only.
func (d dialect) bind(query string, args []any) (string, []any, error) {
	named := false
	for _, a := range args {
		if _, ok := a.(sql.NamedArg); ok {
			named = true
			break
		}
	}
	if !named {
		return query, args, nil
	}
	return rebind(query, args, func(n int) string { return "?" + strconv.Itoa(n) })
}

// rebind rewrites every placeholder in query to placeholder(n), where n numbers the
// returned arguments: the positional ones first, in order, then each named one. It skips
// string literals, quoted identifiers and comments. It fails, as the driver would, when a
// positional argument is missing or left over, and when a name is unbound or unused.
func rebind(query string, args []any, placeholder func(n int) string) (string, []any, error) {
	var positional []any
	names := map[string]any{}
	for _, a := range args {
		if na, ok := a.(sql.NamedArg); ok {
			if _, dup := names[na.Name]; dup {
				return "", nil, fmt.Errorf("bind: named argument %q given twice", na.Name)
			}
			names[na.Name] = na.Value
			continue
		}
		positional = append(positional, a)
	}

	var b strings.Builder
	b.Grow(len(query) + 16)
	highest := 0             // the highest positional number so far: a bare ? is the next, as in SQLite
	used := map[string]int{} // name to its number, after the positional ones
	var order []string       // names by first use
	for i := 0; i < len(query); {
		c := query[i]
		switch {
		case c == '\'' || c == '"':
			j := i + 1
			for j < len(query) {
				if query[j] == c {
					if j+1 < len(query) && query[j+1] == c { // a doubled quote escapes it
						j += 2
						continue
					}
					break
				}
				j++
			}
			if j >= len(query) {
				return "", nil, errors.New("bind: unterminated quote")
			}
			b.WriteString(query[i : j+1])
			i = j + 1
		case c == '-' && i+1 < len(query) && query[i+1] == '-':
			j := strings.IndexByte(query[i:], '\n')
			if j < 0 {
				j = len(query) - i
			}
			b.WriteString(query[i : i+j])
			i += j
		case c == '/' && i+1 < len(query) && query[i+1] == '*':
			j := strings.Index(query[i+2:], "*/")
			if j < 0 {
				return "", nil, errors.New("bind: unterminated comment")
			}
			b.WriteString(query[i : i+2+j+2])
			i += 2 + j + 2
		case c == '?':
			j := i + 1
			for j < len(query) && query[j] >= '0' && query[j] <= '9' {
				j++
			}
			n := highest + 1
			if j > i+1 {
				var err error
				if n, err = strconv.Atoi(query[i+1 : j]); err != nil || n < 1 {
					return "", nil, fmt.Errorf("bind: bad placeholder %q", query[i:j])
				}
			}
			if n > len(positional) {
				return "", nil, fmt.Errorf("bind: missing argument with index %d", n)
			}
			highest = max(highest, n)
			b.WriteString(placeholder(n))
			i = j
		case c == '@' && i+1 < len(query) && isNameStart(query[i+1]):
			j := i + 1
			for j < len(query) && isNamePart(query[j]) {
				j++
			}
			name := query[i+1 : j]
			if _, ok := names[name]; !ok {
				return "", nil, fmt.Errorf("bind: missing named argument %q", name)
			}
			n, ok := used[name]
			if !ok {
				order = append(order, name)
				n = len(positional) + len(order)
				used[name] = n
			}
			b.WriteString(placeholder(n))
			i = j
		default:
			b.WriteByte(c)
			i++
		}
	}

	if highest != len(positional) {
		return "", nil, fmt.Errorf("bind: %d positional arguments for %d placeholders", len(positional), highest)
	}
	if len(order) != len(names) {
		for name := range names {
			if _, ok := used[name]; !ok {
				return "", nil, fmt.Errorf("bind: named argument %q is not used", name)
			}
		}
	}
	out := positional
	for _, name := range order {
		out = append(out, names[name])
	}
	return b.String(), out, nil
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isNamePart(c byte) bool { return isNameStart(c) || (c >= '0' && c <= '9') }
