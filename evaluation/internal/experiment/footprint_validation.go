package experiment

import (
	"errors"
	"fmt"

	"taskbound.local/agent-data-gateway/evaluation/finalv5footprint"
)

// FootprintEvidenceVersion names the footprint evidence wire contract.
const FootprintEvidenceVersion = "taskgate-final-v5-footprint-evidence-v1"

// ValidateFootprintEvidence is the adapter-side fail-closed gate for the
// refused-footprint ladder: every rung must match the frozen corpus identity,
// carry the a-priori acceptance or refusal decision, the corpus-derived
// charges, and a positive client-observed latency.
func ValidateFootprintEvidence(sample Sample) error {
	if sample.ExperimentID != "footprint" || sample.Status != "pass" {
		return errors.New("footprint evidence validation requires a passing footprint sample")
	}
	if sample.WorkloadID != "refused-footprint-ladder-v1" || sample.Scale != "1e5-rows" ||
		(sample.Mode != "bounded" && sample.Mode != "unlimited") || sample.System != "taskgate" {
		return errors.New("footprint sample is outside the frozen ladder matrix")
	}
	evidence := sample.FootprintVerification
	if evidence == nil || evidence.Version != FootprintEvidenceVersion ||
		evidence.CorpusID != finalv5footprint.CorpusID ||
		evidence.CorpusSHA256 != finalv5footprint.CorpusSHA256() ||
		evidence.BoundedMaxDependencyFacts != finalv5footprint.BoundedMaxDependencyFacts {
		return errors.New("footprint evidence is absent or not bound to the frozen ladder corpus")
	}
	wantProduct, wantProfile := finalv5footprint.UnlimitedProduct, finalv5footprint.UnlimitedProfile
	if sample.Mode == "bounded" {
		wantProduct, wantProfile = finalv5footprint.BoundedProduct, finalv5footprint.BoundedProfile
	}
	if evidence.Product != wantProduct || evidence.BudgetProfile != wantProfile {
		return errors.New("footprint sample is not bound to its arm product and budget profile")
	}
	manifest, err := finalv5footprint.Load()
	if err != nil {
		return err
	}
	if len(evidence.Rungs) != len(manifest.Rungs) {
		return fmt.Errorf("footprint sample carries %d rungs, the frozen ladder has %d",
			len(evidence.Rungs), len(manifest.Rungs))
	}
	if sample.RootTaskIDHash != evidence.Rungs[0].RootTaskIDHash {
		return errors.New("footprint sample root identity is not the first rung's root")
	}
	accepted, refused := 0, 0
	for position, rung := range evidence.Rungs {
		want := manifest.Rungs[position]
		if rung.Index != want.Index || rung.ID != want.ID || rung.Rows != want.Rows ||
			!equalStringSlices(rung.Columns, want.Columns) ||
			rung.DirectSQLSHA256 != sha256Hex([]byte(want.DirectSQL)) ||
			rung.LogicalSQLSHA256 != sha256Hex([]byte(want.LogicalSQL(wantProduct))) {
			return fmt.Errorf("footprint rung %d identity differs from the frozen corpus", position+1)
		}
		if !(rung.ClientMS > 0) {
			return fmt.Errorf("footprint rung %d lacks a positive client_ms", position+1)
		}
		if !validSHA256(rung.RootTaskIDHash) {
			return fmt.Errorf("footprint rung %d lacks its fresh root identity", position+1)
		}
		if rung.ExpectedDependencyFacts != want.Dependency.Cardinality {
			return fmt.Errorf("footprint rung %d expected-dependency does not match the corpus", position+1)
		}
		wantRefused := sample.Mode == "bounded" && want.BoundedRefused
		if wantRefused {
			if rung.Accepted || !rung.Rejected || rung.ObservedErrorCode != want.BoundedRefusalCode() ||
				rung.ChargedReleaseFacts != 0 || rung.ChargedDependencyFacts != 0 || rung.ChargedOutcomeFacts != 0 ||
				len(rung.ObservedScalars) != 0 {
				return fmt.Errorf("footprint rung %d must carry its a-priori refusal code without charging", position+1)
			}
			refused++
			continue
		}
		if !rung.Accepted || rung.Rejected || rung.ObservedErrorCode != "" {
			return fmt.Errorf("footprint rung %d must be accepted under its arm", position+1)
		}
		if rung.ChargedDependencyFacts != want.Dependency.Cardinality ||
			rung.ChargedReleaseFacts != want.Release.Cardinality {
			return fmt.Errorf("footprint rung %d charges differ from the corpus-derived footprint", position+1)
		}
		if !equalStringSlices(rung.ObservedScalars, want.ExpectedScalars) {
			return fmt.Errorf("footprint rung %d observed scalars differ from the closed-form expectations", position+1)
		}
		accepted++
	}
	if evidence.AcceptedRungs != accepted || evidence.RefusedRungs != refused {
		return errors.New("footprint acceptance totals disagree with the rung evidence")
	}
	if sample.Mode == "bounded" && accepted != 1 {
		return fmt.Errorf("bounded footprint arm accepted %d rungs, the frozen design accepts exactly one", accepted)
	}
	if sample.Mode == "unlimited" && refused != 0 {
		return errors.New("unlimited footprint arm must accept every rung")
	}
	return nil
}

func validateFootprintVerification(sample Sample) error {
	return ValidateFootprintEvidence(sample)
}

func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
