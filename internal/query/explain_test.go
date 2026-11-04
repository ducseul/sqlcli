package query

import (
	"context"
	"testing"
)

func TestPrettyJSON(t *testing.T) {
	if got := prettyJSON(`[{"Plan":{"Node Type":"Seq Scan"}}]`); got != "[\n  {\n    \"Plan\": {\n      \"Node Type\": \"Seq Scan\"\n    }\n  }\n]" {
		t.Errorf("json not indented: %q", got)
	}
	if got := prettyJSON("Seq Scan on t  (cost=0.00..1.01)"); got != "Seq Scan on t  (cost=0.00..1.01)" {
		t.Errorf("text changed: %q", got)
	}
}

func TestExplainEmpty(t *testing.T) {
	if _, err := Explain(context.Background(), nil, "postgres", "  ; ", false); err == nil {
		t.Error("expected error for empty statement")
	}
}
