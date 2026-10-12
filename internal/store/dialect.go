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
// store's SQL into what the database's driver takes (bind) and, on Postgres, scopes each
// statement to its tenant (see tenant.go), rather than some 300 call sites doing either.
//
// The store's SQL uses SQLite's placeholders: ? (the next argument), ?N (argument N, for a
// value used more than once) and @name, bound by a sql.Named argument. A name is how a
// condition built apart from its query (visibleAccounts) brings its own values without
// knowing their position in the query. The SQL itself is what both dialects accept; the
// few spellings that differ come from the dialect's fragment methods below.

// dialect is the SQL flavour of the database behind a store.
type dialect int

const (
	dialectSQLite dialect = iota
	dialectPostgres
)

// querier is the database or a transaction on it: what a statement shared by both runs on.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *row
}

// rows is the result of a query that returns rows; close it when done. On Postgres a scoped
// query outside a transaction runs in one of its own, which Close ends.
type rows struct {
	*sql.Rows
	tx *sql.Tx
}

// Close closes the rows, and ends the transaction the query ran in, if it had its own.
func (r *rows) Close() error {
	err := r.Rows.Close()
	if r.tx != nil {
		err = errors.Join(err, r.tx.Commit())
		r.tx = nil
	}
	return err
}

// row is the result of a query for one row; Scan reports sql.ErrNoRows when there was none,
// and the error when the query could not be run.
type row struct {
	r   *sql.Row
	err error
	tx  *sql.Tx // as for rows: Scan ends it
}

// Scan copies the row's columns into dest.
func (r *row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	err := r.r.Scan(dest...)
	if r.tx != nil {
		if cerr := r.tx.Commit(); cerr != nil && (err == nil || errors.Is(err, sql.ErrNoRows)) {
			err = cerr
		}
		r.tx = nil
	}
	return err
}

// conn is the store's database.
type conn struct {
	db *sql.DB
	d  dialect
}

var _ querier = (*conn)(nil)

// own begins the transaction a scoped statement runs in on Postgres, or returns nil when the
// statement runs as it is: on SQLite, which keeps no tenant apart, and for an unscoped ctx,
// which on Postgres sees no tenant's rows. write says the statement may change rows.
func (c *conn) own(ctx context.Context, write bool) (*sql.Tx, error) {
	tenant, err := scopeOf(ctx)
	if err != nil || c.d != dialectPostgres || tenant == 0 {
		return nil, err
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if err := enter(ctx, tx, tenant, write); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

// enter scopes the Postgres transaction tx to tenant: the policies then show it only that
// tenant's rows. app.tenant_id is set for this transaction alone, so a pooled connection
// carries nothing over to the next. write also takes the tenant's lock, held to the end of
// the transaction, so a tenant's writers run one at a time, as SQLite's single writer made
// them: a check such as "no live batch yet today" or "under the mailbox limit" holds until
// the commit.
func enter(ctx context.Context, tx *sql.Tx, tenant int64, write bool) error {
	q := `SELECT set_config('app.tenant_id', $1, true)`
	args := []any{strconv.FormatInt(tenant, 10)}
	if write {
		q, args = `SELECT set_config('app.tenant_id', $1, true), pg_advisory_xact_lock($2)`, append(args, tenant)
	}
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("scope to tenant %d: %w", tenant, err)
	}
	return nil
}

func (c *conn) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	q, a, err := c.d.bind(query, args)
	if err != nil {
		return nil, err
	}
	tx, err := c.own(ctx, true)
	if err != nil {
		return nil, err
	}
	if tx == nil {
		return c.db.ExecContext(ctx, q, a...)
	}
	res, err := tx.ExecContext(ctx, q, a...)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return res, tx.Commit()
}

func (c *conn) QueryContext(ctx context.Context, query string, args ...any) (*rows, error) {
	q, a, err := c.d.bind(query, args)
	if err != nil {
		return nil, err
	}
	tx, err := c.own(ctx, false)
	if err != nil {
		return nil, err
	}
	if tx == nil {
		r, err := c.db.QueryContext(ctx, q, a...)
		if err != nil {
			return nil, err
		}
		return &rows{Rows: r}, nil
	}
	r, err := tx.QueryContext(ctx, q, a...)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return &rows{Rows: r, tx: tx}, nil
}

// QueryRowContext runs a query for one row. A write through it (INSERT … RETURNING) does
// not take the tenant's lock: statements that must not interleave with another writer's run
// in one transaction (BeginTx).
func (c *conn) QueryRowContext(ctx context.Context, query string, args ...any) *row {
	q, a, err := c.d.bind(query, args)
	if err != nil {
		return &row{err: err}
	}
	tx, err := c.own(ctx, false)
	if err != nil {
		return &row{err: err}
	}
	if tx == nil {
		return &row{r: c.db.QueryRowContext(ctx, q, a...)}
	}
	return &row{r: tx.QueryRowContext(ctx, q, a...), tx: tx}
}

// PingContext reports whether the database answers.
func (c *conn) PingContext(ctx context.Context) error { return c.db.PingContext(ctx) }

// BeginTx begins a transaction scoped to ctx's tenant; one that is not read-only holds the
// tenant's lock (enter). See Open for how SQLite's lock is taken. A transaction begun with
// no tenant (a sign-in, before the tenant is known) is scoped later with scope.
func (c *conn) BeginTx(ctx context.Context, opts *sql.TxOptions) (*txn, error) {
	tenant, err := scopeOf(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := c.db.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	t := &txn{tx: tx, d: c.d}
	if tenant != 0 {
		if err := t.scope(ctx, tenant, opts == nil || !opts.ReadOnly); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
	}
	return t, nil
}

// txn is a transaction on the store's database.
type txn struct {
	tx *sql.Tx
	d  dialect
}

var _ querier = (*txn)(nil)

// scope scopes the transaction to tenant from here on, as BeginTx does; a no-op on SQLite.
func (t *txn) scope(ctx context.Context, tenant int64, write bool) error {
	if t.d != dialectPostgres {
		return nil
	}
	return enter(ctx, t.tx, tenant, write)
}

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
	return &rows{Rows: r}, nil
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

// bind turns the store's SQL and arguments into the driver's. On SQLite, with no named
// argument the SQL is already the driver's and passes unchanged; named arguments are
// numbered after the positional ones, so the driver sees positions only. On Postgres every
// placeholder becomes $n.
func (d dialect) bind(query string, args []any) (string, []any, error) {
	if d == dialectPostgres {
		return rebind(query, args, func(n int) string { return "$" + strconv.Itoa(n) })
	}
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

// The fragments below are the spellings the two dialects do not share.

// jsonText is the text at key of the JSON object in column col, NULL when absent.
func (d dialect) jsonText(col, key string) string {
	if d == dialectPostgres {
		return "(" + col + "::jsonb ->> '" + key + "')"
	}
	return "json_extract(" + col + ", '$." + key + "')"
}

// jsonFlag is 1 where the JSON object in column col holds true at key, else 0.
func (d dialect) jsonFlag(col, key string) string {
	if d == dialectPostgres {
		return "(CASE WHEN (" + col + "::jsonb -> '" + key + "') = 'true'::jsonb THEN 1 ELSE 0 END)"
	}
	return "COALESCE(json_extract(" + col + ", '$." + key + "'), 0)"
}

// jsonValues is a subquery of the elements of the JSON array bound at placeholder p, as
// SQL type typ (BIGINT or TEXT), in a column named value.
func (d dialect) jsonValues(p, typ string) string {
	if d == dialectPostgres {
		return "SELECT CAST(value AS " + typ + ") AS value FROM json_array_elements_text(CAST(" + p + " AS json))"
	}
	return "SELECT value FROM json_each(" + p + ")"
}

// greatest is the larger of two values that are never NULL.
func (d dialect) greatest(a, b string) string {
	if d == dialectPostgres {
		return "GREATEST(" + a + ", " + b + ")"
	}
	return "MAX(" + a + ", " + b + ")"
}

// insertOrder is the column of identities that orders its rows as they were inserted.
func (d dialect) insertOrder() string {
	if d == dialectPostgres {
		return "id"
	}
	return "rowid"
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
