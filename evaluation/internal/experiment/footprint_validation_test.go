package experiment

import (
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/finalv5footprint"
)

func validFootprintSample(t *testing.T, mode string) Sample {
	t.Helper()
	manifest, err := finalv5footprint.Load()
	if err != nil {
		t.Fatal(err)
	}
	product := finalv5footprint.UnlimitedProduct
	profile := finalv5footprint.UnlimitedProfile
	if mode == "bounded" {
		product, profile = finalv5footprint.BoundedProduct, finalv5footprint.BoundedProfile
	}
	evidence := &FootprintVerificationEvidence{Version: FootprintEvidenceVersion,
		CorpusID: finalv5footprint.CorpusID, CorpusSHA256: finalv5footprint.CorpusSHA256(),
		Product: product, BudgetProfile: profile,
		BoundedMaxDependencyFacts: finalv5footprint.BoundedMaxDependencyFacts}
	for _, want := range manifest.Rungs {
		rung := FootprintRungEvidence{Index: want.Index, ID: want.ID, Rows: want.Rows,
			Columns:          append([]string(nil), want.Columns...),
			DirectSQLSHA256:  sha256Hex([]byte(want.DirectSQL)),
			LogicalSQLSHA256: sha256Hex([]byte(want.LogicalSQL(product))),
			ClientMS:         1.5, RootTaskIDHash: strings.Repeat("a", 64),
			ExpectedDependencyFacts: want.Dependency.Cardinality}
		if mode == "bounded" && want.BoundedRefused {
			rung.Rejected, rung.ObservedErrorCode = true, want.BoundedRefusalCode()
			evidence.RefusedRungs++
		} else {
			rung.Accepted = true
			rung.ChargedDependencyFacts = want.Dependency.Cardinality
			rung.ChargedReleaseFacts = want.Release.Cardinality
			rung.ObservedScalars = append([]string(nil), want.ExpectedScalars...)
			evidence.AcceptedRungs++
		}
		evidence.Rungs = append(evidence.Rungs, rung)
	}
	sample := Sample{ExperimentID: "footprint", WorkloadID: "refused-footprint-ladder-v1",
		Scale: "1e5-rows", Mode: mode, System: "taskgate", Status: "pass",
		FootprintVerification: evidence}
	return sample
}

func TestFootprintLadderEvidenceBindsTheFrozenCorpus(t *testing.T) {
	for _, mode := range []string{"bounded", "unlimited"} {
		if err := ValidateFootprintEvidence(validFootprintSample(t, mode)); err != nil {
			t.Fatalf("valid %s footprint sample: %v", mode, err)
		}
	}
	bounded := validFootprintSample(t, "bounded")
	if bounded.FootprintVerification.AcceptedRungs != 1 || bounded.FootprintVerification.RefusedRungs != 11 {
		t.Fatalf("bounded arm totals = %d/%d, the a-priori design gives 1/11",
			bounded.FootprintVerification.AcceptedRungs, bounded.FootprintVerification.RefusedRungs)
	}
	tests := []struct {
		name          string
		mutate        func(*Sample)
		errorContains string
	}{
		{"corpus digest", func(s *Sample) { s.FootprintVerification.CorpusSHA256 = strings.Repeat("0", 64) }, "not bound"},
		{"missing refusal", func(s *Sample) {
			s.FootprintVerification.Rungs[3].Rejected = false
			s.FootprintVerification.Rungs[3].Accepted = true
		}, "refusal code"},
		{"swapped refusal site code", func(s *Sample) {
			s.FootprintVerification.Rungs[3].ObservedErrorCode = "EXPOSURE_BUDGET_EXHAUSTED"
		}, "refusal code"},
		{"zero client_ms", func(s *Sample) { s.FootprintVerification.Rungs[0].ClientMS = 0 }, "client_ms"},
		{"drifted scalar", func(s *Sample) { s.FootprintVerification.Rungs[0].ObservedScalars[0] = "1/1" }, "scalars"},
		{"charge drift", func(s *Sample) { s.FootprintVerification.Rungs[0].ChargedDependencyFacts++ }, "charges"},
		{"totals drift", func(s *Sample) { s.FootprintVerification.AcceptedRungs++ }, "totals"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sample := validFootprintSample(t, "bounded")
			test.mutate(&sample)
			if err := ValidateFootprintEvidence(sample); err == nil || !strings.Contains(err.Error(), test.errorContains) {
				t.Fatalf("mutated footprint evidence (%s) = %v", test.name, err)
			}
		})
	}
}
