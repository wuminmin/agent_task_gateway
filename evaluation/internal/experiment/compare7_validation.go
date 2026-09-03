package experiment

import (
	"errors"
	"fmt"

	"taskbound.local/agent-data-gateway/evaluation/finalv5compare"
)

// Compare7EvidenceVersion names the comparison evidence wire contract.
const Compare7EvidenceVersion = "taskgate-final-v5-compare7-evidence-v1"

// ValidateCompare7Evidence is the fail-closed gate for the P9.F comparison
// sequence. It pins the corpus identity and step order, requires every
// repeat of an accepted statement to charge nothing, and requires ledger
// deltas to be internally consistent; which unique statements the recipe
// refuses is the measurement and stays unpinned.
func ValidateCompare7Evidence(sample Sample) error {
	if sample.ExperimentID != "compare7" || sample.Status != "pass" {
		return errors.New("compare7 evidence validation requires a passing compare7 sample")
	}
	if sample.WorkloadID != "compare7-sequence-v1" || sample.Scale != "seq-60" ||
		sample.Mode != "bdg" || sample.System != "taskgate" {
		return errors.New("compare7 sample is outside the frozen sequence matrix")
	}
	evidence := sample.Compare7Verification
	if evidence == nil || evidence.Version != Compare7EvidenceVersion ||
		evidence.CorpusID != finalv5compare.CorpusID ||
		evidence.CorpusSHA256 != finalv5compare.CorpusSHA256() ||
		evidence.Product != finalv5compare.Product ||
		evidence.MaxDependencyFacts != finalv5compare.RecipeMaxDependencyFacts {
		return errors.New("compare7 evidence is absent or not bound to the frozen corpus")
	}
	manifest, err := finalv5compare.Load()
	if err != nil {
		return err
	}
	if len(evidence.Steps) != len(manifest.Steps) {
		return fmt.Errorf("compare7 sample carries %d steps, the frozen sequence has %d",
			len(evidence.Steps), len(manifest.Steps))
	}
	accepted, refusals, first := 0, 0, 0
	acceptedByIndex := map[int]bool{}
	var repeatCharges, ledger int64
	for position, step := range evidence.Steps {
		want := manifest.Steps[position]
		if step.Index != want.Index || step.RepeatOf != want.RepeatOf || step.Bound != want.Bound {
			return fmt.Errorf("compare7 step %d identity differs from the frozen corpus", position+1)
		}
		if !(step.ClientMS > 0) {
			return fmt.Errorf("compare7 step %d lacks a positive client_ms", position+1)
		}
		if step.Accepted == step.Rejected {
			return fmt.Errorf("compare7 step %d must be exactly one of accepted or rejected", position+1)
		}
		if step.Rejected {
			if step.ObservedErrorCode == "" {
				return fmt.Errorf("compare7 step %d rejection lacks its error code", position+1)
			}
			if step.ChargedReleaseFacts != 0 || step.ChargedDependencyFacts != 0 || step.ChargedOutcomeFacts != 0 {
				return fmt.Errorf("compare7 step %d refusal charged the ledger", position+1)
			}
			refusals++
			if first == 0 {
				first = step.Index
			}
		} else {
			accepted++
			if step.RepeatOf != 0 && acceptedByIndex[step.RepeatOf] {
				repeatCharges += step.ChargedReleaseFacts + step.ChargedDependencyFacts
				if step.ChargedDependencyFacts != 0 {
					return fmt.Errorf("compare7 step %d repeated an accepted statement but charged dependency facts", position+1)
				}
			}
			acceptedByIndex[step.Index] = true
		}
		ledger += step.ChargedDependencyFacts
		if step.LedgerDependency != ledger {
			return fmt.Errorf("compare7 step %d ledger disagrees with the cumulative charges", position+1)
		}
	}
	if evidence.AcceptedStatements != accepted || evidence.BudgetRefusals != refusals ||
		evidence.FirstBudgetRefusal != first || evidence.RepeatCharges != repeatCharges {
		return errors.New("compare7 summary disagrees with its steps")
	}
	if ledger > evidence.MaxDependencyFacts {
		return errors.New("compare7 ledger exceeded the recipe ceiling without refusing")
	}
	return nil
}
