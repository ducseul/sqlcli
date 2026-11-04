package schema

import (
	"path/filepath"
	"strings"
	"testing"

	"sqlcli/internal/conn"
)

func testSchema() *Schema {
	s := &Schema{Version: cacheVersion, Tables: []Table{
		{Name: "public.users", Columns: []Column{{"id", "int4"}, {"user_name", "text"}, {"email", "text"}}},
		{Name: "public.orders", Columns: []Column{{"id", "int4"}, {"user_id", "int4"}, {"total", "numeric"}}},
		{Name: "audit.user_log", Columns: []Column{{"log_id", "int8"}}},
	}}
	s.index()
	return s
}

func texts(items []Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Text
	}
	return out
}

// complete runs Complete with the cursor at the '|' marker.
func complete(t *testing.T, s *Schema, src string, force bool) (string, []string) {
	t.Helper()
	pos := strings.Index(src, "|")
	text := strings.Replace(src, "|", "", 1)
	start, items := Complete(text, pos, s, force)
	return text[start:pos], texts(items)
}

func has(list []string, want ...string) bool {
	for _, w := range want {
		found := false
		for _, l := range list {
			if l == w {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func TestCompleteTablesAfterFrom(t *testing.T) {
	s := testSchema()
	word, got := complete(t, s, "select * from us|", false)
	if word != "us" || len(got) == 0 || got[0] != "users" {
		t.Fatalf("word %q got %v", word, got)
	}
	if has(got, "USING") {
		t.Errorf("keywords offered where a table is expected: %v", got)
	}
	// Second table in a FROM list.
	_, got = complete(t, s, "select * from users u, ord|", false)
	if len(got) == 0 || got[0] != "orders" {
		t.Errorf("from list: %v", got)
	}
}

func TestCompleteColumnsByAlias(t *testing.T) {
	s := testSchema()
	word, got := complete(t, s, "select o.| from orders o join users u on u.id = o.user_id", false)
	if word != "" || !has(got, "id", "user_id", "total") || has(got, "email") {
		t.Fatalf("word %q got %v", word, got)
	}
	_, got = complete(t, s, "select u.us| from orders o join users as u on 1=1", false)
	if len(got) != 1 || got[0] != "user_name" {
		t.Errorf("alias with AS: %v", got)
	}
	// Table name as qualifier, and schema.table.
	_, got = complete(t, s, "select users.em| from users", false)
	if len(got) != 1 || got[0] != "email" {
		t.Errorf("table qualifier: %v", got)
	}
	_, got = complete(t, s, "select public.orders.to| from public.orders", false)
	if len(got) != 1 || got[0] != "total" {
		t.Errorf("schema.table qualifier: %v", got)
	}
	// Schema qualifier lists its tables.
	_, got = complete(t, s, "select * from audit.|", false)
	if len(got) != 1 || got[0] != "user_log" {
		t.Errorf("schema qualifier: %v", got)
	}
}

func TestCompleteColumnsAndKeywords(t *testing.T) {
	s := testSchema()
	_, got := complete(t, s, "select * from users where us|", false)
	if len(got) == 0 || got[0] != "user_name" {
		t.Fatalf("columns of referenced table first: %v", got)
	}
	_, got = complete(t, s, "sel|", false)
	if len(got) == 0 || got[0] != "select" {
		t.Errorf("lower-case keyword: %v", got)
	}
	_, got = complete(t, s, "SEL|", false)
	if len(got) == 0 || got[0] != "SELECT" {
		t.Errorf("upper-case keyword: %v", got)
	}
	_, got = complete(t, s, "select * from users order by id desc li|", false)
	if !has(got, "limit") {
		t.Errorf("keyword after DESC: %v", got)
	}
}

func TestCompleteQuiet(t *testing.T) {
	s := testSchema()
	for _, src := range []string{
		"select |",                         // nothing typed, not forced
		"select 'us|",                      // inside a string
		"select 1 -- us|",                  // inside a comment
		"select 12|",                       // number
		"select * from users where email|", // exact lone match
	} {
		if _, got := complete(t, s, src, false); len(got) != 0 {
			t.Errorf("%q: expected no suggestions, got %v", src, got)
		}
	}
	if _, got := complete(t, s, "select * from |", true); !has(got, "users", "orders") {
		t.Errorf("forced: %v", got)
	}
}

func TestCompleteOtherStatement(t *testing.T) {
	s := testSchema()
	// Aliases come from the statement the cursor is in only.
	_, got := complete(t, s, "select * from users o;\nselect o.| from orders o", false)
	if !has(got, "total") || has(got, "email") {
		t.Errorf("got %v", got)
	}
}

func TestCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := &conn.Config{Type: conn.Postgres, User: "app", Host: "10.0.0.1", Port: 5432, Database: "shop/x", Password: "secret"}
	p := CachePath(dir, c)
	if strings.Contains(p, "secret") || filepath.Base(p) != "postgres_app@10.0.0.1_5432_shop_x.json" {
		t.Fatalf("cache path %s", p)
	}
	s := testSchema()
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tables) != 3 || got.ColumnCount() != 7 || got.Lookup("USERS") == nil || got.Lookup("audit.user_log") == nil {
		t.Errorf("loaded %+v", got)
	}
}
