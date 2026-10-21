package query

import "testing"

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
