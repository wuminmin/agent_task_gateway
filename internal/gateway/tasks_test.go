package gateway

import (
	"context"
	"testing"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/control"
)

func TestRequestDataTaskUsesCompleteCatalogBudget(t *testing.T) {
	harness := newGatewayHarness(t)
	request := map[string]any{
		"objective":     "compare aggregate and employee expenses",
		"data_products": []string{"expense_summary", "expense_detail"},
		"columns": map[string][]string{
			"expense_summary": {"month", "total_amount"},
			"expense_detail":  {"receipt_no", "employee_name", "amount"},
		},
		"scopes": map[string]any{
			"department": []any{"销售部"},
		},
	}

	result := mustCallGatewayTool(t, harness.service, harness.alice, "request_data_task", request)
	if got := result["approval_mode"]; got != "manual" {
		t.Fatalf("approval_mode = %v, want manual", got)
	}
	if got := result["sensitivity"]; got != "high" {
		t.Fatalf("sensitivity = %v, want high", got)
	}
	budget, ok := result["budget"].(map[string]any)
	if !ok || budget["max_queries"] != int64(5) || budget["max_rows"] != int64(100) {
		t.Fatalf("unexpected approved budget: %#v", result["budget"])
	}
	if result["budget_source"] != "catalog_profile" || result["budget_profile"] != "detail-manual-v3" {
		t.Fatalf("budget was not issued from the complete catalog profile: %#v", result)
	}
	if budget["exposure_profile_version"] != "taskgate-exposure-v3" || budget["max_outcome_facts"] != int64(5) {
		t.Fatalf("default approval route did not select the V3 outcome profile: %#v", result["budget"])
	}
	if len(harness.approval.requests) != 1 {
		t.Fatalf("OA draft calls = %d, want 1", len(harness.approval.requests))
	}
	draft := harness.approval.requests[0]
	if draft.ApprovalMode != "manual" || draft.Approver != "bob" || draft.Manifest.Sensitivity != "high" {
		t.Fatalf("OA route was not catalog-derived: %+v", draft)
	}
	if draft.Manifest.HumanSubject != harness.alice.Subject || draft.Manifest.AgentID != harness.alice.ID {
		t.Fatalf("manifest identity was not gateway-derived: %+v", draft.Manifest)
	}
	if len(draft.Manifest.Products) != 2 || draft.Manifest.Products[0] != "expense_detail" || draft.Manifest.Products[1] != "expense_summary" {
		t.Fatalf("unexpected OA products: %#v", draft.Manifest.Products)
	}
	if got := draft.Manifest.ApprovedColumns["expense_detail"]; len(got) != 3 || got[0] != "amount" || got[2] != "receipt_no" {
		t.Fatalf("OA draft did not receive normalized approved columns: %#v", draft.Manifest.ApprovedColumns)
	}
	if departments, ok := draft.Manifest.MandatoryScope["department"].([]string); !ok || len(departments) != 1 || departments[0] != "销售部" {
		t.Fatalf("OA draft did not receive normalized mandatory scope: %#v", draft.Manifest.MandatoryScope)
	}
	if draft.Manifest.Budget.MaxQueries != 5 || draft.Manifest.Budget.MaxResultRows != 100 || draft.Manifest.Budget.MaxDBMS != 15_000 || draft.Manifest.Budget.PerQueryTimeoutMS != 5_000 || draft.Manifest.Budget.TaskTTLMS != 900_000 {
		t.Fatalf("OA draft did not receive the exact budget: %+v", draft.Manifest.Budget)
	}
	if draft.Manifest.CatalogSHA256 != harness.catalog.SHA256 || len(draft.Manifest.Nonce) != 32 {
		t.Fatalf("manifest omitted catalog digest or nonce: %+v", draft.Manifest)
	}
	if draft.Manifest.DatasourceID != harness.connector.attestation.DatasourceID ||
		draft.Manifest.SchemaDigest != harness.connector.attestation.SchemaDigest {
		t.Fatalf("manifest omitted datasource evidence: %+v", draft.Manifest)
	}
	expectedSnapshot, err := approval.AuthorizationSnapshotSHA256(draft)
	if err != nil {
		t.Fatalf("recompute OA snapshot: %v", err)
	}
	if draft.ManifestDigest == "" || draft.ManifestDigest != expectedSnapshot {
		t.Fatalf("OA manifest hash = %q, want %q", draft.ManifestDigest, expectedSnapshot)
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
	if pending.Budget.MaxQueries != 5 || pending.Budget.MaxRows != 100 || pending.ApprovalMode != "manual" || pending.Approver != "bob" {
		t.Fatalf("unexpected pending policy: %+v", pending)
	}
	if pending.DatasourceID != draft.Manifest.DatasourceID || pending.SchemaDigest != draft.Manifest.SchemaDigest {
		t.Fatalf("pending datasource evidence does not match manifest: %+v", pending)
	}
	if departments, ok := pending.MandatoryScope["department"].([]any); !ok || len(departments) != 1 || departments[0] != "销售部" {
		t.Fatalf("mandatory department scope was not persisted: %#v", pending.MandatoryScope)
	}
	if persistedPending.ManifestDigest != draft.ManifestDigest || persistedPending.Manifest.AgentID != harness.alice.ID {
		t.Fatalf("persisted manifest does not match OA: %+v vs %+v", persistedPending, draft)
	}

	_, err = callGatewayTool(harness.service, harness.alice, "request_data_task", map[string]any{
		"objective":        "legacy client tries to select its own budget",
		"data_products":    []string{"expense_detail"},
		"columns":          map[string][]string{"expense_detail": {"receipt_no", "amount"}},
		"scopes":           map[string]any{"department": []any{"销售部"}},
		"requested_budget": map[string]any{"max_queries": 3, "max_rows": 50},
	})
	requireToolCode(t, err, apierr.CodeInvalidRequest)
	if len(harness.approval.requests) != 1 {
		t.Fatalf("client-selected budget reached OA; calls = %d", len(harness.approval.requests))
	}
	tasks, err := harness.store.ListTasks(context.Background(), control.TaskFilter{PrincipalID: harness.alice.ID, Limit: 10})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("client-selected budget was persisted; tasks = %d", len(tasks))
	}
}

func TestRequestDataTaskRequiresExplicitColumnsAndScopes(t *testing.T) {
	harness := newGatewayHarness(t)
	_, err := callGatewayTool(harness.service, harness.alice, "request_data_task", map[string]any{
		"objective": "missing structured authorization", "data_products": []string{"expense_summary"},
	})
	requireToolCode(t, err, apierr.CodeInvalidRequest)
	_, err = callGatewayTool(harness.service, harness.alice, "request_data_task", map[string]any{
		"objective": "empty product columns", "data_products": []string{"expense_summary"},
		"columns": map[string][]string{"expense_summary": {}}, "scopes": map[string]any{"department": []any{"销售部"}},
	})
	requireToolCode(t, err, apierr.CodeInvalidRequest)
	if len(harness.approval.requests) != 0 {
		t.Fatalf("invalid structured requests reached OA: %d", len(harness.approval.requests))
	}
}
