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

func TestPlanExposureRejectsScalarV1Contract(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createExposureSummaryTask(t, "task-planner", control.ExposureLimits{ReleaseFacts: 6, InfluenceFacts: 8})

	_, err := callGatewayTool(harness.service, harness.alice, "plan_exposure", map[string]any{
		"task_id": "task-planner",
		"candidates": []map[string]any{
			{"id": "forged", "requirement": "detail", "product": "expense_summary",
				"release_cost": 0, "influence_cost": 0, "answer_completeness": 1.0},
		},
	})
	requireToolCode(t, err, apierr.CodeExposureEvidenceRequired)
}

func TestPlanExposureRejectsUnapprovedProducts(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createExposureV2SummaryTask(t, "task-planner-scope", control.ExposureLimits{ReleaseFacts: 6, InfluenceFacts: 8})
	_, err := callGatewayTool(harness.service, harness.alice, "plan_exposure", map[string]any{
		"task_id":      "task-planner-scope",
		"requirements": []map[string]any{{"id": "people", "required_outputs": []string{"employee_no"}}},
		"candidates": []map[string]any{{
			"id": "employee-raw", "requirement": "people",
			"plan": map[string]any{"product": "employee_directory", "columns": []string{"employee_no"}},
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
		Rows: [][]any{{"2026-07", "123.45", "销售部", "机票"}}, RowCount: 1,
	}
	plan := map[string]any{"product": "expense_summary", "columns": []string{"month", "total_amount"}}
	result := mustCallGatewayTool(t, harness.service, harness.alice, "plan_exposure", map[string]any{
		"task_id": "task-planner-v2", "request_id": "planner-v2-request",
		"requirements": []map[string]any{
			{"id": "monthly", "required_outputs": []string{"month", "total_amount"}},
			{"id": "trend", "required_outputs": []string{"month", "total_amount"}},
		},
		"candidates": []map[string]any{
			{"id": "monthly", "requirement": "monthly", "plan": plan},
			{"id": "trend", "requirement": "trend", "plan": plan},
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

func TestPlanExposureV2RejectsClientSuppliedCostsAndUtility(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createExposureV2SummaryTask(t, "task-planner-v2-cost", control.ExposureLimits{ReleaseFacts: 5, InfluenceFacts: 5})
	_, err := callGatewayTool(harness.service, harness.alice, "plan_exposure", map[string]any{
		"task_id":      "task-planner-v2-cost",
		"requirements": []map[string]any{{"id": "r", "required_outputs": []string{"month"}}},
		"candidates": []map[string]any{{"id": "forged", "requirement": "r",
			"plan":             map[string]any{"product": "expense_summary", "columns": []string{"month"}},
			"utility_evidence": map[string]any{"answer_completeness": 1.0, "query_coverage": 1.0},
			"release_cost":     0, "influence_cost": 0}},
	})
	requireToolCode(t, err, apierr.CodeInvalidRequest)
}

func TestDeriveRequiredOutputUtilityUsesExecutedSchemaAndTruncation(t *testing.T) {
	result := bufferedCandidateResult{Columns: []dataconnector.Column{{Name: "month"}, {Name: "total_amount"}}}
	utility := deriveRequiredOutputUtility([]string{"month", "total_amount", "request_count"}, result)
	if utility.QueryCoverage != 2.0/3.0 || utility.AnswerCompleteness != 2.0/3.0 {
		t.Fatalf("complete result utility = %+v", utility)
	}
	result.Limited = true
	utility = deriveRequiredOutputUtility([]string{"month", "total_amount"}, result)
	if utility.QueryCoverage != 1 || utility.AnswerCompleteness != 0 {
		t.Fatalf("truncated result utility = %+v", utility)
	}
}
