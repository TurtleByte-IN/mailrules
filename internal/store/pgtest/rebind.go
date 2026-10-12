package pgtest

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strconv"
	"strings"
)

// rebinding is a connector whose connections rewrite SQLite placeholders (? and ?N) to
// Postgres ones ($n) before a query reaches the server.
type rebinding struct{ driver.Connector }

func (c rebinding) Connect(ctx context.Context) (driver.Conn, error) {
	dc, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	pc, ok := dc.(pgxConn)
	if !ok {
		_ = dc.Close()
		return nil, fmt.Errorf("pgtest: %T lacks an interface the rebinding connection forwards", dc)
	}
	return conn{pc}, nil
}

// pgxConn is every optional interface pgx's stdlib connection implements.
type pgxConn interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.ExecerContext
	driver.QueryerContext
	driver.NamedValueChecker
	driver.Pinger
	driver.SessionResetter
}

type conn struct{ pgxConn }

var (
	_ pgxConn          = conn{}
	_ driver.Validator = conn{}
)

func (c conn) Prepare(query string) (driver.Stmt, error) {
	return c.pgxConn.Prepare(rebind(query))
}

func (c conn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	return c.pgxConn.PrepareContext(ctx, rebind(query))
}

func (c conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.pgxConn.ExecContext(ctx, rebind(query), args)
}

func (c conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.pgxConn.QueryContext(ctx, rebind(query), args)
}

// IsValid forwards to the connection when it is a Validator; database/sql treats one that
// is not as always valid.
func (c conn) IsValid() bool {
	if v, ok := c.pgxConn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

// rebind rewrites ?N to $N and a bare ? to one more than the highest number before it, as
// SQLite numbers them. Text in quotes ('…', "…") and comments (-- …, /* … */) is left alone.
func rebind(q string) string {
	if !strings.Contains(q, "?") {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 8)
	highest := 0
	for i := 0; i < len(q); {
		switch c := q[i]; {
		case c == '\'' || c == '"':
			end := closing(q, i+1, c)
			b.WriteString(q[i:end])
			i = end
		case c == '-' && strings.HasPrefix(q[i:], "--"):
			end := strings.IndexByte(q[i:], '\n')
			if end < 0 {
				end = len(q) - i
			}
			b.WriteString(q[i : i+end])
			i += end
		case c == '/' && strings.HasPrefix(q[i:], "/*"):
			end := strings.Index(q[i+2:], "*/")
			if end < 0 {
				end = len(q) - i
			} else {
				end += 4
			}
			b.WriteString(q[i : i+end])
			i += end
		case c == '?':
			j := i + 1
			for j < len(q) && q[j] >= '0' && q[j] <= '9' {
				j++
			}
			n := highest + 1
			if j > i+1 {
				n, _ = strconv.Atoi(q[i+1 : j])
			}
			highest = max(highest, n)
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			i = j
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// closing is the index just past the quote that closes the one opened before start; a
// doubled quote is an escaped one. An unclosed quote runs to the end.
func closing(q string, start int, quote byte) int {
	for i := start; i < len(q); i++ {
		if q[i] == quote {
			if i+1 < len(q) && q[i+1] == quote {
				i++
				continue
			}
			return i + 1
		}
	}
	return len(q)
}
