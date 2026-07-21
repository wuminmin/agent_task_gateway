package queryplan

import (
	"testing"

	"taskbound.local/agent-data-gateway/internal/deepseek"
)

func TestCompileEscapesLiteralsAndRestrictsColumns(t *testing.T) {
	t.Parallel()
	product := Product{
		Name:              "expense_summary",
		Columns:           map[string]struct{}{"month": {}, "department": {}, "total_amount": {}},
		AllowedAggregates: map[string]struct{}{"sum": {}},
	}
	plan := deepseek.QueryPlan{
		Product:    "expense_summary",
		Columns:    []string{"month"},
		Aggregates: []deepseek.Aggregate{{Function: "sum", Column: "total_amount", Alias: "amount"}},
		Filters:    []deepseek.Filter{{Column: "department", Op: "=", Value: "销售'部"}},
		GroupBy:    []string{"month"},
		OrderBy:    []deepseek.Order{{Column: "month", Direction: "asc"}},
		Limit:      10,
	}
	got, err := Compile(plan, product)
	if err != nil {
		t.Fatal(err)
	}
	want := `SELECT "month", sum("total_amount") AS "amount" FROM "expense_summary" WHERE "department" = '销售''部' GROUP BY "month" ORDER BY "month" ASC LIMIT 10`
	if got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	plan.Columns = append(plan.Columns, "salary")
	if _, err := Compile(plan, product); err == nil {
		t.Fatal("expected unapproved column rejection")
	}
}
