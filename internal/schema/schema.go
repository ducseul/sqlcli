// Package schema introspects database tables/columns, caches them on disk and
// computes autocomplete suggestions from them.
package schema

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"sqlcli/internal/conn"
	"sqlcli/internal/query"
)

// cacheVersion is bumped whenever the cache file layout changes.
const cacheVersion = 1

// Column is one table column.
type Column struct {
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
}

// Table is one table or view. Name has the same form as query.ListTables
// (schema-qualified for PostgreSQL, bare for MariaDB/Oracle).
type Table struct {
	Name    string   `json:"name"`
	Columns []Column `json:"columns,omitempty"`
}

// Schema is the introspected metadata of one connection.
type Schema struct {
	Version   int       `json:"version"`
	DBType    string    `json:"db_type"`
	Generated time.Time `json:"generated"`
	Tables    []Table   `json:"tables"`

	byName map[string]*Table // lower-cased full and short names -> table
}

// TableNames returns the table names in order.
func (s *Schema) TableNames() []string {
	out := make([]string, len(s.Tables))
	for i, t := range s.Tables {
		out[i] = t.Name
	}
	return out
}

// ColumnCount returns the total number of columns.
func (s *Schema) ColumnCount() int {
	n := 0
	for _, t := range s.Tables {
		n += len(t.Columns)
	}
	return n
}

// index (re)builds the name lookup map.
func (s *Schema) index() {
	s.byName = make(map[string]*Table, len(s.Tables)*2)
	for i := range s.Tables {
		t := &s.Tables[i]
		full := strings.ToLower(t.Name)
		s.byName[full] = t
		if _, short, ok := strings.Cut(full, "."); ok {
			if _, dup := s.byName[short]; !dup {
				s.byName[short] = t
			}
		}
	}
}

// Lookup finds a table by full or unqualified name, case-insensitively.
// Surrounding identifier quotes are ignored.
func (s *Schema) Lookup(name string) *Table {
	if s == nil {
		return nil
	}
	if s.byName == nil {
		s.index()
	}
	name = strings.ToLower(unquote(name))
	if t, ok := s.byName[name]; ok {
		return t
	}
	return nil
}

func unquote(name string) string {
	parts := strings.Split(name, ".")
	for i, p := range parts {
		parts[i] = strings.Trim(p, "\"`[]")
	}
	return strings.Join(parts, ".")
}

// FromTables builds a schema holding only table names (no columns yet).
func FromTables(t conn.DBType, names []string) *Schema {
	s := &Schema{Version: cacheVersion, DBType: string(t), Generated: time.Now()}
	for _, n := range names {
		s.Tables = append(s.Tables, Table{Name: n})
	}
	s.index()
	return s
}

// Introspect lists tables (like the sidebar) and their columns. Listing
// tables is quick and reading every column can be slow, so onTables (if not
// nil) is called with the table names as soon as they are known, before the
// column query runs.
func Introspect(ctx context.Context, db *sql.DB, t conn.DBType, onTables func(names []string)) (*Schema, error) {
	names, err := query.ListTables(ctx, db, t)
	if err != nil {
		return nil, err
	}
	if onTables != nil {
		onTables(names)
	}
	s := FromTables(t, names)
	pos := make(map[string]int, len(names))
	for i, n := range names {
		pos[n] = i
	}

	var q string
	switch t {
	case conn.Postgres:
		q = `SELECT table_schema || '.' || table_name, column_name, data_type FROM information_schema.columns
		     WHERE table_schema NOT IN ('pg_catalog','information_schema')
		     ORDER BY table_schema, table_name, ordinal_position`
	case conn.MariaDB:
		q = `SELECT table_name, column_name, column_type FROM information_schema.columns
		     WHERE table_schema = DATABASE() ORDER BY table_name, ordinal_position`
	case conn.Oracle:
		q = `SELECT table_name, column_name, data_type FROM user_tab_columns ORDER BY table_name, column_id`
	default:
		return s, nil
	}
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		// Tables alone are still useful.
		s.index()
		return s, fmt.Errorf("columns: %w", err)
	}
	defer rows.Close()
	var extra []Table // views etc. not in the table list
	extraPos := map[string]int{}
	for rows.Next() {
		var tbl, col string
		var typ sql.NullString
		if err := rows.Scan(&tbl, &col, &typ); err != nil {
			s.index()
			return s, err
		}
		c := Column{Name: col, Type: typ.String}
		if i, ok := pos[tbl]; ok {
			s.Tables[i].Columns = append(s.Tables[i].Columns, c)
			continue
		}
		i, ok := extraPos[tbl]
		if !ok {
			i = len(extra)
			extraPos[tbl] = i
			extra = append(extra, Table{Name: tbl})
		}
		extra[i].Columns = append(extra[i].Columns, c)
	}
	sort.Slice(extra, func(i, j int) bool { return extra[i].Name < extra[j].Name })
	s.Tables = append(s.Tables, extra...)
	s.index()
	return s, rows.Err()
}

// CachePath returns the cache file for a connection inside dir. The file name
// is derived from type, user, host, port and database (never the password).
func CachePath(dir string, c *conn.Config) string {
	key := fmt.Sprintf("%s_%s@%s_%d_%s", c.Type, c.User, c.Host, c.Port, c.Database)
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_', r == '@':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return filepath.Join(dir, b.String()+".json")
}

// Load reads a cache file written by Save.
func Load(path string) (*Schema, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Schema
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.Version != cacheVersion {
		return nil, fmt.Errorf("cache %s: version %d, want %d", path, s.Version, cacheVersion)
	}
	s.index()
	return &s, nil
}

// Save writes the schema to path atomically, creating the directory.
func (s *Schema) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
