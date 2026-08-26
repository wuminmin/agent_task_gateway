//go:build taskgate_scale

// These cases stream a 1,035,000-row dependency set to reproduce every frozen
// role digest. That is costly scale work, which the repository keeps behind
// taskgate_scale rather than in the acceptance run.

package experiment

import (
	"fmt"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
)

func TestScaleDependencyFactStreamsReproduceEveryFrozenRoleDigest(t *testing.T) {
	for _, cell := range finalv5oracle.ExposureScaleDependencyCells() {
		t.Run(cell.Scale, func(t *testing.T) {
			report, err := finalv5oracle.GenerateExposureScaleDependency(
				finalv5oracle.ExposureScaleDependencyRequest{CandidateFacts: cell.CandidateFacts,
					ExistingFacts: cell.CandidateFacts, OverlapFacts: cell.OverlapFacts})
			if err != nil {
				t.Fatal(err)
			}
			for role, expected := range map[DependencyScaleSummaryRole]finalv5oracle.StreamSetSummary{
				DependencyScaleCandidateSummaryRole: report.Candidate,
				DependencyScaleExistingSummaryRole:  report.Existing,
				DependencyScaleUnionSummaryRole:     report.Union,
			} {
				stream, err := scaleDependencyFactStream(cell.Scale, role)
				if err != nil {
					t.Fatal(err)
				}
				observed, err := finalv5oracle.SummarizeSemanticSet(string(role), func(yield func(string) error) error {
					return stream(func(fact finalv5oracle.CanonicalFact) error { return yield(fact.SHA256) })
				}, finalv5oracle.StreamSetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if observed.Cardinality != expected.Cardinality || observed.SetSHA256 != expected.SetSHA256 {
					t.Fatalf("%s stream = %d/%s, oracle = %d/%s", role,
						observed.Cardinality, observed.SetSHA256, expected.Cardinality, expected.SetSHA256)
				}
			}
		})
	}
}

func TestScaleDependencySetVerificationKeepsSemanticAndNativeDomainsSeparate(t *testing.T) {
	digest := func(value int) string { return fmt.Sprintf("%064x", value) }
	verification := ScaleDependencySetVerificationV1{
		Version: ScaleDependencySetVerificationV1Version,
		Role:    DependencyScaleCandidateSummaryRole, Match: true,
		ExpectedCardinality: 10_000, ExpectedSemanticSetSHA256: digest(1),
		ObservedCardinality: 10_000, ObservedSemanticSetSHA256: digest(1),
		ProductionSetSHA256: digest(2), ProductionDictionarySHA256: digest(3),
		ObservedOrdinalSetSHA256: digest(4),
	}
	if err := verification.Validate(); err != nil {
		t.Fatalf("separate semantic/native identities were rejected: %v", err)
	}
	for name, mutate := range map[string]func(*ScaleDependencySetVerificationV1){
		"semantic mismatch": func(value *ScaleDependencySetVerificationV1) {
			value.ObservedSemanticSetSHA256 = digest(5)
		},
		"missing ordinal": func(value *ScaleDependencySetVerificationV1) {
			value.ExpectedOrdinalsMissing = 1
		},
		"unexpected ordinal": func(value *ScaleDependencySetVerificationV1) {
			value.UnexpectedActualOrdinals = 1
		},
		"native digest copied into semantic": func(value *ScaleDependencySetVerificationV1) {
			value.ObservedSemanticSetSHA256 = value.ProductionSetSHA256
		},
	} {
		t.Run(name, func(t *testing.T) {
			mutated := verification
			mutate(&mutated)
			if err := mutated.Validate(); err == nil {
				t.Fatal("mutated Scale dependency link was accepted")
			}
		})
	}
}
