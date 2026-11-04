package query

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"sqlcli/internal/conn"
)

// Plan is the output of Explain. Text is set when the plan is a document
// (plan lines or JSON) meant for a text view; otherwise Result holds a table.
type Plan struct {
	Statement string
	Text      string
	Result    *Result
	Duration  time.Duration
}

// Explain returns the execution plan of one statement without running it.
//
// The readable form (raw == false) is the engine's text plan: EXPLAIN on
// PostgreSQL, EXPLAIN FORMAT=TREE on MySQL (the classic EXPLAIN table on
// MariaDB, which has no tree format) and DBMS_XPLAN.DISPLAY on Oracle.
//
// The raw form (raw == true) is the full machine-level plan: EXPLAIN in JSON
// on PostgreSQL / MariaDB / MySQL and the PLAN_TABLE rows on Oracle.
func Explain(ctx context.Context, db *sql.DB, t conn.DBType, stmt string, raw bool) (*Plan, error) {
	stmt = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(stmt), ";"))
	if stmt == "" {
		return nil, fmt.Errorf("empty statement")
	}
	start := time.Now()
	var p *Plan
	var err error
	switch t {
	case conn.Postgres:
		if raw {
			p, err = explainText(ctx, db, "EXPLAIN (FORMAT JSON) "+stmt)
		} else {
			p, err = explainText(ctx, db, "EXPLAIN "+stmt)
		}
	case conn.MariaDB:
		if raw {
			p, err = explainText(ctx, db, "EXPLAIN FORMAT=JSON "+stmt)
		} else {
			p, err = explainText(ctx, db, "EXPLAIN FORMAT=TREE "+stmt)
			if err != nil { // MariaDB / old MySQL: no tree format
				p, err = explainTable(ctx, db, "EXPLAIN "+stmt)
			}
		}
	case conn.Oracle:
		p, err = explainOracle(ctx, db, stmt, raw)
	default:
		return nil, fmt.Errorf("explain is not supported for %s", t)
	}
	if err != nil {
		return nil, err
	}
	p.Statement = stmt
	p.Duration = time.Since(start)
	return p, nil
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// explainText runs q and joins the first column of every row into lines.
// A single JSON document is pretty-printed.
func explainText(ctx context.Context, q querier, query string, args ...any) (*Plan, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	_, data, err := scanAll(rows, 0)
	if err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(data))
	for _, r := range data {
		if len(r) > 0 {
			lines = append(lines, r[0])
		}
	}
	return &Plan{Text: prettyJSON(strings.Join(lines, "\n"))}, nil
}

func explainTable(ctx context.Context, q querier, query string, args ...any) (*Plan, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, data, err := scanAll(rows, 0)
	if err != nil {
		return nil, err
	}
	return &Plan{Result: &Result{Statement: query, Columns: cols, Rows: data, IsQuery: true}}, nil
}

// explainOracle uses one session: EXPLAIN PLAN writes into the session's
// PLAN_TABLE, which is then read back and cleaned up.
func explainOracle(ctx context.Context, db *sql.DB, stmt string, raw bool) (*Plan, error) {
	c, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()

	id := fmt.Sprintf("sqlcli_%d", time.Now().UnixNano())
	if _, err := c.ExecContext(ctx, fmt.Sprintf("EXPLAIN PLAN SET STATEMENT_ID = '%s' FOR %s", id, stmt)); err != nil {
		return nil, err
	}
	defer c.ExecContext(context.Background(), "DELETE FROM plan_table WHERE statement_id = :1", id) //nolint:errcheck

	if raw {
		return explainTable(ctx, c, "SELECT * FROM plan_table WHERE statement_id = :1 ORDER BY id", id)
	}
	return explainText(ctx, c, "SELECT plan_table_output FROM TABLE(DBMS_XPLAN.DISPLAY('PLAN_TABLE', :1, 'TYPICAL'))", id)
}

func prettyJSON(s string) string {
	t := strings.TrimSpace(s)
	if t == "" || (t[0] != '{' && t[0] != '[') {
		return s
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(t), "", "  "); err != nil {
		return s
	}
	return buf.String()
}
