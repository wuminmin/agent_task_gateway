package experiment

import (
	"errors"
	"fmt"

	"taskbound.local/agent-data-gateway/evaluation/finalv5benign"
)

// BenignEvidenceVersion names the benign-trace evidence wire contract.
const BenignEvidenceVersion = "taskgate-final-v5-benign-evidence-v1"

// benignBudgetNames maps the cell mode to the corpus budget name.
var benignBudgetNames = map[string]string{
	"recipe": "benign-recipe", "x2": "benign-x2", "x4": "benign-x4",
}

// ValidateBenignEvidence is the fail-closed gate for the benign-trace study.
// It pins what is a-priori certain - corpus identity and order, the budget
// binding, budget-independent policy refusals refusing, zero-release
// statements charging nothing, per-step ledger-delta consistency, and
// positive client latency - and deliberately does not predict which
// authorized statements a budget refuses: that pattern is the measurement.
func ValidateBenignEvidence(sample Sample) error {
	if sample.ExperimentID != "benign" || sample.Status != "pass" {
		return errors.New("benign evidence validation requires a passing benign sample")
	}
	budgetName, knownMode := benignBudgetNames[sample.Mode]
	if sample.WorkloadID != finalv5benign.TraceWorkloadID || sample.Scale != "28-statements" ||
		!knownMode || sample.System != "taskgate" {
		return errors.New("benign sample is outside the frozen trace matrix")
	}
	evidence := sample.BenignVerification
	if evidence == nil || evidence.Version != BenignEvidenceVersion ||
		evidence.CorpusID != finalv5benign.CorpusID ||
		evidence.CorpusSHA256 != finalv5benign.CorpusSHA256() ||
		evidence.BudgetName != budgetName {
		return errors.New("benign evidence is absent or not bound to the frozen corpus and budget")
	}
	manifest, err := finalv5benign.Load()
	if err != nil {
		return err
	}
	var budget *finalv5benign.RecipeBudget
	for index := range manifest.Budgets {
		if manifest.Budgets[index].Name == budgetName {
			budget = &manifest.Budgets[index]
		}
	}
	if budget == nil || evidence.MaxReleaseFacts != budget.MaxReleaseFacts ||
		evidence.MaxInfluenceFacts != budget.MaxInfluence || evidence.MaxOutcomeFacts != budget.MaxOutcome {
		return errors.New("benign evidence budget does not equal the corpus recipe")
	}
	if len(evidence.Steps) != len(manifest.Statements) {
		return fmt.Errorf("benign sample carries %d steps, the frozen trace has %d",
			len(evidence.Steps), len(manifest.Statements))
	}
	accepted, refused, budgetRefusals, firstBudgetRefusal := 0, 0, 0, 0
	for position, step := range evidence.Steps {
		want := manifest.Statements[position]
		if step.Index != want.Index || step.ID != want.ID || step.SQLSHA256 != want.SQLSHA256 ||
			step.Classification != string(want.Classification) {
			return fmt.Errorf("benign step %d identity differs from the frozen corpus", position+1)
		}
		if !(step.ClientMS > 0) {
			return fmt.Errorf("benign step %d lacks a positive client_ms", position+1)
		}
		if step.Accepted == step.Rejected {
			return fmt.Errorf("benign step %d must be exactly one of accepted or rejected", position+1)
		}
		if step.Rejected && step.ObservedErrorCode == "" {
			return fmt.Errorf("benign step %d rejection lacks its error code", position+1)
		}
		if step.Before == nil || step.After == nil {
			return fmt.Errorf("benign step %d lacks its ledger snapshots", position+1)
		}
		if step.After.ReleaseCardinality-step.Before.ReleaseCardinality != step.ChargedReleaseFacts ||
			step.After.DependencyCardinality-step.Before.DependencyCardinality != step.ChargedDependencyFacts ||
			step.After.OutcomeCardinality-step.Before.OutcomeCardinality != step.ChargedOutcomeFacts {
			return fmt.Errorf("benign step %d charges disagree with its ledger deltas", position+1)
		}
		if step.Rejected && (step.ChargedReleaseFacts != 0 || step.ChargedDependencyFacts != 0) {
			return fmt.Errorf("benign step %d refusal charged Result or Dependency facts", position+1)
		}
		switch want.Classification {
		case finalv5benign.ClassPolicyRefused:
			// Budget-independent: the statement must refuse under every
			// profile. The exact wire code is recorded, not predicted.
			if step.Accepted {
				return fmt.Errorf("benign step %d is policy-refused a priori but was accepted", position+1)
			}
			refused++
			continue
		case finalv5benign.ClassZeroRelease:
			if step.Accepted && (step.ReleasedRows != 0 || step.ChargedReleaseFacts != 0 ||
				step.ChargedDependencyFacts != 0) {
				return fmt.Errorf("benign step %d is zero-release a priori but charged facts", position+1)
			}
		case finalv5benign.ClassReleased:
			if step.Accepted && step.ReleasedRows != want.ReleasedRows {
				return fmt.Errorf("benign step %d released %d rows, the corpus expects %d",
					position+1, step.ReleasedRows, want.ReleasedRows)
			}
		}
		if step.Accepted {
			accepted++
			continue
		}
		refused++
		budgetRefusals++
		if firstBudgetRefusal == 0 {
			firstBudgetRefusal = position + 1
		}
	}
	if evidence.AcceptedStatements != accepted || evidence.RefusedStatements != refused ||
		evidence.BudgetRefusals != budgetRefusals || evidence.FirstBudgetRefusal != firstBudgetRefusal {
		return errors.New("benign totals disagree with the step evidence")
	}
	if evidence.FinalRoot == nil {
		return errors.New("benign evidence lacks its final root snapshot")
	}
	return nil
}

func validateBenignVerification(sample Sample) error {
	return ValidateBenignEvidence(sample)
}
