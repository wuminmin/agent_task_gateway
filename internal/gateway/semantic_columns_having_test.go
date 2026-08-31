package gateway

import (
	"encoding/json"
	"testing"

	"taskbound.local/agent-data-gateway/internal/queryplan"
)

// A HAVING plan's semantic columns must stay one per visible result column:
// the stored-result width check and the ResultOrder reordering both index
// into this list positionally. Appending HAVING entries made every HAVING
// query fail closed with "semantic-column metadata disagrees with canonical
// result" (pilot-benign-01, statements q05/q12/q15).
func TestSemanticColumnsStayAlignedUnderHaving(t *testing.T) {
	plan := queryplan.QueryPlan{
		Product: "expense_summary",
		Columns: []string{"department"},
		GroupBy: []string{"department"},
		Aggregates: []queryplan.Aggregate{
			{Function: "sum", Column: "total_amount", Alias: "total"},
		},
		Having: []queryplan.Filter{{Column: "total", Op: ">", Value: json.Number("100000")}},
	}
	columns := queryPlanSemanticColumns(plan)
	if len(columns) != len(plan.Columns)+len(plan.Aggregates) {
		t.Fatalf("semantic columns = %d, want one per visible column (%d)",
			len(columns), len(plan.Columns)+len(plan.Aggregates))
	}
	// The HAVING identity lives in the normalized plan, not here.
	withoutHaving := plan
	withoutHaving.Having = nil
	normalWith, err := queryplan.NormalizeV2(plan, testNormalFormProduct())
	if err != nil {
		t.Fatal(err)
	}
	normalWithout, err := queryplan.NormalizeV2(withoutHaving, testNormalFormProduct())
	if err != nil {
		t.Fatal(err)
	}
	if len(normalWith.Having) == len(normalWithout.Having) {
		t.Fatal("the normal form must distinguish HAVING variants")
	}
}

func testNormalFormProduct() queryplan.Product {
	return queryplan.Product{
		Name: "expense_summary", StableRole: "expense_summary",
		SourceNamespace: "test.expense_summary", Snapshot: "test-v1",
		StableEntityKey: []string{"month", "department", "expense_type"},
		Columns: map[string]struct{}{"month": {}, "department": {}, "expense_type": {},
			"total_amount": {}, "request_count": {}},
		ColumnTypes: map[string]string{"month": "text", "department": "text", "expense_type": "text",
			"total_amount": "numeric", "request_count": "bigint"},
		AllowedAggregates: map[string]struct{}{"sum": {}, "count": {}},
	}
}
