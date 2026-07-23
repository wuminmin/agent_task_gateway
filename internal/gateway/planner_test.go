package gateway

import (
	"testing"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/exposure"
)

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
