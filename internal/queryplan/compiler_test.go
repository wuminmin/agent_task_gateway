package queryplan

import (
	"testing"
)

func TestCompileEscapesLiteralsAndRestrictsColumns(t *testing.T) {
	t.Parallel()
	product := Product{
		Name:              "expense_summary",
		Columns:           map[string]struct{}{"month": {}, "department": {}, "total_amount": {}},
		AllowedAggregates: map[string]struct{}{"sum": {}},
	}
	plan := QueryPlan{
		Product:    "expense_summary",
		Columns:    []string{"month"},
		Aggregates: []Aggregate{{Function: "sum", Column: "total_amount", Alias: "amount"}},
		Filters:    []Filter{{Column: "department", Op: "=", Value: "销售'部"}},
		GroupBy:    []string{"month"},
		OrderBy:    []Order{{Column: "month", Direction: "asc"}},
		Limit:      10,
	}
	got, err := Compile(plan, product)
	if err != nil {
		t.Fatal(err)
	}
	want := `SELECT "month", sum("total_amount") AS "amount" FROM "expense_summary" WHERE "department" = E'销售''部' GROUP BY "month" ORDER BY "month" ASC LIMIT 10`
	if got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	plan.Columns = append(plan.Columns, "salary")
	if _, err := Compile(plan, product); err == nil {
		t.Fatal("expected unapproved column rejection")
	}
}

func TestSQLLiteralPinsEscapeStringSemantics(t *testing.T) {
	t.Parallel()
	got, err := sqlLiteral(`\' OR true --`)
	if err != nil {
		t.Fatal(err)
	}
	if want := `E'\\'' OR true --'`; got != want {
		t.Fatalf("escaped literal = %q, want %q", got, want)
	}
}

func TestCompileRejectsNegativeLimitAndDuplicateSelectNames(t *testing.T) {
	t.Parallel()
	product := Product{Name: "expense_summary", Columns: map[string]struct{}{"month": {}}, AllowedAggregates: map[string]struct{}{"count": {}}}
	if _, err := Compile(QueryPlan{Product: product.Name, Columns: []string{"month"}, Limit: -1}, product); err == nil {
		t.Fatal("negative limit was accepted")
	}
	if _, err := Compile(QueryPlan{Product: product.Name, Columns: []string{"month", "month"}}, product); err == nil {
		t.Fatal("duplicate select column was accepted")
	}
}

func TestCompileDistinguishesCountStarFromCountColumn(t *testing.T) {
	t.Parallel()
	product := Product{Name: "expense_summary", Columns: map[string]struct{}{"month": {}}, AllowedAggregates: map[string]struct{}{"count": {}}}
	star, err := Compile(QueryPlan{Product: product.Name, Aggregates: []Aggregate{{Function: "count", Column: "*", Alias: "rows"}}}, product)
	if err != nil {
		t.Fatal(err)
	}
	column, err := Compile(QueryPlan{Product: product.Name, Aggregates: []Aggregate{{Function: "count", Column: "month", Alias: "months"}}}, product)
	if err != nil {
		t.Fatal(err)
	}
	if star != `SELECT count(*) AS "rows" FROM "expense_summary"` || column != `SELECT count("month") AS "months" FROM "expense_summary"` {
		t.Fatalf("count SQL is not canonical: %q / %q", star, column)
	}
	if _, err := Compile(QueryPlan{Product: product.Name, Aggregates: []Aggregate{{Function: "sum", Column: "*", Alias: "bad"}}},
		Product{Name: product.Name, Columns: product.Columns, AllowedAggregates: map[string]struct{}{"sum": {}}}); err == nil {
		t.Fatal("SUM(*) was accepted")
	}
}

func TestCompileRejectsInvalidGroupingOrderingAndFilters(t *testing.T) {
	t.Parallel()
	product := Product{
		Name: "expense_summary", Columns: map[string]struct{}{"month": {}, "department": {}, "total_amount": {}},
		AllowedAggregates: map[string]struct{}{"sum": {}},
	}
	tests := []QueryPlan{
		{Product: product.Name, Columns: []string{"month"}, Aggregates: []Aggregate{{Function: "sum", Column: "total_amount", Alias: "amount"}}},
		{Product: product.Name, Columns: []string{"month"}, GroupBy: []string{"month", "month"}},
		{Product: product.Name, Columns: []string{"month", "department"}, GroupBy: []string{"month"}},
		{Product: product.Name, Columns: []string{"month"}, OrderBy: []Order{{Column: "department"}}},
		{Product: product.Name, Columns: []string{"month"}, OrderBy: []Order{{Column: "month"}, {Column: "month"}}},
		{Product: product.Name, Columns: []string{"month"}, Filters: []Filter{{Column: "month", Op: "LIKE", Value: true}}},
	}
	for index, plan := range tests {
		if _, err := Compile(plan, product); err == nil {
			t.Errorf("invalid plan %d was accepted: %#v", index, plan)
		}
	}
}
