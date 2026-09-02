package experiment

import (
	"errors"
	"fmt"

	"taskbound.local/agent-data-gateway/evaluation/finalv5scale7"
)

// Scale7EvidenceVersion names the scale-ladder evidence wire contract.
const Scale7EvidenceVersion = "taskgate-final-v5-scale7-evidence-v1"

// ValidateScale7Evidence is the fail-closed gate for the P9.E scale-point
// study. It pins what is a-priori certain - corpus identity and rung order,
// the exact scalars from the closed-form model, novel charges equal to the
// derived footprints on a fresh root, and zero-charge semantic replay - and
// leaves the latencies themselves as the measurement.
func ValidateScale7Evidence(sample Sample) error {
	if sample.ExperimentID != "scale7" || sample.Status != "pass" {
		return errors.New("scale7 evidence validation requires a passing scale7 sample")
	}
	if sample.WorkloadID != "scale7-ladder-v1" || sample.Scale != "sum-ladder" ||
		sample.System != "taskgate" {
		return errors.New("scale7 sample is outside the frozen ladder matrix")
	}
	evidence := sample.Scale7Verification
	if evidence == nil || evidence.Version != Scale7EvidenceVersion ||
		evidence.CorpusID != finalv5scale7.CorpusID ||
		evidence.CorpusSHA256 != finalv5scale7.CorpusSHA256() ||
		evidence.Mode != sample.Mode ||
		evidence.MaxDependencyFacts != finalv5scale7.MaxDependencyFacts {
		return errors.New("scale7 evidence is absent or not bound to the frozen corpus")
	}
	manifest, err := finalv5scale7.Load()
	if err != nil {
		return err
	}
	if len(evidence.Rungs) != len(manifest.Rungs) {
		return fmt.Errorf("scale7 sample carries %d rungs, the frozen ladder has %d",
			len(evidence.Rungs), len(manifest.Rungs))
	}
	accepted := 0
	for position, step := range evidence.Rungs {
		want := manifest.Rungs[position]
		if step.Index != want.Index || step.ID != want.ID || step.Rows != want.Rows {
			return fmt.Errorf("scale7 rung %d identity differs from the frozen corpus", position+1)
		}
		if !(step.ClientMS > 0) {
			return fmt.Errorf("scale7 rung %d lacks a positive client_ms", position+1)
		}
		if !step.Accepted {
			return fmt.Errorf("scale7 rung %d refused (%s); the ladder budget admits every rung",
				position+1, step.ObservedErrorCode)
		}
		accepted++
		if len(step.ObservedScalars) != len(want.ExpectedScalars) {
			return fmt.Errorf("scale7 rung %d scalar arity differs from the model", position+1)
		}
		for i, scalar := range step.ObservedScalars {
			if scalar != want.ExpectedScalars[i] {
				return fmt.Errorf("scale7 rung %d scalar %d = %s, the closed-form model derives %s",
					position+1, i+1, scalar, want.ExpectedScalars[i])
			}
		}
		if evidence.Mode != "direct" {
			if step.ChargedDependencyFacts != want.Dependency.Cardinality ||
				step.ExpectedDependencyFacts != want.Dependency.Cardinality {
				return fmt.Errorf("scale7 rung %d charged %d dependency facts, the derived footprint is %d",
					position+1, step.ChargedDependencyFacts, want.Dependency.Cardinality)
			}
			if step.ChargedReleaseFacts != want.Release.Cardinality {
				return fmt.Errorf("scale7 rung %d charged %d release facts, the ladder derives %d",
					position+1, step.ChargedReleaseFacts, want.Release.Cardinality)
			}
		}
		if evidence.Mode == "replay" {
			if !(step.ReplayClientMS > 0) || step.ReplayChargedFacts != 0 {
				return fmt.Errorf("scale7 rung %d replay must time a zero-charge settlement", position+1)
			}
		}
	}
	if evidence.AcceptedRungs != accepted {
		return errors.New("scale7 accepted-rung count disagrees with its steps")
	}
	return nil
}
