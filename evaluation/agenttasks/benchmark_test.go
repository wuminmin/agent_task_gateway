package agenttasks

import "testing"

func TestAgentTaskCampaignIsCompleteAndBudgetSafe(t *testing.T) {
	report, err := Run()
	if err != nil {
		t.Fatal(err)
	}
	if report.Tasks != 120 || len(report.Policies) != 3 {
		t.Fatalf("campaign size = %d tasks/%d policies", report.Tasks, len(report.Policies))
	}
	for _, policy := range report.Policies {
		if policy.BudgetViolations != 0 {
			t.Fatalf("policy %s has %d budget violations", policy.Policy, policy.BudgetViolations)
		}
	}
	if report.Policies[0].TaskSuccessRate <= report.Policies[1].TaskSuccessRate ||
		report.Policies[0].MeanAnswerCompleteness <= report.Policies[1].MeanAnswerCompleteness {
		t.Fatalf("exact policy did not improve externally scored utility: %+v", report.Policies)
	}
	if report.Policies[0].TaskSuccessRate <= report.Policies[2].TaskSuccessRate {
		t.Fatalf("history did not improve task success: %+v", report.Policies)
	}
}
