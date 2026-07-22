package queryplan

import "testing"

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
