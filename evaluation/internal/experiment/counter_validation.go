package experiment

import (
	"errors"
	"fmt"

	"taskbound.local/agent-data-gateway/evaluation/finalv5counter"
)

// CounterEvidenceVersion names the comparator-arm evidence wire contract.
const CounterEvidenceVersion = "taskgate-final-v5-counter-evidence-v1"

// counterRefusalCodes maps the corpus's a-priori refusal kinds to the wire
// codes the production Gateway answers with.
var counterRefusalCodes = map[string]string{
	"exposure": "EXPOSURE_BUDGET_EXHAUSTED",
	"resource": "BUDGET_EXHAUSTED",
	"archived": "TASK_NOT_ACTIVE",
}

// ValidateCounterEvidence is the fail-closed gate for the comparator study:
// unlike the benign trace, every step's accept/refuse outcome is a-priori
// derivable, so the whole table is pinned - outcome, refusal-code family,
// released rows on acceptance, zero charges on refusal, ledger-delta
// consistency, and positive client latency.
func ValidateCounterEvidence(sample Sample) error {
	if sample.ExperimentID != "counter" || sample.Status != "pass" {
		return errors.New("counter evidence validation requires a passing counter sample")
	}
	armKnown := false
	for _, arm := range finalv5counter.Arms {
		armKnown = armKnown || sample.Mode == arm
	}
	orderingKnown := false
	for _, ordering := range finalv5counter.Orderings {
		orderingKnown = orderingKnown || sample.Scale == ordering
	}
	if sample.WorkloadID != finalv5counter.WorkloadID || !armKnown || !orderingKnown ||
		sample.System != "taskgate" {
		return errors.New("counter sample is outside the frozen comparator matrix")
	}
	evidence := sample.CounterVerification
	if evidence == nil || evidence.Version != CounterEvidenceVersion ||
		evidence.CorpusID != finalv5counter.CorpusID ||
		evidence.CorpusSHA256 != finalv5counter.CorpusSHA256() ||
		evidence.Arm != sample.Mode || evidence.Ordering != sample.Scale ||
		evidence.BudgetProfile != finalv5counter.ArmProfiles[sample.Mode] {
		return errors.New("counter evidence is absent or not bound to the frozen corpus cell")
	}
	manifest, err := finalv5counter.Load()
	if err != nil {
		return err
	}
	want, err := manifest.Trace(sample.Mode, sample.Scale)
	if err != nil {
		return err
	}
	if len(evidence.Steps) != len(want.Steps) {
		return fmt.Errorf("counter sample carries %d steps, the frozen trace has %d",
			len(evidence.Steps), len(want.Steps))
	}
	accepted, refused, firstRefusal := 0, 0, 0
	for position, step := range evidence.Steps {
		expected := want.Steps[position]
		if step.Position != expected.Position || step.SourceIndex != expected.SourceIndex ||
			step.StepID != expected.StepID {
			return fmt.Errorf("counter step %d identity differs from the frozen ordering", position+1)
		}
		if !(step.ClientMS > 0) {
			return fmt.Errorf("counter step %d lacks a positive client_ms", position+1)
		}
		if step.Accepted == step.Rejected {
			return fmt.Errorf("counter step %d must be exactly one of accepted or rejected", position+1)
		}
		if step.Before == nil || step.After == nil {
			return fmt.Errorf("counter step %d lacks its ledger snapshots", position+1)
		}
		if step.After.ReleaseCardinality-step.Before.ReleaseCardinality != step.ChargedReleaseFacts ||
			step.After.DependencyCardinality-step.Before.DependencyCardinality != step.ChargedDependencyFacts ||
			step.After.OutcomeCardinality-step.Before.OutcomeCardinality != step.ChargedOutcomeFacts {
			return fmt.Errorf("counter step %d charges disagree with its ledger deltas", position+1)
		}
		if step.Accepted != expected.Accepted {
			return fmt.Errorf("counter step %d observed %v, the a-priori table says %v",
				position+1, step.Accepted, expected.Accepted)
		}
		if step.Accepted {
			if step.ReleasedRows != expected.ReleasedRows {
				return fmt.Errorf("counter step %d released %d rows, the corpus expects %d",
					position+1, step.ReleasedRows, expected.ReleasedRows)
			}
			accepted++
			continue
		}
		if step.ChargedReleaseFacts != 0 || step.ChargedDependencyFacts != 0 {
			return fmt.Errorf("counter step %d refusal charged Result or Dependency facts", position+1)
		}
		if wantCode := counterRefusalCodes[expected.RefusalKind]; step.ObservedErrorCode != wantCode {
			return fmt.Errorf("counter step %d refused with %q, the a-priori kind %q maps to %q",
				position+1, step.ObservedErrorCode, expected.RefusalKind, wantCode)
		}
		refused++
		if firstRefusal == 0 {
			firstRefusal = position + 1
		}
	}
	if evidence.AcceptedSteps != accepted || evidence.RefusedSteps != refused ||
		evidence.FirstRefusal != firstRefusal ||
		accepted != want.AcceptedSteps || refused != want.RefusedSteps {
		return errors.New("counter totals disagree with the a-priori table")
	}
	if evidence.FinalRoot == nil {
		return errors.New("counter evidence lacks its final root snapshot")
	}
	if sample.Mode == "exact" || sample.Mode == "release" {
		// The exact and release arms meter the ledger itself; the frozen
		// distinct totals must be visible in the final root.
		if evidence.FinalRoot.DependencyCardinality != want.DistinctDep ||
			evidence.FinalRoot.ReleaseCardinality != want.DistinctRelease {
			return errors.New("counter final root differs from the frozen distinct totals")
		}
	}
	return nil
}

func validateCounterVerification(sample Sample) error {
	return ValidateCounterEvidence(sample)
}
