package experiment

import (
	"errors"
	"fmt"

	"taskbound.local/agent-data-gateway/evaluation/finalv5adversary"
)

// AdversaryEvidenceVersion names the optimizing-adversary evidence wire
// contract.
const AdversaryEvidenceVersion = "taskgate-final-v5-adversary-evidence-v1"

// ValidateAdversaryEvidence is the fail-closed gate for the P9.C
// optimizing-adversary study: every step's accept/refuse outcome, released
// rows, scalar answer, and the final recovery summary are a-priori derivable,
// so the whole table is pinned against the frozen corpus. Every refusal in
// the frozen traces is an exposure refusal (the query ceiling is proven
// unreachable at corpus build time), so the only admissible refusal code is
// EXPOSURE_BUDGET_EXHAUSTED with zero Result/Dependency charges.
func ValidateAdversaryEvidence(sample Sample) error {
	if sample.ExperimentID != "adversary" || sample.Status != "pass" {
		return errors.New("adversary evidence validation requires a passing adversary sample")
	}
	tierKnown := ""
	for _, tier := range finalv5adversary.Tiers {
		if sample.Mode == tier.Name {
			tierKnown = tier.BudgetProfile
		}
	}
	strategyKnown := false
	for _, strategy := range finalv5adversary.Strategies {
		strategyKnown = strategyKnown || sample.Scale == strategy
	}
	if sample.WorkloadID != finalv5adversary.WorkloadID || tierKnown == "" || !strategyKnown ||
		sample.System != "taskgate" {
		return errors.New("adversary sample is outside the frozen strategy matrix")
	}
	evidence := sample.AdversaryVerification
	if evidence == nil || evidence.Version != AdversaryEvidenceVersion ||
		evidence.CorpusID != finalv5adversary.CorpusID ||
		evidence.CorpusSHA256 != finalv5adversary.CorpusSHA256() ||
		evidence.Tier != sample.Mode || evidence.Strategy != sample.Scale ||
		evidence.BudgetProfile != tierKnown {
		return errors.New("adversary evidence is absent or not bound to the frozen corpus cell")
	}
	manifest, err := finalv5adversary.Load()
	if err != nil {
		return err
	}
	want, err := manifest.Trace(sample.Mode, sample.Scale)
	if err != nil {
		return err
	}
	if len(evidence.Steps) != len(want.Steps) {
		return fmt.Errorf("adversary sample carries %d steps, the frozen trace has %d",
			len(evidence.Steps), len(want.Steps))
	}
	accepted, refused := 0, 0
	for position, step := range evidence.Steps {
		expected := want.Steps[position]
		if step.Position != expected.Position || step.StepID != expected.StepID ||
			step.Threshold != expected.Threshold {
			return fmt.Errorf("adversary step %d identity differs from the frozen trace", position+1)
		}
		if !(step.ClientMS > 0) {
			return fmt.Errorf("adversary step %d lacks a positive client_ms", position+1)
		}
		if step.Accepted == step.Rejected {
			return fmt.Errorf("adversary step %d must be exactly one of accepted or rejected", position+1)
		}
		if step.Before == nil || step.After == nil {
			return fmt.Errorf("adversary step %d lacks its ledger snapshots", position+1)
		}
		if step.After.ReleaseCardinality-step.Before.ReleaseCardinality != step.ChargedReleaseFacts ||
			step.After.DependencyCardinality-step.Before.DependencyCardinality != step.ChargedDependencyFacts ||
			step.After.OutcomeCardinality-step.Before.OutcomeCardinality != step.ChargedOutcomeFacts {
			return fmt.Errorf("adversary step %d charges disagree with its ledger deltas", position+1)
		}
		if step.Accepted != expected.Accepted {
			return fmt.Errorf("adversary step %d observed %v, the a-priori table says %v",
				position+1, step.Accepted, expected.Accepted)
		}
		if step.Accepted {
			if step.ReleasedRows != expected.ReleasedRows {
				return fmt.Errorf("adversary step %d released %d rows, the corpus expects %d",
					position+1, step.ReleasedRows, expected.ReleasedRows)
			}
			if expected.ScalarCount != nil &&
				(step.ScalarCount == nil || *step.ScalarCount != *expected.ScalarCount) {
				return fmt.Errorf("adversary step %d scalar answer differs from the fixture", position+1)
			}
			accepted++
			continue
		}
		if step.ChargedReleaseFacts != 0 || step.ChargedDependencyFacts != 0 || step.ChargedOutcomeFacts != 0 {
			return fmt.Errorf("adversary step %d refusal charged exposure facts", position+1)
		}
		if step.ObservedErrorCode != "EXPOSURE_BUDGET_EXHAUSTED" {
			return fmt.Errorf("adversary step %d refused with %q, the frozen traces admit only EXPOSURE_BUDGET_EXHAUSTED",
				position+1, step.ObservedErrorCode)
		}
		refused++
	}
	if evidence.AcceptedSteps != accepted || evidence.RefusedSteps != refused ||
		accepted != want.AcceptedSteps || refused != want.RefusedSteps {
		return errors.New("adversary totals disagree with the a-priori table")
	}
	if evidence.RecoveredLo != want.RecoveredLo || evidence.RecoveredHi != want.RecoveredHi ||
		evidence.RecoveredBits != want.RecoveredBits {
		return errors.New("adversary recovery summary disagrees with the a-priori table")
	}
	if (evidence.RecoveredValue == nil) != (want.RecoveredValue == nil) ||
		(want.RecoveredValue != nil && *evidence.RecoveredValue != *want.RecoveredValue) {
		return errors.New("adversary recovered value disagrees with the a-priori table")
	}
	if evidence.FinalRoot == nil {
		return errors.New("adversary evidence lacks its final root snapshot")
	}
	if evidence.FinalRoot.ReleaseCardinality != want.DistinctRelease ||
		evidence.FinalRoot.DependencyCardinality != want.DistinctDep ||
		evidence.FinalRoot.OutcomeCardinality != want.DistinctOutcome {
		return errors.New("adversary final root differs from the frozen distinct totals")
	}
	return nil
}

func validateAdversaryVerification(sample Sample) error {
	return ValidateAdversaryEvidence(sample)
}
