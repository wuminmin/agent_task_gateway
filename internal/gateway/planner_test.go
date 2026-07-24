package gateway

import (
	"encoding/json"
	"testing"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

func TestStrictJSONRejectsTrailingValue(t *testing.T) {
	var target []map[string]any
	if err := strictJSON(json.RawMessage(`[] {}`), &target); err == nil {
		t.Fatal("trailing JSON value was accepted")
	}
}

func TestPlanExposureUsesRootLedgerRemainingBudget(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createExposureSummaryTask(t, "task-planner", control.ExposureLimits{ReleaseFacts: 6, InfluenceFacts: 8})

	result := mustCallGatewayTool(t, harness.service, harness.alice, "plan_exposure", map[string]any{
		"task_id": "task-planner",
		"candidates": []map[string]any{
			{"id": "detail-raw", "requirement": "detail", "product": "expense_summary", "representation": "raw", "release_cost": 6, "influence_cost": 8, "answer_completeness": 1.0, "query_coverage": 1.0},
			{"id": "detail-projection", "requirement": "detail", "product": "expense_summary", "representation": "projection", "release_cost": 4, "influence_cost": 5, "answer_completeness": 0.8, "query_coverage": 0.8},
			{"id": "trend-aggregate", "requirement": "trend", "product": "expense_summary", "representation": "aggregate", "release_cost": 2, "influence_cost": 3, "answer_completeness": 0.9, "query_coverage": 0.9},
			{"id": "trend-generalized", "requirement": "trend", "product": "expense_summary", "representation": "generalized", "release_cost": 1, "influence_cost": 1, "answer_completeness": 0.5, "query_coverage": 0.6},
		},
	})
	plan := result["plan"].(exposure.Plan)
	if len(plan.Candidates) != 2 || plan.Candidates[0].ID != "detail-projection" || plan.Candidates[1].ID != "trend-aggregate" {
		t.Fatalf("selected candidates = %+v", plan.Candidates)
	}
	if plan.ReleaseCost != 6 || plan.InfluenceCost != 8 {
		t.Fatalf("selected cost = (%d,%d), want (6,8)", plan.ReleaseCost, plan.InfluenceCost)
	}
	remaining := result["budget_remaining"].(control.ExposureLimits)
	if remaining.ReleaseFacts != 6 || remaining.InfluenceFacts != 8 {
		t.Fatalf("remaining budget = %+v", remaining)
	}
}

func TestPlanExposureRejectsUnapprovedProducts(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createExposureSummaryTask(t, "task-planner-scope", control.ExposureLimits{ReleaseFacts: 6, InfluenceFacts: 8})
	_, err := callGatewayTool(harness.service, harness.alice, "plan_exposure", map[string]any{
		"task_id": "task-planner-scope",
		"candidates": []map[string]any{{
			"id": "employee-raw", "requirement": "people", "product": "employee_directory", "representation": "raw",
			"release_cost": 1, "influence_cost": 1, "answer_completeness": 1.0, "query_coverage": 1.0,
		}},
	})
	requireToolCode(t, err, apierr.CodePolicyDenied)
}

func TestPlanExposureV2ExecutesCandidatesAndSettlesTheirExactUnion(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createExposureV2SummaryTask(t, "task-planner-v2", control.ExposureLimits{ReleaseFacts: 2, InfluenceFacts: 3})
	harness.connector.result = dataconnector.Result{
		Columns: []dataconnector.Column{{Name: "month", DataTypeOID: 25}, {Name: "total_amount", DataTypeOID: 1700},
			{Name: "department", DataTypeOID: 25}, {Name: "expense_type", DataTypeOID: 25}},
		Rows: [][]any{{"2026-07", 123.45, "销售部", "机票"}}, RowCount: 1,
	}
	plan := map[string]any{"product": "expense_summary", "columns": []string{"month", "total_amount"}}
	result := mustCallGatewayTool(t, harness.service, harness.alice, "plan_exposure", map[string]any{
		"task_id": "task-planner-v2", "request_id": "planner-v2-request",
		"candidates": []map[string]any{
			{"id": "monthly", "requirement": "monthly", "plan": plan,
				"utility_evidence": map[string]any{"answer_completeness": 1.0, "query_coverage": 1.0}},
			{"id": "trend", "requirement": "trend", "plan": plan,
				"utility_evidence": map[string]any{"answer_completeness": 1.0, "query_coverage": 1.0}},
		},
	})
	exact := result["plan"].(exposure.ExactPlan)
	if len(exact.Selected) != 2 || exact.ReleaseCost != 2 || exact.InfluenceCost != 3 {
		t.Fatalf("V2 exact plan = %+v", exact)
	}
	if got := len(result["results"].([]bufferedCandidateResult)); got != 2 {
		t.Fatalf("released candidate results = %d, want 2", got)
	}
	receipt := result["receipt"].(map[string]any)
	if receipt["version"] != queryreceipt.VersionV5 {
		t.Fatalf("receipt version = %v, want V5", receipt["version"])
	}
	charge := result["exposure"].(control.ExposureCharge)
	if charge.ChargedReleaseFacts != 2 || charge.ChargedInfluenceFacts != 3 || charge.PlannerVersion != exposure.ExactPlannerVersion {
		t.Fatalf("V2 charge = %+v", charge)
	}
	stored, err := harness.store.GetRepresentationPlan(t.Context(), result["query_id"].(string))
	if err != nil || len(stored.Selected) != 2 || stored.UnionEffectSHA256 == "" {
		t.Fatalf("stored representation plan = %+v, %v", stored, err)
	}
}

func TestPlanExposureV2RejectsClientSuppliedCosts(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createExposureV2SummaryTask(t, "task-planner-v2-cost", control.ExposureLimits{ReleaseFacts: 5, InfluenceFacts: 5})
	_, err := callGatewayTool(harness.service, harness.alice, "plan_exposure", map[string]any{
		"task_id": "task-planner-v2-cost",
		"candidates": []map[string]any{{"id": "forged", "requirement": "r",
			"plan":             map[string]any{"product": "expense_summary", "columns": []string{"month"}},
			"utility_evidence": map[string]any{"answer_completeness": 1.0, "query_coverage": 1.0},
			"release_cost":     0, "influence_cost": 0}},
	})
	requireToolCode(t, err, apierr.CodeInvalidRequest)
}
