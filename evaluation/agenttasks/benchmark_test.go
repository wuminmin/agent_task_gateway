package agenttasks

import "testing"

func TestAgentTaskCampaignIsCompleteAndNonIsomorphic(t *testing.T) {
	report, err := Run()
	if err != nil {
		t.Fatal(err)
	}
	if report.Tasks != 24 || len(report.Policies) != 6 || report.Kinds != 5 {
		t.Fatalf("campaign shape = %d tasks/%d policies/%d kinds, want 24/6/5",
			report.Tasks, len(report.Policies), report.Kinds)
	}
	if report.UtilitySignal == "" {
		t.Fatal("utility signal must be declared and decoupled from gold reveal")
	}

	byPolicy := make(map[string]PolicyResult, len(report.Policies))
	for _, policy := range report.Policies {
		byPolicy[policy.Policy] = policy
	}
	// The exact overlap-aware planner must never breach the dual budget.
	if byPolicy["taskgate_exact"].BudgetViolations != 0 {
		t.Fatalf("exact planner has %d budget violations", byPolicy["taskgate_exact"].BudgetViolations)
	}
	// Exact dominates the degenerate single-candidate baseline and is at least as
	// good as greedy by both success and independent gold completeness.
	if byPolicy["taskgate_exact"].TaskSuccessRate <= byPolicy["single_candidate"].TaskSuccessRate {
		t.Fatalf("exact did not beat single-candidate: %+v", byPolicy)
	}
	if byPolicy["taskgate_exact"].TaskSuccessRate < byPolicy["utility_greedy"].TaskSuccessRate {
		t.Fatalf("exact below greedy: %+v", byPolicy)
	}
	// History must not hurt: the history-aware exact policy is at least as
	// successful as the no-history variant.
	if byPolicy["taskgate_exact"].TaskSuccessRate < byPolicy["taskgate_exact_no_history"].TaskSuccessRate {
		t.Fatalf("history-aware exact below no-history: %+v", byPolicy)
	}

	// Report the full policy and per-kind tables for visibility.
	for _, policy := range report.Policies {
		t.Logf("policy %-26s success=%2d/%d (%.2f) completeness=%.3f violations=%d",
			policy.Policy, policy.TaskSuccesses, policy.Tasks, policy.TaskSuccessRate,
			policy.MeanAnswerCompleteness, policy.BudgetViolations)
	}
	for _, kind := range report.Results {
		t.Logf("kind %-22s tasks=%d exact=%d greedy=%d additive=%d",
			kind.Kind, kind.Tasks, kind.ExactSuccesses, kind.GreedySuccesses, kind.AdditiveSuccess[kind.Kind])
	}
}
