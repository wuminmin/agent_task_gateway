package gateway

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

func TestAliceCannotObserveAnotherQueryPrincipalTask(t *testing.T) {
	harness := newGatewayHarness(t)
	ctx := context.Background()
	other := mcp.Principal{ID: "principal-other", Subject: "other-alice", Role: "query"}
	if err := harness.store.CreatePrincipal(ctx, control.Principal{
		ID: other.ID, Subject: other.Subject, Role: other.Role, CreatedAt: harness.clock.value,
	}); err != nil {
		t.Fatalf("create other principal: %v", err)
	}
	for _, task := range []control.Task{
		{
			ID: "task-alice", PrincipalID: harness.alice.ID, Objective: "alice task",
			State: control.TaskAwaitingSubmission, CatalogVersion: harness.catalog.CatalogVersion,
			RequestContext: json.RawMessage(`{}`),
			CreatedAt:      harness.clock.value, UpdatedAt: harness.clock.value,
		},
		{
			ID: "task-other", PrincipalID: other.ID, Objective: "other task",
			State: control.TaskAwaitingSubmission, CatalogVersion: harness.catalog.CatalogVersion,
			RequestContext: json.RawMessage(`{}`),
			CreatedAt:      harness.clock.value, UpdatedAt: harness.clock.value,
		},
	} {
		if err := harness.store.CreateTask(ctx, task); err != nil {
			t.Fatalf("create task %s: %v", task.ID, err)
		}
	}

	_, err := callGatewayTool(harness.service, harness.alice, "get_task_status", map[string]any{"task_id": "task-other"})
	requireToolCode(t, err, apierr.CodeNotFound)
	_, err = callGatewayTool(harness.service, harness.alice, "complete_task", map[string]any{"task_id": "task-other"})
	requireToolCode(t, err, apierr.CodeNotFound)

	result := mustCallGatewayTool(t, harness.service, harness.alice, "list_my_tasks", map[string]any{})
	tasks, ok := result["tasks"].([]map[string]any)
	if !ok {
		t.Fatalf("tasks has type %T, want []map[string]any", result["tasks"])
	}
	if len(tasks) != 1 || tasks[0]["task_id"] != "task-alice" {
		t.Fatalf("Alice task listing leaked another principal: %#v", tasks)
	}
}

func TestStructuredPlanAndDirectSQLKeepRawResultsOwnerOnly(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createActiveSummaryTask(t, "task-direct-sql")

	planned := mustCallGatewayTool(t, harness.service, harness.alice, "execute_plan", map[string]any{
		"task_id": "task-direct-sql", "request_id": "plan-access-1", "plan": map[string]any{
			"product": "expense_summary", "columns": []string{"month", "total_amount"},
			"order_by": []map[string]any{{"column": "month", "direction": "asc"}},
		},
	})
	if planned["query_plan"] == nil || len(harness.connector.requests) != 1 {
		t.Fatalf("structured plan was not executed: result=%#v connector=%d", planned, len(harness.connector.requests))
	}

	direct := mustCallGatewayTool(t, harness.service, harness.alice, "query_sql", map[string]any{
		"task_id":    "task-direct-sql",
		"request_id": "direct-access-1",
		"sql":        "SELECT month, total_amount FROM expense_summary",
	})
	if len(harness.connector.requests) != 2 {
		t.Fatalf("query connector calls = %d, want 2", len(harness.connector.requests))
	}
	if harness.connector.requests[1].MaxRows <= 0 || harness.connector.requests[1].StatementTimeout <= 0 {
		t.Fatalf("direct SQL omitted enforced connector bounds: %+v", harness.connector.requests[1])
	}
	queryID, ok := direct["query_id"].(string)
	if !ok || queryID == "" {
		t.Fatalf("direct SQL result omitted query_id: %#v", direct)
	}

	ownerResult := mustCallGatewayTool(t, harness.service, harness.alice, "get_query_result", map[string]any{
		"task_id": "task-direct-sql", "query_id": queryID,
	})
	ownerJSON, err := json.Marshal(ownerResult)
	if err != nil {
		t.Fatalf("marshal owner result: %v", err)
	}
	if !strings.Contains(string(ownerJSON), "sensitive-row") {
		t.Fatalf("owner did not receive stored raw result: %s", ownerJSON)
	}

	_, err = callGatewayTool(harness.service, harness.carol, "get_query_result", map[string]any{
		"task_id": "task-direct-sql", "query_id": queryID,
	})
	requireToolCode(t, err, apierr.CodeForbidden)
	for _, tool := range harness.service.ListTools(harness.carol) {
		if tool.Name == "get_query_result" || tool.Name == "query_sql" || tool.Name == "execute_plan" {
			t.Fatalf("Carol was advertised raw-data tool %q", tool.Name)
		}
	}

	receipt := mustCallGatewayTool(t, harness.service, harness.carol, "get_audit_receipt", map[string]any{
		"receipt_id": queryID,
	})
	receiptJSON, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("marshal Carol receipt: %v", err)
	}
	if strings.Contains(string(receiptJSON), "sensitive-row") {
		t.Fatalf("Carol receipt leaked raw query data: %s", receiptJSON)
	}
	inclusion, ok := receipt["audit_inclusion"].(map[string]any)
	if !ok || inclusion["terminal_event"] == nil || inclusion["checkpoint"] == nil || inclusion["successor_events"] == nil {
		t.Fatalf("Carol receipt omitted audit inclusion evidence: %#v", receipt["audit_inclusion"])
	}
	for _, rawResultField := range []string{"rows", "columns"} {
		if _, exists := receipt[rawResultField]; exists {
			t.Fatalf("Carol receipt exposed raw result field %q: %#v", rawResultField, receipt)
		}
	}
	signedJSON, err := json.Marshal(receipt["receipt"])
	if err != nil {
		t.Fatalf("marshal signed receipt: %v", err)
	}
	var signed queryreceipt.QueryReceiptV1
	if err := json.Unmarshal(signedJSON, &signed); err != nil {
		t.Fatalf("decode signed receipt: %v", err)
	}
	verifier, err := queryreceipt.NewVerifier(map[string]ed25519.PublicKey{
		harness.service.queryReceiptSigner.KeyID(): harness.service.queryReceiptSigner.PublicKey(),
	})
	if err != nil {
		t.Fatalf("create query receipt verifier: %v", err)
	}
	if err := verifier.Verify(signed); err != nil {
		t.Fatalf("query receipt did not verify: %v", err)
	}
	tampered := signed
	tampered.RowCount++
	if verifier.Verify(tampered) == nil {
		t.Fatal("tampered query receipt signature verified")
	}
}

func TestDisabledPrincipalCannotUseStolenBearerToken(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createActiveSummaryTask(t, "task-disabled-principal")
	first := mustCallGatewayTool(t, harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": "task-disabled-principal", "request_id": "before-disable-1", "sql": testSummarySQL,
	})
	queryID, ok := first["query_id"].(string)
	if !ok || queryID == "" {
		t.Fatalf("query result omitted query_id: %#v", first)
	}
	if len(harness.service.ListTools(harness.alice)) == 0 {
		t.Fatal("Alice tools were hidden before principal disable")
	}

	disabled, err := harness.store.DisablePrincipal(context.Background(), harness.alice.ID, harness.clock.value.Add(time.Second))
	if err != nil {
		t.Fatalf("DisablePrincipal: %v", err)
	}
	if disabled.DisabledAt == nil {
		t.Fatalf("disabled principal has nil disabled_at: %+v", disabled)
	}
	if tools := harness.service.ListTools(harness.alice); len(tools) != 0 {
		t.Fatalf("disabled principal still sees tools: %#v", tools)
	}

	_, err = callGatewayTool(harness.service, harness.alice, "list_data_products", map[string]any{})
	requireToolCode(t, err, apierr.CodeForbidden)
	_, err = callGatewayTool(harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": "task-disabled-principal", "request_id": "after-disable-1", "sql": testSummarySQL,
	})
	requireToolCode(t, err, apierr.CodeForbidden)
	_, err = callGatewayTool(harness.service, harness.alice, "get_query_result", map[string]any{
		"task_id": "task-disabled-principal", "query_id": queryID,
	})
	requireToolCode(t, err, apierr.CodeForbidden)
	if len(harness.connector.requests) != 1 {
		t.Fatalf("disabled principal reached connector: %d calls", len(harness.connector.requests))
	}
}
