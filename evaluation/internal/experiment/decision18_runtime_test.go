package experiment

import (
	"strings"
	"testing"
)

func TestDependencyScaleDecision18RuntimeAccountingEveryCell(t *testing.T) {
	scales := []string{
		"10k-overlap-0", "10k-overlap-50", "10k-overlap-90", "10k-overlap-100",
		"100k-overlap-0", "100k-overlap-50", "100k-overlap-90", "100k-overlap-100",
		"1035000-overlap-0", "1035000-overlap-50", "1035000-overlap-90", "1035000-overlap-100",
	}
	if len(scales) != 12 {
		t.Fatalf("runtime accounting table has %d cells, want 12", len(scales))
	}
	for index, scale := range scales {
		t.Run(scale, func(t *testing.T) {
			spec, err := ParseDependencyScale(scale)
			if err != nil {
				t.Fatal(err)
			}
			existingDigest := strings.Repeat(string(rune('a'+index%6)), 64)
			candidateDigest := strings.Repeat(string(rune('1'+index%6)), 64)
			unionDigest := strings.Repeat(string(rune('7'+index%3)), 64)
			before := decision18RootSnapshot(1, spec.ExistingFacts, existingDigest)
			after := decision18RootSnapshot(2, spec.UnionFacts, unionDigest)
			sample := Sample{
				Mode: "novel", ActualDependencyFacts: spec.CandidateFacts,
				ChargedDependencyFacts: spec.CandidateFacts - spec.OverlapFacts,
				DependencySetSHA256:    candidateDigest,
				RootEpochBefore:        before.Epoch, RootEpochAfter: after.Epoch,
				RootSetSHA256Before: rootLedgerSetSHA256(before),
				RootSetSHA256After:  rootLedgerSetSHA256(after),
			}
			evidence := &ScaleVerificationEvidence{
				ExpectedCandidateFacts:    spec.CandidateFacts,
				ExpectedExistingFacts:     spec.ExistingFacts,
				ExpectedOverlapFacts:      spec.OverlapFacts,
				ExpectedUnionFacts:        spec.UnionFacts,
				ExistingDependencySHA256:  existingDigest,
				CandidateDependencySHA256: candidateDigest,
				UnionDependencySHA256:     unionDigest,
				RootBefore:                before, RootAfter: after,
			}
			if err := validateDependencyScaleAccountingV3(sample, evidence, spec); err != nil {
				t.Fatalf("Decision-18 runtime transition rejected: %v", err)
			}
			if sample.ChargedDependencyFacts != spec.CandidateFacts-spec.OverlapFacts {
				t.Fatalf("charged=%d, want N-5K=%d", sample.ChargedDependencyFacts,
					spec.CandidateFacts-spec.OverlapFacts)
			}
			if sample.ActualDependencyFacts-sample.ChargedDependencyFacts != spec.OverlapFacts {
				t.Fatalf("actual-charged=%d, want 5K=%d",
					sample.ActualDependencyFacts-sample.ChargedDependencyFacts, spec.OverlapFacts)
			}
			if evidence.RootBefore.DependencyCardinality != spec.ExistingFacts {
				t.Fatalf("RootBefore=%d, want ExistingFacts=%d",
					evidence.RootBefore.DependencyCardinality, spec.ExistingFacts)
			}
			if evidence.RootAfter.DependencyCardinality != spec.UnionFacts ||
				evidence.RootAfter.DependencySetSHA256 != evidence.UnionDependencySHA256 {
				t.Fatalf("RootAfter=%d/%s, want union=%d/%s", evidence.RootAfter.DependencyCardinality,
					evidence.RootAfter.DependencySetSHA256, spec.UnionFacts, evidence.UnionDependencySHA256)
			}

			replaySample := sample
			replaySample.Mode = "semantic_replay"
			replaySample.SemanticReplay = true
			replaySample.ChargedDependencyFacts = 0
			replaySample.RootEpochBefore = after.Epoch
			replaySample.RootSetSHA256Before = rootLedgerSetSHA256(after)
			replayEvidence := *evidence
			replayEvidence.RootBefore = after
			replayEvidence.RootAfter = after
			if err := validateDependencyScaleAccountingV3(replaySample, &replayEvidence, spec); err != nil {
				t.Fatalf("Decision-18 semantic replay did not preserve the union root: %v", err)
			}
		})
	}
}

func TestDependencyScaleDecision18RuntimeAccountingRejectsInvariantDrift(t *testing.T) {
	spec, err := ParseDependencyScale("10k-overlap-50")
	if err != nil {
		t.Fatal(err)
	}
	existingDigest := strings.Repeat("a", 64)
	candidateDigest := strings.Repeat("b", 64)
	unionDigest := strings.Repeat("c", 64)
	before := decision18RootSnapshot(1, spec.ExistingFacts, existingDigest)
	after := decision18RootSnapshot(2, spec.UnionFacts, unionDigest)
	validSample := Sample{
		Mode: "novel", ActualDependencyFacts: spec.CandidateFacts,
		ChargedDependencyFacts: spec.CandidateFacts - spec.OverlapFacts,
		DependencySetSHA256:    candidateDigest,
		RootEpochBefore:        before.Epoch, RootEpochAfter: after.Epoch,
		RootSetSHA256Before: rootLedgerSetSHA256(before), RootSetSHA256After: rootLedgerSetSHA256(after),
	}
	validEvidence := ScaleVerificationEvidence{
		ExpectedCandidateFacts: spec.CandidateFacts, ExpectedExistingFacts: spec.ExistingFacts,
		ExpectedOverlapFacts: spec.OverlapFacts, ExpectedUnionFacts: spec.UnionFacts,
		ExistingDependencySHA256: existingDigest, CandidateDependencySHA256: candidateDigest,
		UnionDependencySHA256: unionDigest, RootBefore: before, RootAfter: after,
	}
	tests := map[string]func(*Sample, *ScaleVerificationEvidence){
		"charged is not N-5K": func(sample *Sample, _ *ScaleVerificationEvidence) {
			sample.ChargedDependencyFacts++
		},
		"actual minus charged is not 5K": func(sample *Sample, _ *ScaleVerificationEvidence) {
			sample.ActualDependencyFacts++
		},
		"RootBefore is not ExistingFacts": func(_ *Sample, evidence *ScaleVerificationEvidence) {
			evidence.RootBefore.DependencyCardinality--
		},
		"RootAfter is not union": func(_ *Sample, evidence *ScaleVerificationEvidence) {
			evidence.RootAfter.DependencyCardinality--
		},
		"RootAfter does not use union summary": func(_ *Sample, evidence *ScaleVerificationEvidence) {
			evidence.RootAfter.DependencySetSHA256 = candidateDigest
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			sample := validSample
			evidence := validEvidence
			mutate(&sample, &evidence)
			if err := validateDependencyScaleAccountingV3(sample, &evidence, spec); err == nil {
				t.Fatal("drifted Decision-18 runtime transition was accepted")
			}
		})
	}
}

func decision18RootSnapshot(epoch, dependencyFacts int64, dependencyDigest string) RootLedgerSnapshot {
	digest := strings.Repeat("d", 64)
	return RootLedgerSnapshot{
		Epoch: epoch, DictionarySetSHA256: digest,
		ReleaseSetSHA256: digest, ReleaseCardinality: 1,
		DependencySetSHA256: dependencyDigest, DependencyCardinality: dependencyFacts,
		OutcomeSetSHA256: digest, OutcomeCardinality: 1,
		RootObservationSetSHA256: digest, RootObservationCount: epoch,
	}
}
