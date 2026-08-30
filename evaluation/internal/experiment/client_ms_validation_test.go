package experiment

import (
	"strings"
	"testing"
)

// Evidence-v2 samples must time every step; sealed v1 samples must not carry
// the field. The rule is uniform across workloads and arms.
func TestRLSEvidenceV2RequiresPositiveClientMSAndV1ForbidsIt(t *testing.T) {
	sample := validDirectRLSSample(t)
	v2 := cloneRLSSample(t, sample)
	v2.RLSVerification.Version = rlsEvidenceVersionV2
	for index := range v2.RLSVerification.Steps {
		v2.RLSVerification.Steps[index].ClientMS = 1.5
	}
	if err := ValidateRLSEvidence(v2); err != nil {
		t.Fatalf("v2 evidence with positive client_ms: %v", err)
	}
	missing := cloneRLSSample(t, v2)
	missing.RLSVerification.Steps[3].ClientMS = 0
	if err := ValidateRLSEvidence(missing); err == nil || !strings.Contains(err.Error(), "client_ms") {
		t.Fatalf("v2 step without client_ms was accepted: %v", err)
	}
	v1 := cloneRLSSample(t, sample)
	v1.RLSVerification.Steps[3].ClientMS = 2
	if err := ValidateRLSEvidence(v1); err == nil || !strings.Contains(err.Error(), "client_ms") {
		t.Fatalf("v1 step with client_ms was accepted: %v", err)
	}
}
