package gateway

import (
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/queryplan"
)

func TestDeriveObservationV2RequiresEqualUngroupedRowCounts(t *testing.T) {
	field := func(name string) catalog.Field {
		return catalog.Field{Name: name, Type: "text", Collation: "C", CollationVersion: "builtin"}
	}
	context := planExposureContext{
		product: catalog.Product{
			Name: "summary", Snapshot: "snapshot-v1", FactNamespace: "travel.summary",
			StableRelationRole: "summary", EntityKey: []string{"month", "department", "expense_type"},
			Fields: []catalog.Field{field("month"), field("department"), field("expense_type"), field("total_amount")},
		},
		plan:             queryplan.QueryPlan{Product: "summary", Columns: []string{"month", "total_amount"}},
		visibleFields:    []string{"month", "total_amount"},
		provenanceFields: []string{"department", "expense_type", "month", "total_amount"},
		planDigest:       strings.Repeat("a", 64),
	}
	visibleColumns := []dataconnector.Column{{Name: "month"}, {Name: "total_amount"}, {Name: "department"}, {Name: "expense_type"}}
	provenanceColumns := []dataconnector.Column{{Name: "department"}, {Name: "expense_type"}, {Name: "month"}, {Name: "total_amount"}}
	visibleRows := [][]any{
		{"2026-01", "1680.00", "销售部", "机票"},
		{"2026-01", "880.00", "销售部", "酒店"},
	}
	provenanceRows := [][]any{
		{"销售部", "机票", "2026-01", "1680.00"},
		{"销售部", "酒店", "2026-01", "880.00"},
	}

	tests := []struct {
		name           string
		visibleRows    [][]any
		provenanceRows [][]any
		wantError      bool
	}{
		{name: "equal", visibleRows: visibleRows, provenanceRows: provenanceRows},
		{name: "visible has an extra row", visibleRows: visibleRows, provenanceRows: provenanceRows[:1], wantError: true},
		{name: "provenance has an extra row", visibleRows: visibleRows[:1], provenanceRows: provenanceRows, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			visible := dataconnector.Result{Columns: visibleColumns, Rows: test.visibleRows, RowCount: int64(len(test.visibleRows))}
			provenance := dataconnector.Result{Columns: provenanceColumns, Rows: test.provenanceRows, RowCount: int64(len(test.provenanceRows))}
			_, err := context.deriveObservationV2(visible, provenance)
			if test.wantError {
				if err == nil || err.Error() != "visible and provenance row sets differ" {
					t.Fatalf("deriveObservationV2 error = %v, want visible/provenance row-set mismatch", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("equal row sets were rejected: %v", err)
			}
		})
	}
}
