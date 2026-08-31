package experiment

import (
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/finalv5benign"
)

func validBenignSample(t *testing.T, mode string) Sample {
	t.Helper()
	manifest, err := finalv5benign.Load()
	if err != nil {
		t.Fatal(err)
	}
	budgetName := map[string]string{"recipe": "benign-recipe", "x2": "benign-x2", "x4": "benign-x4"}[mode]
	var budget finalv5benign.RecipeBudget
	for _, candidate := range manifest.Budgets {
		if candidate.Name == budgetName {
			budget = candidate
		}
	}
	evidence := &BenignVerificationEvidence{Version: BenignEvidenceVersion,
		CorpusID: finalv5benign.CorpusID, CorpusSHA256: finalv5benign.CorpusSHA256(),
		BudgetName: budgetName, BudgetProfile: "final-v5-" + budgetName + "-v1",
		MaxReleaseFacts: budget.MaxReleaseFacts, MaxInfluenceFacts: budget.MaxInfluence,
		MaxOutcomeFacts: budget.MaxOutcome}
	ledger := RootLedgerSnapshot{
		DictionarySetSHA256: strings.Repeat("d", 64), ReleaseSetSHA256: strings.Repeat("a", 64),
		DependencySetSHA256: strings.Repeat("b", 64), OutcomeSetSHA256: strings.Repeat("c", 64),
		RootObservationSetSHA256: strings.Repeat("e", 64)}
	for _, want := range manifest.Statements {
		before := ledger
		step := BenignStepEvidence{Index: want.Index, ID: want.ID, SQLSHA256: want.SQLSHA256,
			Classification: string(want.Classification), ClientMS: 2.5}
		switch want.Classification {
		case finalv5benign.ClassPolicyRefused:
			step.Rejected, step.ObservedErrorCode = true, "SQL_OPERATOR_NOT_ALLOWED"
			evidence.RefusedStatements++
		default:
			step.Accepted = true
			step.ReleasedRows = want.ReleasedRows
			step.ChargedReleaseFacts = want.ReleaseFacts
			step.ChargedDependencyFacts = want.Dependency.Cardinality
			step.ChargedOutcomeFacts = 1
			ledger.ReleaseCardinality += step.ChargedReleaseFacts
			ledger.DependencyCardinality += step.ChargedDependencyFacts
			ledger.OutcomeCardinality += step.ChargedOutcomeFacts
			ledger.Epoch++
			evidence.AcceptedStatements++
		}
		after := ledger
		step.Before, step.After = &before, &after
		evidence.Steps = append(evidence.Steps, step)
	}
	evidence.FinalRoot = &ledger
	return Sample{ExperimentID: "benign", WorkloadID: finalv5benign.TraceWorkloadID,
		Scale: "27-statements", Mode: mode, System: "taskgate", Status: "pass",
		BenignVerification: evidence}
}

func TestBenignTraceEvidenceBindsTheFrozenCorpus(t *testing.T) {
	for _, mode := range []string{"recipe", "x2", "x4"} {
		if err := ValidateBenignEvidence(validBenignSample(t, mode)); err != nil {
			t.Fatalf("valid %s benign sample: %v", mode, err)
		}
	}
	tests := []struct {
		name          string
		mutate        func(*Sample)
		errorContains string
	}{
		{"corpus digest", func(s *Sample) { s.BenignVerification.CorpusSHA256 = strings.Repeat("0", 64) }, "not bound"},
		{"budget drift", func(s *Sample) { s.BenignVerification.MaxInfluenceFacts++ }, "recipe"},
		{"identity drift", func(s *Sample) { s.BenignVerification.Steps[0].SQLSHA256 = strings.Repeat("f", 64) }, "identity"},
		{"policy refusal accepted", func(s *Sample) {
			for index := range s.BenignVerification.Steps {
				if s.BenignVerification.Steps[index].Classification == string(finalv5benign.ClassPolicyRefused) {
					s.BenignVerification.Steps[index].Accepted = true
					s.BenignVerification.Steps[index].Rejected = false
					s.BenignVerification.Steps[index].ObservedErrorCode = ""
					return
				}
			}
		}, "policy-refused"},
		{"delta mismatch", func(s *Sample) { s.BenignVerification.Steps[0].ChargedDependencyFacts++ }, "ledger deltas"},
		{"zero client_ms", func(s *Sample) { s.BenignVerification.Steps[3].ClientMS = 0 }, "client_ms"},
		{"totals drift", func(s *Sample) { s.BenignVerification.AcceptedStatements++ }, "totals"},
		{"missing final root", func(s *Sample) { s.BenignVerification.FinalRoot = nil }, "final root"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sample := validBenignSample(t, "x4")
			test.mutate(&sample)
			if err := ValidateBenignEvidence(sample); err == nil || !strings.Contains(err.Error(), test.errorContains) {
				t.Fatalf("mutated benign evidence (%s) = %v", test.name, err)
			}
		})
	}
}
