package experiment

import (
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/queryreceipt"
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

func TestArtifactPublicationDoesNotConsumePrivateBindingSection(t *testing.T) {
	privateSection := strings.Repeat("a", 64)
	publicBridgeBinding := strings.Repeat("b", 64)
	artifact := Sample{
		ExperimentID: "artifact",
		ArtifactVerification: &ArtifactVerificationEvidence{
			BindingSHA256: publicBridgeBinding,
		},
	}
	if got := sampleAdapterBindingSHA256(artifact); got != "" {
		t.Fatalf("Artifact public Bridge binding was treated as private section identity: %s", got)
	}
	if reasons := validatePublicationSampleBindingDigests("artifact",
		map[string]string{"deployment-01": privateSection},
		map[string]map[string]bool{"deployment-01": {publicBridgeBinding: true}}); len(reasons) != 0 {
		t.Fatalf("Artifact publication consumed the deleted private section input: %v", reasons)
	}

	for _, experimentID := range []string{"scale", "provsql"} {
		if reasons := validatePublicationSampleBindingDigests(experimentID,
			map[string]string{"deployment-01": privateSection},
			map[string]map[string]bool{"deployment-01": {publicBridgeBinding: true}}); len(reasons) != 1 {
			t.Fatalf("%s no longer enforces its private section identity: %v", experimentID, reasons)
		}
	}
}

func TestArtifactAcceptanceClosesPublicDeploymentBinding(t *testing.T) {
	sample, scaleEvidence := dependencyScaleV3ObservationFixture(t, "novel")
	operationID := "artifact/result-heavy/100x4/novel"
	bindingSHA256 := strings.Repeat("b", 64)
	datasetSHA256 := strings.Repeat("c", 64)
	probeSHA256 := strings.Repeat("d", 64)
	catalogSHA256 := strings.Repeat("e", 64)
	sample.ExperimentID = "artifact"
	sample.WorkloadID = "result-heavy"
	sample.Scale = "100x4"
	sample.SchemaVersion = FinalizedSampleSchemaVersion
	sample.TaskGateAcceptanceV3.Operation.OperationID = operationID
	sample.TaskGateAcceptanceV3.Operation.ContractIdentity = "final-v5-contracts-v1.4:" +
		strings.Repeat("9", 64) + ":binding=" + bindingSHA256 + ":dataset=" + datasetSHA256 +
		":probe=" + probeSHA256 + ":catalog=" + catalogSHA256 + ":" + operationID
	resealAcceptedClassifierForTest(t, sample.TaskGateAcceptanceV3)
	evidence := &ArtifactVerificationEvidence{
		BindingSHA256: bindingSHA256, DatasetSHA256: datasetSHA256,
		DatasetProbeSHA256: probeSHA256, CatalogSHA256: catalogSHA256,
		ObserverWindow: *scaleEvidence.ObserverWindow,
	}
	if err := validateArtifactObservationV3(sample, evidence); err != nil {
		t.Fatalf("honest public Artifact binding was rejected: %v", err)
	}

	for name, mutate := range map[string]func(*ArtifactVerificationEvidence){
		"binding": func(value *ArtifactVerificationEvidence) { value.BindingSHA256 = strings.Repeat("1", 64) },
		"Dataset": func(value *ArtifactVerificationEvidence) { value.DatasetSHA256 = strings.Repeat("2", 64) },
		"probe":   func(value *ArtifactVerificationEvidence) { value.DatasetProbeSHA256 = strings.Repeat("3", 64) },
		"Catalog": func(value *ArtifactVerificationEvidence) { value.CatalogSHA256 = strings.Repeat("4", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			mutatedEvidence := *evidence
			mutate(&mutatedEvidence)
			if err := validateArtifactObservationV3(sample, &mutatedEvidence); err == nil {
				t.Fatalf("Artifact evidence substituted another public %s identity", name)
			}
		})
	}

	mutatedSample := sample
	accepted := *sample.TaskGateAcceptanceV3
	mutatedSample.TaskGateAcceptanceV3 = &accepted
	accepted.Operation.ContractIdentity = strings.Replace(accepted.Operation.ContractIdentity,
		":binding="+bindingSHA256+":", ":binding="+strings.Repeat("5", 64)+":", 1)
	resealAcceptedClassifierForTest(t, &accepted)
	if err := validateArtifactObservationV3(mutatedSample, evidence); err == nil {
		t.Fatal("Artifact acceptance substituted another public deployment binding")
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
		SchemaVersion: SampleSchemaVersion, ExperimentID: "scale", WorkloadID: "outcome-merkle", Scale: "10k-x100-o50", Mode: "merkle_control",
		RandomSeed: 20260801,
		System:     "taskgate", ResultSHA256: digest("a"), Counters: map[string]int64{
			"blocks_loaded": 2, "leaves_loaded": 3, "hashes_loaded": 51, "blocks_reused": oracle.blocksReused,
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
			BlocksLoaded: 2, LeavesLoaded: 3, HashesLoaded: 51, BlocksReused: oracle.blocksReused, LeavesChanged: 2,
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
		{name: "forged zero reused blocks", mutate: func(value *Sample, evidence *OutcomeMerkleEvidence) {
			value.Counters["blocks_reused"] = 0
			evidence.BlocksReused = 0
		}},
		{name: "fake Task root", mutate: func(value *Sample, _ *OutcomeMerkleEvidence) { value.RootTaskIDHash = digest("e") }},
		{name: "fake governed acceptance", mutate: func(value *Sample, _ *OutcomeMerkleEvidence) {
			value.TaskGateAcceptanceV3 = &FinalizationV3{}
			value.ScaleVerification.ObserverWindow = &ObserverWindowV2{}
		}},
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

func TestOutcomeMerkleReusedBlocksFollowProductionGeometryForEveryFrozenCell(t *testing.T) {
	wantByCandidateAndOverlap := map[string]int64{
		"x1-o0": 255, "x1-o50": 256, "x1-o90": 256, "x1-o100": 256,
		"x100-o0": 178, "x100-o50": 213, "x100-o90": 247, "x100-o100": 256,
		"x10k-o0": 0, "x10k-o50": 0, "x10k-o90": 5, "x10k-o100": 256,
	}
	for _, root := range []string{"10k", "100k", "1m"} {
		for candidateAndOverlap, want := range wantByCandidateAndOverlap {
			scale := root + "-" + candidateAndOverlap
			t.Run(scale, func(t *testing.T) {
				spec, err := ParseOutcomeMerkleScale(scale)
				if err != nil {
					t.Fatal(err)
				}
				oracle, err := reconstructOutcomeMerkleOracle(20260801, scale, spec)
				if err != nil {
					t.Fatal(err)
				}
				if oracle.blocksReused != want {
					t.Fatalf("production-derived reused blocks = %d, want %d", oracle.blocksReused, want)
				}
			})
		}
	}
}

func TestKernelStorageValidatorRejectsDigestAndRootTaskRelabeling(t *testing.T) {
	digest := strings.Repeat("a", 64)
	sample := Sample{SchemaVersion: SampleSchemaVersion, ExperimentID: "scale", WorkloadID: "taskgate_scale_extreme", Scale: "10m",
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
	mutated = sample
	evidence = *sample.ScaleVerification
	evidence.ObserverWindow = &ObserverWindowV2{}
	mutated.ScaleVerification = &evidence
	mutated.TaskGateAcceptanceV3 = &FinalizationV3{}
	if err := validateScaleVerification(mutated); err == nil {
		t.Fatal("kernel control evidence was accepted as a governed v3 operation")
	}
}

func TestArtifactAndDependencyValidatorsFailClosedBeforeRawVerifierEvidence(t *testing.T) {
	digest := strings.Repeat("a", 64)
	artifact := Sample{ExperimentID: "artifact", WorkloadID: "result-heavy", Scale: "100x4", Mode: "novel",
		System: "taskgate", RowCount: 99, ColumnCount: 4, ResultSHA256: digest,
		ArtifactVerification: &ArtifactVerificationEvidence{Version: artifactEvidenceVersionV1,
			BindingSHA256: digest, DatasetSHA256: digest, CatalogSHA256: digest, DatasetProbeSHA256: digest,
			QuerySHA256: digest, ExpectedRows: 100, ExpectedColumns: 4, ExpectedResultSHA256: digest,
			ObservedRows: 99, ObservedColumns: 4, ObservedResultSHA256: digest}}
	if err := validateArtifactVerification(artifact); err == nil {
		t.Fatal("mislabeled artifact row count was accepted")
	}
	dependency := Sample{ExperimentID: "scale", WorkloadID: "dependency-e2e", Scale: "10k-overlap-50",
		Mode: "novel", System: "taskgate", ActualDependencyFacts: 9_999, ResultSHA256: digest,
		ScaleVerification: &ScaleVerificationEvidence{Version: scaleEvidenceVersion, Boundary: "dependency_e2e",
			BindingFileSHA256: digest, BindingSHA256: digest,
			DatasetSHA256: digest, CatalogSHA256: digest, DatasetProbeSHA256: digest,
			QuerySHA256: digest, ExpectedResultSHA256: digest, CandidateDependencySHA256: digest,
			ExpectedCandidateFacts: 10_000, ObservedCandidateFacts: 9_999, ExpectedOverlapFacts: 5_000,
			ObservedOverlapFacts: 5_000}}
	if err := validateScaleVerification(dependency); err == nil {
		t.Fatal("mislabeled dependency cardinality was accepted")
	}
}

func TestDependencyScaleReceiptCatalogUsesActivatedProfileForCurrentEvidence(t *testing.T) {
	masterCatalog := strings.Repeat("a", 64)
	profile := testBinding()
	sample := Sample{
		ProfileBinding:       &profile,
		BaselineVerification: &BaselineVerificationEvidence{Receipt: queryreceipt.QueryReceiptV1{CatalogDigest: profile.CatalogSHA256}},
	}
	evidence := &ScaleVerificationEvidence{
		Version:       scaleDependencyEvidenceVersionV5,
		CatalogSHA256: masterCatalog,
	}
	if err := validateDependencyScaleReceiptCatalog(sample, evidence); err != nil {
		t.Fatalf("profile Catalog matching Receipt was rejected because master Catalog differs: %v", err)
	}

	wrong := sample
	verification := *sample.BaselineVerification
	wrong.BaselineVerification = &verification
	wrong.BaselineVerification.Receipt.CatalogDigest = strings.Repeat("f", 64)
	if err := validateDependencyScaleReceiptCatalog(wrong, evidence); err == nil {
		t.Fatal("Receipt signed by a different Catalog was accepted for the activated profile")
	}

	withoutProfile := sample
	withoutProfile.ProfileBinding = nil
	if err := validateDependencyScaleReceiptCatalog(withoutProfile, evidence); err == nil {
		t.Fatal("current Scale evidence without an activated ProfileBinding was accepted")
	}
}

func TestDependencyScaleHistoricalReceiptCatalogRuleIsNotReinterpreted(t *testing.T) {
	masterCatalog := strings.Repeat("a", 64)
	profile := testBinding()
	sample := Sample{
		ProfileBinding:       &profile,
		BaselineVerification: &BaselineVerificationEvidence{Receipt: queryreceipt.QueryReceiptV1{CatalogDigest: masterCatalog}},
	}
	evidence := &ScaleVerificationEvidence{
		Version:       scaleDependencyEvidenceVersionV4,
		CatalogSHA256: masterCatalog,
	}
	if err := validateDependencyScaleReceiptCatalog(sample, evidence); err != nil {
		t.Fatalf("historical Scale Catalog equality changed meaning: %v", err)
	}
	sample.BaselineVerification.Receipt.CatalogDigest = profile.CatalogSHA256
	if err := validateDependencyScaleReceiptCatalog(sample, evidence); err == nil {
		t.Fatal("historical Scale evidence silently acquired the profile-Catalog rule")
	}
}

func TestArtifactDatasetIdentityAndProbeHaveVersionedSemantics(t *testing.T) {
	dataset := strings.Repeat("a", 64)
	probe := strings.Repeat("b", 64)

	legacy := Sample{SchemaVersion: SampleSchemaVersion}
	legacyEvidence := &ArtifactVerificationEvidence{
		Version: artifactEvidenceVersionV1, DatasetSHA256: dataset, DatasetProbeSHA256: dataset,
	}
	if err := validateArtifactDatasetIdentity(legacy, legacyEvidence); err != nil {
		t.Fatalf("sample-v1 rejected its frozen evidence-v1 equality rule: %v", err)
	}
	legacyEvidence.DatasetProbeSHA256 = probe
	if err := validateArtifactDatasetIdentity(legacy, legacyEvidence); err == nil {
		t.Fatal("sample-v1 silently acquired the sample-v3 independent-probe meaning")
	}

	current := Sample{SchemaVersion: FinalizedSampleSchemaVersion}
	currentEvidence := &ArtifactVerificationEvidence{
		Version: artifactEvidenceVersionV2, DatasetSHA256: dataset, DatasetProbeSHA256: probe,
	}
	if err := validateArtifactDatasetIdentity(current, currentEvidence); err != nil {
		t.Fatalf("sample-v3 rejected independent typed Dataset and probe identities: %v", err)
	}
	currentEvidence.DatasetProbeSHA256 = dataset
	if err := validateArtifactDatasetIdentity(current, currentEvidence); err != nil {
		t.Fatalf("sample-v3 imposed an equality or inequality relation between independent domains: %v", err)
	}

	for _, test := range []struct {
		name     string
		sample   Sample
		evidence *ArtifactVerificationEvidence
	}{
		{"v1 with evidence-v2", legacy, currentEvidence},
		{"v3 with evidence-v1", current, legacyEvidence},
		{"v3 malformed Dataset", current, &ArtifactVerificationEvidence{
			Version: artifactEvidenceVersionV2, DatasetSHA256: "not-a-digest", DatasetProbeSHA256: probe}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateArtifactDatasetIdentity(test.sample, test.evidence); err == nil {
				t.Fatal("cross-version or malformed Artifact Dataset evidence was accepted")
			}
		})
	}
}

func TestDependencyScaleV3ObservationValidation(t *testing.T) {
	for _, mode := range []string{"novel", "semantic_replay"} {
		t.Run(mode, func(t *testing.T) {
			sample, evidence := dependencyScaleV3ObservationFixture(t, mode)
			if err := validateScaleObservationV3(sample, evidence); err != nil {
				t.Fatalf("honest %s observation was rejected: %v", mode, err)
			}
		})
	}

	mutations := []struct {
		name   string
		mutate func(*Sample, *ScaleVerificationEvidence)
	}{
		{"missing acceptance", func(sample *Sample, _ *ScaleVerificationEvidence) {
			sample.TaskGateAcceptanceV3 = nil
		}},
		{"accepted receipt substitution", func(sample *Sample, _ *ScaleVerificationEvidence) {
			sample.TaskGateAcceptanceV3.ReceiptSHA256 = strings.Repeat("6", 64)
		}},
		{"sample receipt substitution", func(sample *Sample, _ *ScaleVerificationEvidence) {
			sample.ReceiptSHA256 = strings.Repeat("6", 64)
		}},
		{"retained receipt substitution", func(sample *Sample, _ *ScaleVerificationEvidence) {
			sample.BaselineVerification.Receipt.RequestID = "another-retained-attempt"
		}},
		{"wrong path", func(sample *Sample, _ *ScaleVerificationEvidence) {
			sample.TaskGateAcceptanceV3.Operation.PathKind = PathSemanticReplay
		}},
		{"classifier substitution", func(sample *Sample, _ *ScaleVerificationEvidence) {
			sample.TaskGateAcceptanceV3.ClassifierManifestSHA256 = strings.Repeat("7", 64)
		}},
		{"classifier binding substitution", func(sample *Sample, _ *ScaleVerificationEvidence) {
			sample.TaskGateAcceptanceV3.ClassifierBindingSHA256 = strings.Repeat("6", 64)
		}},
		{"operation contract substitution", func(sample *Sample, _ *ScaleVerificationEvidence) {
			sample.TaskGateAcceptanceV3.Operation.ContractIdentity = "another-contract"
		}},
		{"operation coordinate substitution", func(sample *Sample, _ *ScaleVerificationEvidence) {
			sample.TaskGateAcceptanceV3.Operation.OperationID = "scale/dependency-e2e/100k-overlap-0/novel"
		}},
		{"split window identity", func(_ *Sample, evidence *ScaleVerificationEvidence) {
			evidence.ObserverWindow.After.ObserverWindowID = strings.Repeat("8", 64)
		}},
		{"different retained total", func(_ *Sample, evidence *ScaleVerificationEvidence) {
			evidence.ObserverWindow.After.Structural[0].Calls++
			evidence.ObserverWindow.After.Total++
		}},
		{"different retained structure", func(_ *Sample, evidence *ScaleVerificationEvidence) {
			evidence.ObserverWindow.After.Structural[0].StrictASTSHA256 = strings.Repeat("9", 64)
		}},
		{"different accepted attempt", func(_ *Sample, evidence *ScaleVerificationEvidence) {
			evidence.ObserverWindow.Before.ObserverWindowID = strings.Repeat("8", 64)
			evidence.ObserverWindow.After.ObserverWindowID = strings.Repeat("8", 64)
		}},
		{"reset inside retained interval", func(_ *Sample, evidence *ScaleVerificationEvidence) {
			evidence.ObserverWindow.After.StatsReset = "2026-08-04 10:42:00+00"
		}},
		{"resource relabel", func(sample *Sample, _ *ScaleVerificationEvidence) {
			sample.GatewayCPUUsecDelta++
		}},
		{"edited accepted delta", func(sample *Sample, _ *ScaleVerificationEvidence) {
			sample.TaskGateAcceptanceV3.Delta.Total++
		}},
		{"extra accepted delta class", func(sample *Sample, _ *ScaleVerificationEvidence) {
			sample.TaskGateAcceptanceV3.Delta.PerClass[GatewayStatementClassV3("invented")] = 0
		}},
		{"duplicate accepted internal expectation", func(sample *Sample, _ *ScaleVerificationEvidence) {
			entry := sample.TaskGateAcceptanceV3.InternalExpectation[0]
			sample.TaskGateAcceptanceV3.InternalExpectation = append(
				sample.TaskGateAcceptanceV3.InternalExpectation, entry)
		}},
		{"Outcome control mixed in", func(_ *Sample, evidence *ScaleVerificationEvidence) {
			evidence.OutcomeMerkle = &OutcomeMerkleEvidence{}
		}},
		{"kernel control mixed in", func(_ *Sample, evidence *ScaleVerificationEvidence) {
			evidence.KernelStorage = &KernelStorageEvidence{}
		}},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			sample, evidence := dependencyScaleV3ObservationFixture(t, "novel")
			test.mutate(&sample, evidence)
			if err := validateScaleObservationV3(sample, evidence); err == nil {
				t.Fatal("mutated Scale observation was accepted")
			}
		})
	}
}

func TestDependencyScaleAcceptanceRefusesAnotherAttemptsCoherentAcceptanceAndWindow(t *testing.T) {
	for _, direction := range []string{"a_accepts_b", "b_accepts_a"} {
		t.Run(direction, func(t *testing.T) {
			first, firstEvidence := dependencyScaleV3ObservationFixture(t, "novel")
			second, secondEvidence := dependencyScaleV3ObservationFixture(t, "novel")
			retargetDependencyScaleAttempt(t, &second, secondEvidence, "8")

			target, targetEvidence := &first, firstEvidence
			donor, donorEvidence := &second, secondEvidence
			if direction == "b_accepts_a" {
				target, targetEvidence, donor, donorEvidence = &second, secondEvidence, &first, firstEvidence
			}
			accepted := *donor.TaskGateAcceptanceV3
			window := *donorEvidence.ObserverWindow
			target.TaskGateAcceptanceV3 = &accepted
			targetEvidence.ObserverWindow = &window
			if err := validateScaleObservationV3(*target, targetEvidence); err == nil {
				t.Fatal("one attempt accepted another attempt's coherent acceptance/window pair")
			}
		})
	}
}

func TestDependencyScaleAcceptanceClosesPrivateContractIdentity(t *testing.T) {
	for name, mutate := range map[string]func(string) string{
		"binding prefix": func(identity string) string {
			return strings.Replace(identity, ":binding-file=", ":private-file=", 1)
		},
		"binding file": func(identity string) string {
			return strings.Replace(identity, strings.Repeat("7", 64), strings.Repeat("6", 64), 1)
		},
		"binding section": func(identity string) string {
			return strings.Replace(identity, strings.Repeat("8", 64), strings.Repeat("5", 64), 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			sample, evidence := dependencyScaleV3ObservationFixture(t, "novel")
			sample.TaskGateAcceptanceV3.Operation.ContractIdentity = mutate(
				sample.TaskGateAcceptanceV3.Operation.ContractIdentity)
			resealAcceptedClassifierForTest(t, sample.TaskGateAcceptanceV3)
			if err := validateScaleObservationV3(sample, evidence); err == nil {
				t.Fatal("a private contract identity mutation survived retained validation")
			}
		})
	}
}

func dependencyScaleV3ObservationFixture(t *testing.T, mode string) (Sample, *ScaleVerificationEvidence) {
	t.Helper()
	var plan GatewayControlPlanV3
	switch mode {
	case "novel":
		plan = mustPairedNovel(t, 1)
	case "semantic_replay":
		plan = mustSemanticReplay(t, 1)
	default:
		t.Fatalf("unsupported Scale mode %q", mode)
	}
	planSHA256, err := plan.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	row := ObserverStructuralRow{StrictASTSHA256: strings.Repeat("e", 64), TopLevel: true, Calls: plan.ExpectedTotal()}
	before := snapshotOf(t, "before", nil)
	after := snapshotOf(t, "after", []ObserverStructuralRow{row})
	before.Resource.GatewayMemoryPeakBytes = 100
	before.Resource.GatewayCPUUsec = 10
	before.Resource.GatewayNetworkRXBytes = 20
	before.Resource.GatewayNetworkTXBytes = 30
	before.Resource.ControlWALBytes = 40
	before.Resource.BusinessWALBytes = 50
	after.Resource.GatewayMemoryPeakBytes = 200
	after.Resource.GatewayCPUUsec = 11
	after.Resource.GatewayNetworkRXBytes = 22
	after.Resource.GatewayNetworkTXBytes = 33
	after.Resource.ControlWALBytes = 44
	after.Resource.BusinessWALBytes = 55
	window := ObserverWindowV2{Before: before, After: after}
	windowSHA256, err := window.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	operationID := "scale/dependency-e2e/10k-overlap-0/" + mode
	bindingFileSHA256 := strings.Repeat("7", 64)
	bindingSectionSHA256 := strings.Repeat("8", 64)
	operation := OperationIdentity{
		OperationID: operationID,
		PathKind:    plan.PathKind,
		ContractIdentity: "final-v5-contracts-v1.4:" + strings.Repeat("9", 64) +
			":binding-file=" + bindingFileSHA256 + ":binding-section=" + bindingSectionSHA256 + ":" + operationID,
		ExpectedSchemaDigest:       plan.ExpectedSchemaDigest,
		AttestationFootprintSHA256: plan.AttestationFootprintSHA256,
	}
	classifierBinding, err := classifierBindingSHA256(operation, planSHA256,
		before.ClassifierManifestSHA256)
	if err != nil {
		t.Fatal(err)
	}
	receipt := queryreceipt.QueryReceiptV1{Version: "fixture", RequestID: "scale-attempt-a"}
	receiptSHA256, err := queryreceipt.DocumentSHA256(receipt)
	if err != nil {
		t.Fatal(err)
	}
	accepted := FinalizationV3{
		Operation: operation,
		Plan:      plan, PlanSHA256: planSHA256,
		ReceiptSHA256:            receiptSHA256,
		ExpectedSchemaDigest:     plan.ExpectedSchemaDigest,
		ExpectedSchemaEntries:    plan.ExpectedSchemaEntries,
		ClassifierManifestSHA256: before.ClassifierManifestSHA256,
		ClassifierBindingSHA256:  classifierBinding,
		ObserverWindowID:         before.ObserverWindowID,
		ObserverWindowSHA256:     windowSHA256,
		InternalExpectation:      append([]InternalExpectation(nil), plan.InternalExpectation...),
		Delta: ObservedDelta{
			Total: plan.ExpectedTotal(), PerClass: plan.Expected(),
			Internal: append([]InternalExpectation(nil), plan.InternalExpectation...),
		},
	}
	sample := Sample{
		ExperimentID: "scale", WorkloadID: "dependency-e2e", Scale: "10k-overlap-0", Mode: mode,
		BusinessSQLDelta:       plan.ExpectedVisibleCalls + plan.ExpectedCompanionCall,
		GatewayMemoryPeakBytes: 200, GatewayCPUUsecDelta: 1,
		GatewayNetworkRXDelta: 2, GatewayNetworkTXDelta: 3,
		ControlWALBytesDelta: 4, BusinessWALBytesDelta: 5,
		ReceiptSHA256: receiptSHA256, BaselineVerification: &BaselineVerificationEvidence{Receipt: receipt},
		TaskGateAcceptanceV3: &accepted,
	}
	evidence := &ScaleVerificationEvidence{
		BindingFileSHA256: bindingFileSHA256, BindingSHA256: bindingSectionSHA256,
		ObserverWindow: &window,
	}
	return sample, evidence
}

func retargetDependencyScaleAttempt(t *testing.T, sample *Sample, evidence *ScaleVerificationEvidence,
	hexDigit string) {
	t.Helper()
	receipt := sample.BaselineVerification.Receipt
	receipt.RequestID = "scale-attempt-" + hexDigit
	receiptSHA256, err := queryreceipt.DocumentSHA256(receipt)
	if err != nil {
		t.Fatal(err)
	}
	sample.BaselineVerification.Receipt = receipt
	sample.ReceiptSHA256 = receiptSHA256
	sample.TaskGateAcceptanceV3.ReceiptSHA256 = receiptSHA256
	windowID := strings.Repeat(hexDigit, 64)
	evidence.ObserverWindow.Before.ObserverWindowID = windowID
	evidence.ObserverWindow.After.ObserverWindowID = windowID
	windowSHA256, err := evidence.ObserverWindow.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	sample.TaskGateAcceptanceV3.ObserverWindowID = windowID
	sample.TaskGateAcceptanceV3.ObserverWindowSHA256 = windowSHA256
}

func resealAcceptedClassifierForTest(t *testing.T, accepted *FinalizationV3) {
	t.Helper()
	binding, err := classifierBindingSHA256(accepted.Operation, accepted.PlanSHA256,
		accepted.ClassifierManifestSHA256)
	if err != nil {
		t.Fatal(err)
	}
	accepted.ClassifierBindingSHA256 = binding
}
