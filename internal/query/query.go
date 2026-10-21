// Package query executes SQL statements and returns normalized results.
package query

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"sqlcli/internal/conn"
)

// Result is a normalized result of one statement.
type Result struct {
	Statement    string
	Columns      []string
	Rows         [][]string
	RowsAffected int64
	IsQuery      bool
	Duration     time.Duration
	Err          error
}

// Split splits a script into individual statements on ';' while respecting
// single/double quotes, line comments (--) and block comments (/* */).
// Oracle PL/SQL blocks (BEGIN ... END; or DECLARE ...) are kept whole and
// terminated by a line containing only "/".
func Split(script string) []string {
	var out []string
	var cur strings.Builder
	inSingle, inDouble, inLine, inBlock := false, false, false, false
	plsqlDepth := 0

	flush := func() {
		s := strings.TrimSpace(cur.String())
		cur.Reset()
		if s != "" {
			out = append(out, s)
		}
	}

	runes := []rune(script)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		next := rune(0)
		if i+1 < len(runes) {
			next = runes[i+1]
		}
		switch {
		case inLine:
			cur.WriteRune(r)
			if r == '\n' {
				inLine = false
			}
		case inBlock:
			cur.WriteRune(r)
			if r == '*' && next == '/' {
				cur.WriteRune(next)
				i++
				inBlock = false
			}
		case inSingle:
			cur.WriteRune(r)
			if r == '\'' {
				inSingle = false
			}
		case inDouble:
			cur.WriteRune(r)
			if r == '"' {
				inDouble = false
			}
		case r == '-' && next == '-':
			inLine = true
			cur.WriteRune(r)
		case r == '/' && next == '*':
			inBlock = true
			cur.WriteRune(r)
		case r == '\'':
			inSingle = true
			cur.WriteRune(r)
		case r == '"':
			inDouble = true
			cur.WriteRune(r)
		case r == '/' && isLineAlone(runes, i):
			// Oracle terminator for PL/SQL blocks.
			plsqlDepth = 0
			flush()
		case r == ';':
			if plsqlDepth > 0 {
				cur.WriteRune(r)
			} else {
				flush()
			}
		default:
			cur.WriteRune(r)
			// Track PL/SQL block start so inner ';' don't split.
			if plsqlDepth == 0 {
				head := strings.ToUpper(strings.TrimSpace(cur.String()))
				if head == "BEGIN" || head == "DECLARE" || strings.HasPrefix(head, "CREATE OR REPLACE PROCEDURE") ||
					strings.HasPrefix(head, "CREATE OR REPLACE FUNCTION") || strings.HasPrefix(head, "CREATE OR REPLACE PACKAGE") ||
					strings.HasPrefix(head, "CREATE OR REPLACE TRIGGER") || strings.HasPrefix(head, "CREATE PROCEDURE") ||
					strings.HasPrefix(head, "CREATE FUNCTION") || strings.HasPrefix(head, "CREATE TRIGGER") {
					if len(head) == 5 || len(head) == 7 || strings.HasSuffix(head, " ") || r == '\n' || r == ' ' {
						plsqlDepth = 1
					}
				}
			}
		}
	}
	flush()
	return out
}

// isLineAlone reports whether runes[i] is the only non-space char on its line.
func isLineAlone(runes []rune, i int) bool {
	for j := i - 1; j >= 0 && runes[j] != '\n'; j-- {
		if runes[j] != ' ' && runes[j] != '\t' && runes[j] != '\r' {
			return false
		}
	}
	for j := i + 1; j < len(runes) && runes[j] != '\n'; j++ {
		if runes[j] != ' ' && runes[j] != '\t' && runes[j] != '\r' {
			return false
		}
	}
	return true
}

// IsQuery guesses whether a statement returns rows.
func IsQuery(stmt string) bool {
	s := strings.ToUpper(strings.TrimSpace(stripLeadingComments(stmt)))
	for _, kw := range []string{"SELECT", "WITH", "SHOW", "DESCRIBE", "DESC ", "EXPLAIN", "VALUES", "TABLE ", "PRAGMA", "CALL"} {
		if strings.HasPrefix(s, kw) {
			return true
		}
	}
	// DML with RETURNING returns rows.
	if strings.Contains(s, " RETURNING ") && (strings.HasPrefix(s, "INSERT") || strings.HasPrefix(s, "UPDATE") || strings.HasPrefix(s, "DELETE")) {
		return true
	}
	return false
}

func stripLeadingComments(s string) string {
	for {
		s = strings.TrimSpace(s)
		switch {
		case strings.HasPrefix(s, "--"):
			if i := strings.Index(s, "\n"); i >= 0 {
				s = s[i+1:]
			} else {
				return ""
			}
		case strings.HasPrefix(s, "/*"):
			if i := strings.Index(s, "*/"); i >= 0 {
				s = s[i+2:]
			} else {
				return ""
			}
		default:
			return s
		}
	}
}

// Exec runs one statement and returns its result. maxRows <= 0 means unlimited.
func Exec(ctx context.Context, db *sql.DB, stmt string, maxRows int) *Result {
	res := &Result{Statement: stmt}
	start := time.Now()
	defer func() { res.Duration = time.Since(start) }()

	if IsQuery(stmt) {
		res.IsQuery = true
		rows, err := db.QueryContext(ctx, stmt)
		if err != nil {
			res.Err = err
			return res
		}
		defer rows.Close()
		res.Columns, res.Rows, res.Err = scanAll(rows, maxRows)
		return res
	}

	r, err := db.ExecContext(ctx, stmt)
	if err != nil {
		res.Err = err
		return res
	}
	if n, err := r.RowsAffected(); err == nil {
		res.RowsAffected = n
	}
	return res
}

// ExecScript splits and runs every statement, stopping at the first error
// unless continueOnError is set.
func ExecScript(ctx context.Context, db *sql.DB, script string, maxRows int, continueOnError bool) []*Result {
	var out []*Result
	for _, stmt := range Split(script) {
		r := Exec(ctx, db, stmt, maxRows)
		out = append(out, r)
		if r.Err != nil && !continueOnError {
			break
		}
	}
	return out
}

func scanAll(rows *sql.Rows, maxRows int) ([]string, [][]string, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}
	var out [][]string
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if maxRows > 0 && len(out) >= maxRows {
			break
		}
		if err := rows.Scan(ptrs...); err != nil {
			return cols, out, err
		}
		row := make([]string, len(cols))
		for i, v := range vals {
			row[i] = FormatValue(v)
		}
		out = append(out, row)
	}
	return cols, out, rows.Err()
}

// FormatValue converts a scanned driver value to a display string.
func FormatValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return string(x)
	case time.Time:
		if x.Hour() == 0 && x.Minute() == 0 && x.Second() == 0 && x.Nanosecond() == 0 {
			return x.Format("2006-01-02")
		}
		return x.Format("2006-01-02 15:04:05.000")
	case string:
		return x
	case fmt.Stringer:
		return x.String()
	case float64:
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%f", x), "0"), ".")
	case float32:
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%f", x), "0"), ".")
	default:
		return fmt.Sprint(x)
	}
}

// ListTables returns a list of tables visible to the current user.
func ListTables(ctx context.Context, db *sql.DB, t conn.DBType) ([]string, error) {
	var q string
	switch t {
	case conn.Postgres:
		q = `SELECT table_schema || '.' || table_name FROM information_schema.tables
		     WHERE table_schema NOT IN ('pg_catalog','information_schema') ORDER BY 1`
	case conn.MariaDB:
		q = `SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() ORDER BY 1`
	case conn.Oracle:
		q = `SELECT table_name FROM user_tables ORDER BY 1`
	default:
		return nil, fmt.Errorf("unsupported type %s", t)
	}
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return out, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DescribeTable returns column metadata for a table.
func DescribeTable(ctx context.Context, db *sql.DB, t conn.DBType, table string) *Result {
	var q string
	switch t {
	case conn.Postgres:
		schema, name := "public", table
		if s, n, ok := strings.Cut(table, "."); ok {
			schema, name = s, n
		}
		q = fmt.Sprintf(`SELECT column_name, data_type, is_nullable, column_default
		     FROM information_schema.columns WHERE table_schema = '%s' AND table_name = '%s' ORDER BY ordinal_position`,
			escape(schema), escape(name))
	case conn.MariaDB:
		q = fmt.Sprintf("SHOW FULL COLUMNS FROM `%s`", strings.ReplaceAll(table, "`", "``"))
	case conn.Oracle:
		q = fmt.Sprintf(`SELECT column_name, data_type, data_length, nullable, data_default
		     FROM user_tab_columns WHERE table_name = '%s' ORDER BY column_id`, escape(strings.ToUpper(table)))
	}
	return Exec(ctx, db, q, 0)
}

func escape(s string) string { return strings.ReplaceAll(s, "'", "''") }
