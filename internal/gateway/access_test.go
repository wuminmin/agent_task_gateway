package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/deepseek"
	"taskbound.local/agent-data-gateway/internal/mcp"
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
			RequestedBudget: json.RawMessage(`{}`), RequestContext: json.RawMessage(`{}`),
			CreatedAt: harness.clock.value, UpdatedAt: harness.clock.value,
		},
		{
			ID: "task-other", PrincipalID: other.ID, Objective: "other task",
			State: control.TaskAwaitingSubmission, CatalogVersion: harness.catalog.CatalogVersion,
			RequestedBudget: json.RawMessage(`{}`), RequestContext: json.RawMessage(`{}`),
			CreatedAt: harness.clock.value, UpdatedAt: harness.clock.value,
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

func TestModelOutageDoesNotDisableDirectSQLAndCarolCannotReadRawResult(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createActiveSummaryTask(t, "task-direct-sql")
	harness.translator.queryErr = apierr.New(apierr.CodeModelUnavailable, "model unavailable in test")

	_, err := callGatewayTool(harness.service, harness.alice, "query_data", map[string]any{
		"task_id": "task-direct-sql", "question": "show monthly totals",
	})
	requireToolCode(t, err, apierr.CodeModelUnavailable)
	if harness.translator.queryCalls != 1 || len(harness.connector.requests) != 0 {
		t.Fatalf("model failure reached connector: translator=%d connector=%d", harness.translator.queryCalls, len(harness.connector.requests))
	}

	direct := mustCallGatewayTool(t, harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": "task-direct-sql",
		"sql":     "SELECT month, total_amount FROM expense_summary",
	})
	if harness.translator.queryCalls != 1 {
		t.Fatalf("direct SQL unexpectedly called translator %d times", harness.translator.queryCalls)
	}
	if len(harness.connector.requests) != 1 {
		t.Fatalf("direct SQL connector calls = %d, want 1", len(harness.connector.requests))
	}
	if harness.connector.requests[0].MaxRows <= 0 || harness.connector.requests[0].StatementTimeout <= 0 {
		t.Fatalf("direct SQL omitted enforced connector bounds: %+v", harness.connector.requests[0])
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
		if tool.Name == "get_query_result" || tool.Name == "query_sql" || tool.Name == "query_data" {
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
	for _, rawResultField := range []string{"rows", "columns"} {
		if _, exists := receipt[rawResultField]; exists {
			t.Fatalf("Carol receipt exposed raw result field %q: %#v", rawResultField, receipt)
		}
	}
}

var _ deepseek.Translator = (*fakeTranslator)(nil)
