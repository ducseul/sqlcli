package query

import (
	"strings"
	"testing"
)

func TestSplit(t *testing.T) {
	got := Split(`select 1; select 'a;b'; -- c;d
select "x;y" /* ; */ from t;
begin
  null;
end;
/
update t set a=1`)
	want := []string{
		"select 1",
		"select 'a;b'",
		"-- c;d\nselect \"x;y\" /* ; */ from t",
		"begin\n  null;\nend;",
		"update t set a=1",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d statements: %q", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("stmt %d: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestIsQuery(t *testing.T) {
	yes := []string{"select 1", "  WITH x AS (select 1) select * from x", "-- hi\nselect 1", "show tables", "insert into t values(1) returning id"}
	no := []string{"insert into t values(1)", "update t set a=1", "create table t(a int)", "delete from t"}
	for _, s := range yes {
		if !IsQuery(s) {
			t.Errorf("expected query: %q", s)
		}
	}
	for _, s := range no {
		if IsQuery(s) {
			t.Errorf("expected non-query: %q", s)
		}
	}
}

func TestStatementAt(t *testing.T) {
	script := "select 1;\nselect 2;  \n\nselect 'a;b'\n  from dual;\nselect 4"
	at := func(sub string) int { return strings.Index(script, sub) }
	cases := []struct {
		name string
		pos  int
		want string
	}{
		{"start", 0, "select 1"},
		{"inside first", 3, "select 1"},
		{"on terminator", at(";"), "select 1"},
		{"after terminator same line", at("select 2;") + len("select 2;  "), "select 2"},
		{"blank line picks next", at("\n\nselect 'a") + 1, "select 'a;b'\n  from dual"},
		{"inside quoted semicolon", at("a;b") + 1, "select 'a;b'\n  from dual"},
		{"second line of stmt", at("from"), "select 'a;b'\n  from dual"},
		{"last without terminator", len(script), "select 4"},
		{"out of range", len(script) + 10, "select 4"},
	}
	for _, c := range cases {
		if got := StatementAt(script, c.pos); got != c.want {
			t.Errorf("%s (pos %d): got %q, want %q", c.name, c.pos, got, c.want)
		}
	}
	if got := StatementAt("  \n ", 1); got != "" {
		t.Errorf("empty script: got %q", got)
	}
	if got := StatementAt("select 1;\n", 10); got != "select 1" {
		t.Errorf("trailing newline: got %q", got)
	}
}
