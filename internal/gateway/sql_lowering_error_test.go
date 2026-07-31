package gateway

import (
	"reflect"
	"testing"

	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqllowering"
)

func TestSQLLoweringErrorMapsRepairFieldsWithoutLosingStableCode(t *testing.T) {
	mapped := sqlLoweringToolError(&sqllowering.Error{
		Code: sqllowering.CodeJoinTypeUnsupported, Reason: "LEFT_JOIN_UNSUPPORTED",
		Message:     "connected INNER equijoins only",
		Location:    sqllowering.Location{Clause: "FROM", Relation: "customer", Offset: 42},
		Alternative: "Use an INNER JOIN.", Retryable: true,
	})
	toolErr, ok := mapped.(*mcp.ToolError)
	if !ok {
		t.Fatalf("mapped error = %T, want *mcp.ToolError", mapped)
	}
	if toolErr.Code != sqllowering.CodeJoinTypeUnsupported || toolErr.Details["reason"] != "LEFT_JOIN_UNSUPPORTED" ||
		toolErr.Details["supported_alternative"] != "Use an INNER JOIN." || toolErr.Details["retryable_after_rewrite"] != true ||
		toolErr.Details["sql_profile"] != sqllowering.Profile {
		t.Fatalf("repairable error fields = %#v", toolErr)
	}
	location, ok := toolErr.Details["location"].(map[string]any)
	if !ok || location["clause"] != "FROM" || location["relation"] != "customer" || location["offset"] != int32(42) {
		t.Fatalf("error location = %#v", toolErr.Details["location"])
	}
}

func TestSQLResponseMetadataRestoresAliasesAndInterleavedProjectionOrder(t *testing.T) {
	stored := storedQueryResult{
		Columns: []dataconnector.Column{{Name: "orders.status"}, {Name: "revenue"}},
		Rows:    [][]any{{"OPEN", "12.50"}}, RowCount: 1,
	}
	metadata := &queryResponseMetadata{
		Plan: queryplan.QueryPlan{Columns: []string{"orders.status"}, Aggregates: []queryplan.Aggregate{{
			Function: "sum", Column: "lineitem.extended_price", Alias: "revenue",
		}}},
		SQLProfile: sqllowering.Profile, PlanDigest: "digest",
		DisplayColumns: []string{"sales", "order_state"}, ResultOrder: []int{1, 0},
	}
	if err := applyQueryResponseMetadata(&stored, metadata); err != nil {
		t.Fatal(err)
	}
	result := map[string]any{}
	if err := addStoredResponseMetadata(result, stored); err != nil {
		t.Fatal(err)
	}
	columns := result["columns"].([]dataconnector.Column)
	rows := result["rows"].([][]any)
	if !reflect.DeepEqual(columns, []dataconnector.Column{{Name: "sales"}, {Name: "order_state"}}) ||
		!reflect.DeepEqual(rows, [][]any{{"12.50", "OPEN"}}) {
		t.Fatalf("public result = columns %+v rows %#v", columns, rows)
	}
}

func TestSemanticReplayAlignsCanonicalColumnsToCurrentProjection(t *testing.T) {
	stored := storedQueryResult{
		Columns:         []dataconnector.Column{{Name: "a"}, {Name: "b"}},
		Rows:            [][]any{{"A", "B"}},
		SemanticColumns: []string{"column:a", "column:b"},
	}
	desired := []string{"column:b", "column:a"}
	if err := alignStoredSemanticColumns(&stored, desired); err != nil {
		t.Fatal(err)
	}
	metadata := &queryResponseMetadata{
		Plan: queryplan.QueryPlan{Columns: []string{"b", "a"}}, SQLProfile: sqllowering.Profile,
		DisplayColumns: []string{"b", "a"}, ResultOrder: []int{0, 1}, SemanticColumns: desired,
	}
	if err := applyQueryResponseMetadata(&stored, metadata); err != nil {
		t.Fatal(err)
	}
	result := map[string]any{}
	if err := addStoredResponseMetadata(result, stored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result["rows"], [][]any{{"B", "A"}}) {
		t.Fatalf("aligned semantic replay rows = %#v", result["rows"])
	}
}
