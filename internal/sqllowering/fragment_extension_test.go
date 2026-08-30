package sqllowering

import (
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/queryplan"
)

func extensionTestProducts() map[string]queryplan.Product {
	products := loweringTestProducts()
	orders := products["orders"]
	orders.AllowedAggregates = map[string]struct{}{"count": {}, "min": {}, "max": {}, "sum": {}, "avg": {}}
	products["orders"] = orders
	return products
}

func TestLowerHavingNamesTheSelectedAggregate(t *testing.T) {
	result, err := Lower(`SELECT o.region, count(*) AS n, avg(o.status_id) AS mean FROM orders o GROUP BY o.region HAVING count(*) > 2 AND 5 <= avg(o.status_id)`, extensionTestProducts())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Plan.Having) != 2 {
		t.Fatalf("having = %#v", result.Plan.Having)
	}
	byColumn := map[string]queryplan.Filter{}
	for _, filter := range result.Plan.Having {
		byColumn[filter.Column] = filter
	}
	if byColumn["n"].Op != ">" || byColumn["mean"].Op != ">=" {
		t.Fatalf("having operators = %#v", byColumn)
	}
	sql, err := queryplan.Compile(result.Plan, extensionTestProducts()["orders"])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, " HAVING ") || !strings.Contains(sql, `avg("status_id") >= 5`) {
		t.Fatalf("compiled SQL = %s", sql)
	}
}

func TestLowerHavingRequiresASelectedAggregate(t *testing.T) {
	_, err := Lower(`SELECT o.region, count(*) AS n FROM orders o GROUP BY o.region HAVING max(o.status_id) > 2`, extensionTestProducts())
	if err == nil || !strings.Contains(err.Error(), "HAVING_AGGREGATE_NOT_SELECTED") {
		t.Fatalf("err = %v", err)
	}
}

func TestLowerCountDistinctAndAvg(t *testing.T) {
	result, err := Lower(`SELECT count(DISTINCT o.region) AS regions, avg(o.status_id) AS mean FROM orders o`, extensionTestProducts())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Plan.Aggregates) != 2 || !result.Plan.Aggregates[0].Distinct || result.Plan.Aggregates[1].Function != "avg" {
		t.Fatalf("aggregates = %#v", result.Plan.Aggregates)
	}
	if _, err := Lower(`SELECT avg(o.region) AS mean FROM orders o`, extensionTestProducts()); err == nil || !strings.Contains(err.Error(), "AGGREGATE_TYPE_UNSUPPORTED") {
		t.Fatalf("avg over text err = %v", err)
	}
	if _, err := Lower(`SELECT sum(DISTINCT o.status_id) AS s FROM orders o`, extensionTestProducts()); err == nil || !strings.Contains(err.Error(), "AGGREGATE_MODIFIER_UNSUPPORTED") {
		t.Fatalf("sum distinct err = %v", err)
	}
}

func TestLowerStarExpandsToApprovedColumnsInSortedOrder(t *testing.T) {
	result, err := Lower(`SELECT * FROM orders o WHERE o.status = 'open'`, extensionTestProducts())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"order_id", "region", "status", "status_id"}
	if len(result.Plan.Columns) != len(want) {
		t.Fatalf("columns = %#v", result.Plan.Columns)
	}
	for index, column := range want {
		if !strings.HasSuffix(result.Plan.Columns[index], column) {
			t.Fatalf("column %d = %q, want suffix %q", index, result.Plan.Columns[index], column)
		}
	}
}
