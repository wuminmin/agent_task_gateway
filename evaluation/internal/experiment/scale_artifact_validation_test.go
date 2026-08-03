package experiment

import (
	"strings"
	"testing"
)

func TestFrozenScaleParsersRejectRenamedOrApproximateCells(t *testing.T) {
	for _, value := range []string{
		"10k-overlap-0", "10k-overlap-50", "10k-overlap-90", "10k-overlap-100",
		"100k-overlap-0", "100k-overlap-50", "100k-overlap-90", "100k-overlap-100",
		"1035000-overlap-0", "1035000-overlap-50", "1035000-overlap-90", "1035000-overlap-100",
	} {
		if _, err := ParseDependencyScale(value); err != nil {
			t.Fatalf("frozen dependency scale %q: %v", value, err)
		}
	}
	for _, root := range []string{"10k", "100k", "1m"} {
		for _, candidate := range []string{"x1", "x100", "x10k"} {
			for _, overlap := range []string{"o0", "o50", "o90", "o100"} {
				value := root + "-" + candidate + "-" + overlap
				if _, err := ParseOutcomeMerkleScale(value); err != nil {
					t.Fatalf("frozen Outcome scale %q: %v", value, err)
				}
			}
		}
	}
	for _, value := range []string{"100x4", "10k-x4", "100k-x4", "100x16", "10k-x16", "100k-x16"} {
		if _, err := ParseArtifactScale(value); err != nil {
			t.Fatalf("frozen artifact scale %q: %v", value, err)
		}
	}
	for _, value := range []string{"10m", "100m"} {
		if _, err := ParseExtremeScale(value); err != nil {
			t.Fatalf("frozen extreme scale %q: %v", value, err)
		}
	}
	for _, value := range []string{"1m-overlap-50", "10k-overlap-49", "10K-overlap-50", "10k-x10-o50", "1m-x1-o99", "1000x4", "10k-x8", "1m"} {
		if _, err := ParseDependencyScale(value); err == nil {
			t.Fatalf("non-frozen dependency scale %q was accepted", value)
		}
		if _, err := ParseOutcomeMerkleScale(value); err == nil {
			t.Fatalf("non-frozen Outcome scale %q was accepted", value)
		}
		if _, err := ParseArtifactScale(value); err == nil {
			t.Fatalf("non-frozen artifact scale %q was accepted", value)
		}
	}
}

func TestOutcomeX1OverlapUsesEvidenceVisibleNearestIntegerRule(t *testing.T) {
	for _, value := range []string{"10k-x1-o50", "10k-x1-o90", "100k-x1-o50", "1m-x1-o90"} {
		spec, err := ParseOutcomeMerkleScale(value)
		if err != nil {
			t.Fatal(err)
		}
		if spec.CandidateFacts != 1 || spec.OverlapFacts != 1 {
			t.Fatalf("%s resolved overlap = %d/%d, want nearest-integer 1/1", value, spec.OverlapFacts, spec.CandidateFacts)
		}
	}
	zero, _ := ParseOutcomeMerkleScale("10k-x1-o0")
	if zero.OverlapFacts != 0 {
		t.Fatalf("x1-o0 overlap = %d", zero.OverlapFacts)
	}
}

func TestScaleMicrobenchmarksNeverClaimFreshTaskRoots(t *testing.T) {
	for _, mode := range []string{"merkle_control", "kernel_storage_only"} {
		if freshRootAnchor(mode) {
			t.Fatalf("%s was incorrectly classified as a real Task-root anchor", mode)
		}
	}
}

func TestOutcomeMerkleValidatorCrossChecksProductionOracleReplayAndCounters(t *testing.T) {
	digest := func(character string) string { return strings.Repeat(character, 64) }
	spec, err := ParseOutcomeMerkleScale("10k-x100-o50")
	if err != nil {
		t.Fatal(err)
	}
	oracle, err := reconstructOutcomeMerkleOracle(20260801, "10k-x100-o50", spec)
	if err != nil {
		t.Fatal(err)
	}
	sample := Sample{
		ExperimentID: "scale", WorkloadID: "outcome-merkle", Scale: "10k-x100-o50", Mode: "merkle_control",
		RandomSeed: 20260801,
		System:     "taskgate", ResultSHA256: digest("a"), Counters: map[string]int64{
			"blocks_loaded": 2, "leaves_loaded": 3, "hashes_loaded": 51, "blocks_reused": 254,
			"leaves_changed": 2, "novelty": 50, "storage_bytes": 3000,
			"heap_alloc_bytes_after": 4000, "replay_changed_objects": 0,
		}, DiagnosticMS: map[string]float64{"outcome_radix_load": 1, "outcome_radix_difference_union": 2, "outcome_radix_persist": 3},
	}
	sample.ScaleVerification = &ScaleVerificationEvidence{Version: scaleEvidenceVersion, Boundary: "outcome_merkle_control",
		OutcomeMerkle: &OutcomeMerkleEvidence{
			ProductionPath: outcomeProductionPath, ContentCachePolicy: "warm_immutable_content_after_fixture_prefill",
			OverlapRounding: "nearest_integer_half_up", FixtureSHA256: oracle.fixtureSHA256, BackendRunSHA256: digest("2"),
			RootCardinality: 10_000, CandidateCardinality: 100, OverlapCardinality: 50,
			NovelCardinality: 50, UnionCardinality: 10_050, RootMemberOracleSHA256: oracle.rootSHA256,
			CandidateMemberOracleSHA256: oracle.candidateSHA256, UnionMemberOracleSHA256: oracle.unionSHA256,
			ObservedUnionMemberSHA256: oracle.unionSHA256,
			ProductionRootSHA256:      digest("6"), ProductionUnionSHA256: digest("a"), ReplayUnionSHA256: digest("a"),
			BlocksLoaded: 2, LeavesLoaded: 3, HashesLoaded: 51, BlocksReused: 254, LeavesChanged: 2,
			ChangedObjects: 4, ReplayChangedObjects: 0, StorageObjectsBefore: 100, StorageObjectsAfter: 104,
			StorageBytesBefore: 2000, StorageBytesAfter: 3000, HeapAllocBytesAfter: 4000,
			LoadMS: 1, DifferenceUnionMS: 2, PersistMS: 3,
		}}
	if err := validateScaleVerification(sample); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*Sample, *OutcomeMerkleEvidence)
	}{
		{name: "candidate label", mutate: func(_ *Sample, value *OutcomeMerkleEvidence) { value.CandidateCardinality++ }},
		{name: "replay object", mutate: func(_ *Sample, value *OutcomeMerkleEvidence) { value.ReplayChangedObjects = 1 }},
		{name: "replay digest", mutate: func(_ *Sample, value *OutcomeMerkleEvidence) { value.ReplayUnionSHA256 = digest("f") }},
		{name: "persisted member digest", mutate: func(_ *Sample, value *OutcomeMerkleEvidence) {
			value.ObservedUnionMemberSHA256 = digest("f")
		}},
		{name: "paired oracle tamper", mutate: func(_ *Sample, value *OutcomeMerkleEvidence) {
			value.UnionMemberOracleSHA256 = digest("f")
			value.ObservedUnionMemberSHA256 = digest("f")
		}},
		{name: "counter", mutate: func(value *Sample, _ *OutcomeMerkleEvidence) { value.Counters["novelty"]++ }},
		{name: "fake Task root", mutate: func(value *Sample, _ *OutcomeMerkleEvidence) { value.RootTaskIDHash = digest("e") }},
		{name: "cold relabel", mutate: func(_ *Sample, value *OutcomeMerkleEvidence) { value.ContentCachePolicy = "cold" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := sample
			mutated.Counters = map[string]int64{}
			for key, value := range sample.Counters {
				mutated.Counters[key] = value
			}
			evidence := *sample.ScaleVerification
			merkle := *sample.ScaleVerification.OutcomeMerkle
			evidence.OutcomeMerkle = &merkle
			mutated.ScaleVerification = &evidence
			test.mutate(&mutated, &merkle)
			if err := validateScaleVerification(mutated); err == nil {
				t.Fatal("tampered Outcome-Merkle evidence was accepted")
			}
		})
	}
}

func TestKernelStorageValidatorRejectsDigestAndRootTaskRelabeling(t *testing.T) {
	digest := strings.Repeat("a", 64)
	sample := Sample{ExperimentID: "scale", WorkloadID: "taskgate_scale_extreme", Scale: "10m",
		Mode: "kernel_storage_only", System: "taskgate", KernelOnly: true, ResultSHA256: digest,
		Counters: map[string]int64{"candidate_facts": 10_000_000, "difference_facts": 10_000_000,
			"union_facts": 10_000_000, "segments": 4, "containers": 160, "storage_bytes": 1000,
			"alloc_bytes": 2000, "alloc_objects": 10, "heap_alloc_bytes_after": 3000}}
	sample.ScaleVerification = &ScaleVerificationEvidence{Version: scaleEvidenceVersion, Boundary: "kernel_storage_only",
		KernelStorage: &KernelStorageEvidence{ProductionPath: kernelProductionPath, FixtureSHA256: strings.Repeat("1", 64),
			RunIdentitySHA256: strings.Repeat("2", 64), ExpectedCardinality: 10_000_000,
			CandidateCardinality: 10_000_000, DifferenceCardinality: 10_000_000, UnionCardinality: 10_000_000,
			CandidateSHA256: digest, DifferenceSHA256: digest, UnionSHA256: digest, RoundTripSHA256: digest,
			SegmentCount: 4, ContainerCount: 160, StorageBytes: 1000, AllocatedBytes: 2000, Allocations: 10,
			HeapAllocBytesAfter: 3000, DifferenceMS: 1, UnionMS: 1, CardinalityMS: 1, StorageRoundTripMS: 1}}
	if err := validateScaleVerification(sample); err != nil {
		t.Fatal(err)
	}
	mutated := sample
	mutated.RootTaskIDHash = strings.Repeat("3", 64)
	if err := validateScaleVerification(mutated); err == nil {
		t.Fatal("kernel microbenchmark was accepted with a fabricated Task root")
	}
	mutated = sample
	evidence := *sample.ScaleVerification
	kernel := *evidence.KernelStorage
	kernel.RoundTripSHA256 = strings.Repeat("f", 64)
	evidence.KernelStorage = &kernel
	mutated.ScaleVerification = &evidence
	if err := validateScaleVerification(mutated); err == nil {
		t.Fatal("kernel storage digest tamper was accepted")
	}
}

func TestArtifactAndDependencyValidatorsFailClosedBeforeRawVerifierEvidence(t *testing.T) {
	digest := strings.Repeat("a", 64)
	artifact := Sample{ExperimentID: "artifact", WorkloadID: "result-heavy", Scale: "100x4", Mode: "novel",
		System: "taskgate", RowCount: 99, ColumnCount: 4, ResultSHA256: digest,
		ArtifactVerification: &ArtifactVerificationEvidence{Version: artifactEvidenceVersion,
			BindingSHA256: digest, DatasetSHA256: digest, CatalogSHA256: digest, DatasetProbeSHA256: digest,
			QuerySHA256: digest, ExpectedRows: 100, ExpectedColumns: 4, ExpectedResultSHA256: digest,
			ObservedRows: 99, ObservedColumns: 4, ObservedResultSHA256: digest}}
	if err := validateArtifactVerification(artifact); err == nil {
		t.Fatal("mislabeled artifact row count was accepted")
	}
	dependency := Sample{ExperimentID: "scale", WorkloadID: "dependency-e2e", Scale: "10k-overlap-50",
		Mode: "novel", System: "taskgate", ActualDependencyFacts: 9_999, ResultSHA256: digest,
		ScaleVerification: &ScaleVerificationEvidence{Version: scaleEvidenceVersion, Boundary: "dependency_e2e",
			BindingSHA256: digest, DatasetSHA256: digest, CatalogSHA256: digest, DatasetProbeSHA256: digest,
			QuerySHA256: digest, ExpectedResultSHA256: digest, CandidateDependencySHA256: digest,
			ExpectedCandidateFacts: 10_000, ObservedCandidateFacts: 9_999, ExpectedOverlapFacts: 5_000,
			ObservedOverlapFacts: 5_000}}
	if err := validateScaleVerification(dependency); err == nil {
		t.Fatal("mislabeled dependency cardinality was accepted")
	}
}

func TestObserverTotalBusinessSQLMustMatchTargetedCounters(t *testing.T) {
	before := ObserverSnapshot{SchemaVersion: 1, MemoryScope: observerMemoryScope, Phase: "before",
		RuntimeIdentitySHA256:  strings.Repeat("9", 64),
		GatewayMemoryPeakBytes: 100, GatewayCPUUsec: 10, GatewayNetworkRXBytes: 20,
		GatewayNetworkTXBytes: 30, BusinessSQLQueries: 40, ControlWALBytes: 50, BusinessWALBytes: 60}
	after := ObserverSnapshot{SchemaVersion: 1, MemoryScope: observerMemoryScope, Phase: "after",
		RuntimeIdentitySHA256:  strings.Repeat("9", 64),
		GatewayMemoryPeakBytes: 200, GatewayCPUUsec: 11, GatewayNetworkRXBytes: 22,
		GatewayNetworkTXBytes: 33, BusinessSQLQueries: 42, ControlWALBytes: 54, BusinessWALBytes: 65}
	sample := Sample{GatewayMemoryPeakBytes: 200, GatewayCPUUsecDelta: 1, GatewayNetworkRXDelta: 2,
		GatewayNetworkTXDelta: 3, ControlWALBytesDelta: 4, BusinessWALBytesDelta: 5, BusinessSQLDelta: 2}
	if err := validateObserverTransition(sample, &before, &after); err != nil {
		t.Fatal(err)
	}
	after.BusinessSQLQueries++
	if err := validateObserverTransition(sample, &before, &after); err == nil {
		t.Fatal("observer total SQL mutation was accepted despite unchanged targeted counters")
	}
}
