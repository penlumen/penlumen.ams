package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/penlumen/penlumen.ams/internal/db"
)

// Queries are written once, in SQL both databases understand, with $1-style
// parameters. Where they differ, a query uses one of these and run() swaps
// in the right SQL:
//
//	now()           the current time
//	{{forUpdate}}   lock the selected rows (Postgres; SQLite locks the file)
//
// SQLite keeps times as UTC text in one layout, so comparing the text
// compares the times; run() turns time arguments into that text.

// sqliteTime is how SQLite keeps times: what strftime('%Y-%m-%d %H:%M:%f',
// 'now') gives, in UTC.
const sqliteTime = "2006-01-02 15:04:05.000"

var dialects = map[db.Dialect]*strings.Replacer{
	db.Postgres: strings.NewReplacer(
		"{{forUpdate}}", "FOR UPDATE",
	),
	db.SQLite: strings.NewReplacer(
		"now()", "strftime('%Y-%m-%d %H:%M:%f', 'now')",
		"{{forUpdate}}", "",
	),
}

// SQLite numbers its parameters ?1, ?2…
var dollarParam = regexp.MustCompile(`\$(\d+)`)

// runner is the database or a transaction.
type runner interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// conn runs queries in the store's dialect, on the database or in a
// transaction.
type conn struct {
	r       runner
	dialect db.Dialect
}

func (c conn) prepare(query string, args []any) (string, []any) {
	query = dialects[c.dialect].Replace(query)
	if c.dialect != db.SQLite {
		return query, args
	}
	query = dollarParam.ReplaceAllString(query, "?$1")
	out := make([]any, len(args))
	for i, a := range args {
		switch v := a.(type) {
		case time.Time:
			out[i] = v.UTC().Format(sqliteTime)
		case *time.Time:
			if v != nil {
				out[i] = v.UTC().Format(sqliteTime)
			}
		default:
			out[i] = a
		}
	}
	return query, out
}

// exec runs a statement and says how many rows it changed.
func (c conn) exec(ctx context.Context, query string, args ...any) (int64, error) {
	query, args = c.prepare(query, args)
	res, err := c.r.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (c conn) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	query, args = c.prepare(query, args)
	return c.r.QueryContext(ctx, query, args...)
}

func (c conn) row(ctx context.Context, query string, args ...any) *sql.Row {
	query, args = c.prepare(query, args)
	return c.r.QueryRowContext(ctx, query, args...)
}

func (s *Store) c() conn { return conn{r: s.db, dialect: s.dialect} }

// begin starts a transaction; call Rollback (deferred) and Commit.
func (s *Store) begin(ctx context.Context) (*sql.Tx, conn, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, conn{}, err
	}
	return tx, conn{r: tx, dialect: s.dialect}, nil
}

// in returns "$n, $n+1, …" for values, to use in IN (…), and the arguments
// to add. With no values it matches nothing.
func in(n int, values []string) (string, []any) {
	if len(values) == 0 {
		return "NULL", nil
	}
	marks := make([]string, len(values))
	args := make([]any, len(values))
	for i, v := range values {
		marks[i] = fmt.Sprintf("$%d", n+i)
		args[i] = v
	}
	return strings.Join(marks, ", "), args
}

// nowMinusGrace is the oldest deadline that still accepts changes.
func nowMinusGrace() time.Time { return time.Now().Add(-Grace) }

// newID makes a random (version 4) uuid.
func newID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// jsonList reads and writes a list kept as JSON text.
type jsonList []string

func (l jsonList) Value() (driver.Value, error) {
	if l == nil {
		return "[]", nil
	}
	b, err := json.Marshal([]string(l))
	return string(b), err
}

func (l *jsonList) Scan(src any) error {
	b, err := jsonBytes(src)
	if err != nil || b == nil {
		*l = nil
		return err
	}
	return json.Unmarshal(b, (*[]string)(l))
}

// jsonMap reads and writes a string map kept as JSON text.
type jsonMap map[string]string

func (m jsonMap) Value() (driver.Value, error) {
	if m == nil {
		return "{}", nil
	}
	b, err := json.Marshal(map[string]string(m))
	return string(b), err
}

func (m *jsonMap) Scan(src any) error {
	*m = map[string]string{}
	b, err := jsonBytes(src)
	if err != nil || b == nil {
		return err
	}
	return json.Unmarshal(b, (*map[string]string)(m))
}

func (s Settings) Value() (driver.Value, error) {
	b, err := json.Marshal(s)
	return string(b), err
}

func (s *Settings) Scan(src any) error {
	*s = Settings{}
	b, err := jsonBytes(src)
	if err != nil || b == nil {
		return err
	}
	return json.Unmarshal(b, s)
}

func jsonBytes(src any) ([]byte, error) {
	switch v := src.(type) {
	case nil:
		return nil, nil
	case []byte:
		return v, nil
	case string:
		return []byte(v), nil
	}
	return nil, fmt.Errorf("can't read JSON from %T", src)
}

// sqlTime reads a time that SQLite returns as text (from max() and other
// expressions, which have no column type).
type sqlTime struct{ t **time.Time }

func (n sqlTime) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*n.t = nil
		return nil
	case time.Time:
		*n.t = &v
		return nil
	case string:
		return n.parse(v)
	case []byte:
		return n.parse(string(v))
	}
	return fmt.Errorf("can't read a time from %T", src)
}

func (n sqlTime) parse(s string) error {
	t, err := time.ParseInLocation(sqliteTime, s, time.UTC)
	if err != nil {
		return err
	}
	*n.t = &t
	return nil
}
