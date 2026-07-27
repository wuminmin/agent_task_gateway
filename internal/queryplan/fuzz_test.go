package queryplan

import (
	"encoding/json"
	"testing"
)

func FuzzCompileNeverPanicsOrEmitsUnapprovedIdentifiers(f *testing.F) {
	f.Add("month", "=", "2026-01", "asc", int(10))
	f.Add("salary", "LIKE", "%' OR true --", "sideways", int(-1))
	product := Product{
		Name: "expense_summary",
		Columns: map[string]struct{}{
			"month": {}, "department": {}, "total_amount": {},
		},
		AllowedAggregates: map[string]struct{}{"sum": {}, "count": {}},
	}
	f.Fuzz(func(t *testing.T, column, operator, value, direction string, limit int) {
		plan := QueryPlan{
			Product: product.Name, Columns: []string{"month"},
			Filters: []Filter{{Column: column, Op: operator, Value: value}},
			OrderBy: []Order{{Column: "month", Direction: direction}}, Limit: limit,
		}
		compiled, err := Compile(plan, product)
		if err == nil && compiled == "" {
			t.Fatal("successful compilation emitted empty SQL")
		}
	})
}

func FuzzCompileRelationalNeverPanics(f *testing.F) {
	f.Add([]byte(`{"from":{"join":{"left":{"product":"left","role":"left"},"right":{"product":"right","role":"right"},"on":[{"left":"left.k","right":"right.k"}]}},"columns":["left.value"]}`))
	f.Add([]byte(`{"from":{"union_distinct":{"role":"left","columns":["k","value"],"left":{"product":"left","role":"a"},"right":{"product":"left","role":"b"}}},"columns":["left.value"]}`))
	products := map[string]Product{
		"left":  {Name: "left", StableRole: "left", SourceNamespace: "source.left", Snapshot: "s1", StableEntityKey: []string{"k"}, Columns: map[string]struct{}{"k": {}, "value": {}}, ColumnTypes: map[string]string{"k": "integer", "value": "text"}, ColumnCollations: map[string]string{"value": "C"}, CollationVersions: map[string]string{"value": "builtin"}, AllowedAggregates: map[string]struct{}{"count": {}}},
		"right": {Name: "right", StableRole: "right", SourceNamespace: "source.right", Snapshot: "s1", StableEntityKey: []string{"k"}, Columns: map[string]struct{}{"k": {}, "value": {}}, ColumnTypes: map[string]string{"k": "integer", "value": "text"}, ColumnCollations: map[string]string{"value": "C"}, CollationVersions: map[string]string{"value": "builtin"}, AllowedAggregates: map[string]struct{}{"count": {}}},
	}
	f.Fuzz(func(t *testing.T, encoded []byte) {
		var plan QueryPlan
		if err := json.Unmarshal(encoded, &plan); err != nil {
			return
		}
		_, _ = CompileRelational(plan, products)
	})
}
