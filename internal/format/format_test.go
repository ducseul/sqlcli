package format

import (
	"strings"
	"testing"

	"sqlcli/internal/query"
)

func TestAutoFallsBackToVertical(t *testing.T) {
	r := &query.Result{IsQuery: true, Columns: []string{"ID", "NAME"}, Rows: [][]string{{"1", "Lê Thị Lan"}}}
	var b strings.Builder
	if err := WriteWidth(&b, r, "auto", false, 80); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(b.String(), "+----") {
		t.Fatalf("expected table, got:\n%s", b.String())
	}
	b.Reset()
	if err := WriteWidth(&b, r, "auto", false, 10); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "1. row") {
		t.Fatalf("expected vertical, got:\n%s", b.String())
	}
}
