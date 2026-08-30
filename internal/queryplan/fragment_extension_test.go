package queryplan

import (
	"strings"
	"testing"
)

func extensionProduct() Product {
	return Product{
		Name: "orders", StableRole: "orders", SourceNamespace: "sales.orders", Snapshot: "snapshot-1",
		StableEntityKey: []string{"order_id"},
		Columns:         map[string]struct{}{"order_id": {}, "status": {}, "amount": {}},
		ColumnTypes:     map[string]string{"order_id": "integer", "status": "text", "amount": "numeric"},
		ColumnCollations: map[string]string{"status": "C"}, CollationVersions: map[string]string{"status": "builtin"},
		AllowedAggregates: map[string]struct{}{"count": {}, "sum": {}, "avg": {}},
	}
}

func TestCompileEmitsHavingAndDistinct(t *testing.T) {
	plan := QueryPlan{Product: "orders", Columns: []string{"status"},
		Aggregates: []Aggregate{{Function: "count", Column: "order_id", Alias: "orders", Distinct: true}, {Function: "avg", Column: "amount", Alias: "mean"}},
		GroupBy:    []string{"status"}, Having: []Filter{{Column: "mean", Op: ">", Value: 10}}}
	sql, err := Compile(plan, extensionProduct())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, `count(DISTINCT "order_id")`) || !strings.Contains(sql, ` HAVING avg("amount") > 10`) {
		t.Fatalf("sql = %s", sql)
	}
	if AggregateOutputType("avg", "integer") != "numeric" || AggregateOutputType("avg", "text") != "" {
		t.Fatalf("avg output types = %q / %q", AggregateOutputType("avg", "integer"), AggregateOutputType("avg", "text"))
	}
}

func TestNormalFormCarriesHavingAndDistinct(t *testing.T) {
	product := extensionProduct()
	plan := QueryPlan{Product: "orders", Columns: []string{"status"},
		Aggregates: []Aggregate{{Function: "count", Column: "order_id", Alias: "orders", Distinct: true}},
		GroupBy:    []string{"status"}, Having: []Filter{{Column: "orders", Op: ">", Value: 1}}}
	withHaving, err := NormalizeV2(plan, product)
	if err != nil {
		t.Fatal(err)
	}
	plan.Having = nil
	withoutHaving, err := NormalizeV2(plan, product)
	if err != nil {
		t.Fatal(err)
	}
	if len(withHaving.Having) != 1 || withHaving.Having[0].SQLType != "bigint" || !strings.HasPrefix(withHaving.Having[0].Column, "count(distinct ") {
		t.Fatalf("having = %#v", withHaving.Having)
	}
	if len(withoutHaving.Having) != 0 {
		t.Fatalf("having leaked: %#v", withoutHaving.Having)
	}
	if withHaving.Aggregates[0].Expression == withoutHaving.Aggregates[0].Expression && withHaving.Aggregates[0].Expression != "count(distinct sales.orders.order_id)" {
		t.Fatalf("distinct expression = %q", withHaving.Aggregates[0].Expression)
	}
}
