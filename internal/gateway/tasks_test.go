package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/deepseek"
)

func TestRequestDataTaskUsesCatalogPolicyAndBudgetCeiling(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.translator.intent = deepseek.TaskIntent{
		Objective:    "compare aggregate and employee expenses",
		DataProducts: []string{"expense_summary", "expense_detail"},
		Columns: map[string][]string{
			"expense_summary": {"month", "total_amount"},
			"expense_detail":  {"receipt_no", "employee_name", "amount"},
		},
		Scopes: map[string]any{
			"department": []any{"销售部"},
		},
		RequestedBudget: &deepseek.RequestedBudget{MaxQueries: 4, MaxRows: 80},
	}

	result := mustCallGatewayTool(t, harness.service, harness.alice, "request_data_task", map[string]any{
		"objective":        "compare aggregate and employee expenses",
		"requested_budget": map[string]any{"max_queries": 3, "max_rows": 50},
	})
	if got := result["approval_mode"]; got != "manual" {
		t.Fatalf("approval_mode = %v, want manual", got)
	}
	if got := result["sensitivity"]; got != "high" {
		t.Fatalf("sensitivity = %v, want high", got)
	}
	budget, ok := result["budget"].(map[string]any)
	if !ok || budget["max_queries"] != int64(3) || budget["max_rows"] != int64(50) {
		t.Fatalf("unexpected approved budget: %#v", result["budget"])
	}
	if len(harness.approval.requests) != 1 {
		t.Fatalf("OA draft calls = %d, want 1", len(harness.approval.requests))
	}
	draft := harness.approval.requests[0]
	if draft.ApprovalMode != "manual" || draft.Approver != "bob" || draft.Sensitivity != "high" {
		t.Fatalf("OA route was not catalog-derived: %+v", draft)
	}
	if len(draft.DataProducts) != 2 || draft.DataProducts[0] != "expense_summary" || draft.DataProducts[1] != "expense_detail" {
		t.Fatalf("unexpected OA products: %#v", draft.DataProducts)
	}
	if got := draft.ApprovedColumns["expense_detail"]; len(got) != 3 || got[0] != "receipt_no" || got[2] != "amount" {
		t.Fatalf("OA draft did not receive normalized approved columns: %#v", draft.ApprovedColumns)
	}
	if departments, ok := draft.MandatoryScope["department"].([]string); !ok || len(departments) != 1 || departments[0] != "销售部" {
		t.Fatalf("OA draft did not receive normalized mandatory scope: %#v", draft.MandatoryScope)
	}
	if draft.Budget.MaxQueries != 3 || draft.Budget.MaxRows != 50 || draft.Budget.MaxDBMS != 15_000 || draft.Budget.QueryTimeoutMS != 5_000 || draft.Budget.TaskTTLMS != 900_000 {
		t.Fatalf("OA draft did not receive the exact budget: %+v", draft.Budget)
	}
	expectedSnapshot, err := approval.AuthorizationSnapshotSHA256(draft)
	if err != nil {
		t.Fatalf("recompute OA snapshot: %v", err)
	}
	if draft.AuthorizationSnapshotSHA256 == "" || draft.AuthorizationSnapshotSHA256 != expectedSnapshot {
		t.Fatalf("OA snapshot hash = %q, want %q", draft.AuthorizationSnapshotSHA256, expectedSnapshot)
	}

	taskID, ok := result["task_id"].(string)
	if !ok || taskID == "" {
		t.Fatalf("missing task_id in %#v", result)
	}
	task, err := harness.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("load requested task: %v", err)
	}
	if task.State != control.TaskAwaitingSubmission || task.Sensitivity != "high" || task.CatalogVersion != harness.catalog.CatalogVersion {
		t.Fatalf("unexpected persisted task: %+v", task)
	}
	persistedPending, err := decodePersistedPending(task)
	if err != nil {
		t.Fatalf("decode pending context: %v", err)
	}
	pending := persistedPending.pendingContext
	if pending.Budget.MaxQueries != 3 || pending.Budget.MaxRows != 50 || pending.ApprovalMode != "manual" || pending.Approver != "bob" {
		t.Fatalf("unexpected pending policy: %+v", pending)
	}
	if departments, ok := pending.MandatoryScope["department"].([]any); !ok || len(departments) != 1 || departments[0] != "销售部" {
		t.Fatalf("mandatory department scope was not persisted: %#v", pending.MandatoryScope)
	}
	if persistedPending.AuthorizationSnapshotSHA256 != draft.AuthorizationSnapshotSHA256 {
		t.Fatalf("pending snapshot hash = %q, OA hash = %q", persistedPending.AuthorizationSnapshotSHA256, draft.AuthorizationSnapshotSHA256)
	}

	if harness.translator.intentCalls != 1 || len(harness.translator.intentCatalogs) != 1 {
		t.Fatalf("translator calls = %d, catalogs = %d", harness.translator.intentCalls, len(harness.translator.intentCatalogs))
	}
	logicalCatalog := harness.translator.intentCatalogs[0]
	for _, forbiddenText := range []string{"reporting.", "secretRef", "GATEWAY_DB_PASSWORD", `"address"`} {
		if strings.Contains(logicalCatalog, forbiddenText) {
			t.Fatalf("sanitized model catalog leaked %q: %s", forbiddenText, logicalCatalog)
		}
	}

	_, err = callGatewayTool(harness.service, harness.alice, "request_data_task", map[string]any{
		"objective":        "request more than the detail policy permits",
		"requested_budget": map[string]any{"max_queries": 6, "max_rows": 50},
	})
	requireToolCode(t, err, apierr.CodeInvalidRequest)
	if len(harness.approval.requests) != 1 {
		t.Fatalf("over-budget request reached OA; calls = %d", len(harness.approval.requests))
	}
	tasks, err := harness.store.ListTasks(context.Background(), control.TaskFilter{PrincipalID: harness.alice.ID, Limit: 10})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("over-budget request was persisted; tasks = %d", len(tasks))
	}

	var requested map[string]any
	if err := json.Unmarshal(task.RequestedBudget, &requested); err != nil {
		t.Fatalf("decode requested budget: %v", err)
	}
	if requested["max_queries"] != float64(3) || requested["max_rows"] != float64(50) {
		t.Fatalf("explicit requested budget was not persisted: %#v", requested)
	}
}
