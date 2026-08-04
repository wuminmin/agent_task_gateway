package experiment

import (
	"strconv"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/provsqlfixture"
)

func validProvSQLValidationSample(t *testing.T, mode string) Sample {
	t.Helper()
	positions := map[string]int{"direct": 1, "provsql": 2, "taskgate": 3}
	systems := map[string]string{"direct": "postgresql", "provsql": "provsql", "taskgate": "taskgate"}
	nonce, err := provsqlfixture.Nonce("1k", 1, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	nonceBinding, _ := provsqlfixture.NonceBindingSHA256("1k", 1, 1, false)
	logical, _ := provsqlfixture.LogicalSQL("1k", nonce)
	rows, _ := provsqlfixture.ExpectedResultRows("1k")
	resultSHA, err := CanonicalResultHash(rows)
	if err != nil {
		t.Fatal(err)
	}
	physical := provsqlfixture.PhysicalSQLSHA256()
	if mode == "taskgate" {
		physical = sha256Hex([]byte(logical))
	}
	sample := Sample{
		SchemaVersion: 1, CampaignID: "provsql-campaign", DeploymentID: "deployment-01",
		ExperimentID: "provsql", CellID: "nonce-join-group/1k/" + mode,
		SampleID: "sample-" + mode, Iteration: 1, ProcessReplicate: 1,
		OrderPosition: positions[mode], RandomSeed: 20260801, PairID: "pair-1",
		PairedSystemOrder: "direct,provsql,taskgate", RootGroupID: "root-1",
		System: systems[mode], Mode: mode, WorkloadID: "nonce-join-group", Scale: "1k",
		ClientAvailableMS: 1, ClientFullDrainMS: 3, PipelineMS: map[string]float64{"execute_and_derive": 3, "server_total": 3},
		RowCount: provsqlfixture.ExpectedRows, ColumnCount: provsqlfixture.ExpectedColumns,
		ResultSHA256: resultSHA, PhysicalSQLSHA256: physical, LogicalSQLSHA256: sha256Hex([]byte(logical)),
		Status: "pass", PublicationEligible: true,
	}
	evidence := &ProvSQLVerificationEvidence{
		Version: "taskgate-final-v5-provsql-verification-v1", BindingSHA256: strings.Repeat("a", 64),
		FixtureVersion: provsqlfixture.Version, FixtureSQLSHA256: provsqlfixture.FixtureSQLSHA256(),
		EnableSQLSHA256: provsqlfixture.EnableSQLSHA256(), DatasetSHA256: provsqlfixture.ExpectedDatasetSHA256(),
		DatasetProbeSQLSHA256:         provsqlfixture.DatasetProbeSQLSHA256(),
		BusinessDatasetProbeSQLSHA256: provsqlfixture.BusinessDatasetProbeSQLSHA256(),
		DatasetRows:                   provsqlfixture.DatasetRowCount, ScaleLimit: 1000, Nonce: nonce,
		NonceBindingSHA256: nonceBinding, PhysicalSQLSHA256: physical, LogicalSQLSHA256: sha256Hex([]byte(logical)),
		CacheConditionSHA256: sha256Hex([]byte(provsqlfixture.Version + "\x00warm-after-complete-typed-dataset-fingerprint")),
		ExecutionOrderSHA256: sha256Hex([]byte(strings.Join([]string{provsqlfixture.Version, "execution-order-v2", sample.PairID,
			sample.PairedSystemOrder, strconv.Itoa(positions[mode]), mode, strconv.FormatInt(nonce, 10)}, "\x00"))),
		ExpectedRows: provsqlfixture.ExpectedRows, ExpectedColumns: provsqlfixture.ExpectedColumns,
		ExpectedResultSHA256: resultSHA, ObservedResultSHA256: resultSHA,
		ExpectedDependencyFacts: 1234, ExpectedDependencySHA256: strings.Repeat("b", 64),
		TypedDrainSHA256: strings.Repeat("c", 64),
	}
	switch mode {
	case "direct":
		evidence.Boundary = "direct_complete_typed_drain"
		evidence.TypedDrainFields = 12
		evidence.FieldOIDs = []uint32{25, 1700, 20, 20}
		setProvSQLExternalSession(evidence)
	case "provsql":
		evidence.Boundary = "provsql_complete_typed_drain"
		evidence.TypedDrainFields = 15
		setProvSQLExternalSession(evidence)
		evidence.ProvSQLVersion, evidence.ProvSQLCommit = provsqlfixture.ProvSQLVersion, provsqlfixture.ProvSQLCommit
		evidence.SharedPreload, evidence.AggTokenTextAsUUID = true, true
		evidence.AggTokenOID = 91000
		evidence.FieldOIDs = []uint32{25, 91000, 91000, 91000, 2950}
		evidence.CarrierGateType, evidence.RowGateType, evidence.RootTypesVerified = "agg", "delta", true
		evidence.AggregateTokens, evidence.RowTokens = 9, 3
		evidence.GatesBefore, evidence.GatesAfter = 10, 20
		evidence.ArtifactBytesBefore, evidence.ArtifactBytesAfter = 100, 200
		evidence.RepresentationSHA256 = strings.Repeat("d", 64)
	case "taskgate":
		evidence.Boundary = "taskgate_released_parquet_v8"
		evidence.TypedDrainFields, evidence.TypedDrainSHA256 = 12, resultSHA
		sample.ActualDependencyFacts, sample.DependencySetSHA256 = evidence.ExpectedDependencyFacts, evidence.ExpectedDependencySHA256
		sample.GenerationBoundaryMS, sample.FullTaskGateMS = 2, sample.ClientFullDrainMS
		businessBefore := BusinessSQLSnapshot{StatsResetUnixMicro: 100, Dealloc: 2, VisibleCalls: 10, CompanionCalls: 20}
		businessAfter := businessBefore
		businessAfter.VisibleCalls++
		businessAfter.CompanionCalls++
		rootBefore := RootLedgerSnapshot{RootObservationSetSHA256: emptyRootObservationSetSHA256()}
		rootAfter := RootLedgerSnapshot{Epoch: 1, DictionarySetSHA256: strings.Repeat("1", 64),
			ReleaseSetSHA256: strings.Repeat("2", 64), ReleaseCardinality: 0,
			DependencySetSHA256: evidence.ExpectedDependencySHA256, DependencyCardinality: evidence.ExpectedDependencyFacts,
			OutcomeSetSHA256: strings.Repeat("3", 64), OutcomeCardinality: 0,
			RootObservationSetSHA256: strings.Repeat("4", 64), RootObservationCount: 1}
		observerBefore := ObserverSnapshot{SchemaVersion: 1, MemoryScope: observerMemoryScope,
			Phase: "before", RuntimeIdentitySHA256: strings.Repeat("5", 64), GatewayMemoryPeakBytes: 100,
			GatewayCPUUsec: 10, GatewayNetworkRXBytes: 20, GatewayNetworkTXBytes: 30,
			BusinessSQLQueries: 40, ControlWALBytes: 50, BusinessWALBytes: 60}
		observerAfter := observerBefore
		observerAfter.Phase = "after"
		observerAfter.GatewayMemoryPeakBytes = 200
		observerAfter.GatewayCPUUsec++
		observerAfter.GatewayNetworkRXBytes += 2
		observerAfter.GatewayNetworkTXBytes += 3
		// Two targeted statements plus the fourteen controls a one-view profile
		// derives over two governed transactions.
		accounting := resultHeavyAccounting()
		observerAfter.BusinessSQLQueries += accounting.ObserverTotalDelta
		observerAfter.ControlWALBytes += 4
		observerAfter.BusinessWALBytes += 5
		evidence.BusinessBefore, evidence.BusinessAfter = &businessBefore, &businessAfter
		evidence.RootBefore, evidence.RootAfter = &rootBefore, &rootAfter
		evidence.ObserverBefore, evidence.ObserverAfter = &observerBefore, &observerAfter
		sample.BusinessSQLDelta = 2
		sample.ObserverAccounting = &accounting
		sample.RootEpochBefore, sample.RootEpochAfter = rootBefore.Epoch, rootAfter.Epoch
		sample.ReleaseSetSHA256, sample.OutcomeSetSHA256 = rootAfter.ReleaseSetSHA256, rootAfter.OutcomeSetSHA256
		sample.RootSetSHA256Before, sample.RootSetSHA256After = rootLedgerSetSHA256(rootBefore), rootLedgerSetSHA256(rootAfter)
		sample.GatewayMemoryPeakBytes = 200
		sample.GatewayCPUUsecDelta, sample.GatewayNetworkRXDelta, sample.GatewayNetworkTXDelta = 1, 2, 3
		sample.ControlWALBytesDelta, sample.BusinessWALBytesDelta = 4, 5
	}
	sample.ProvSQLVerification = evidence
	return sample
}

func setProvSQLExternalSession(evidence *ProvSQLVerificationEvidence) {
	evidence.PostgreSQLVersion = "16.14 (Debian)"
	evidence.PostgreSQLVersionNum = "160014"
	evidence.StatementTimeoutMS = provsqlfixture.StatementTimeout
	evidence.MaxParallelWorkers = 0
	evidence.ClientMinMessages, evidence.LogMinMessages = "error", "error"
	evidence.UUIDOID = 2950
}

func validProvSQLWarmupValidationSample(t *testing.T, mode string) Sample {
	t.Helper()
	sample := validProvSQLValidationSample(t, mode)
	nonce, err := provsqlfixture.Nonce(sample.Scale, sample.ProcessReplicate, sample.Iteration, true)
	if err != nil {
		t.Fatal(err)
	}
	nonceBinding, err := provsqlfixture.NonceBindingSHA256(sample.Scale, sample.ProcessReplicate, sample.Iteration, true)
	if err != nil {
		t.Fatal(err)
	}
	logical, err := provsqlfixture.LogicalSQL(sample.Scale, nonce)
	if err != nil {
		t.Fatal(err)
	}
	physical := provsqlfixture.PhysicalSQLSHA256()
	if mode == "taskgate" {
		physical = sha256Hex([]byte(logical))
	}
	sample.PhysicalSQLSHA256 = physical
	sample.LogicalSQLSHA256 = sha256Hex([]byte(logical))
	sample.ProvSQLVerification.Warmup = true
	sample.ProvSQLVerification.Nonce = nonce
	sample.ProvSQLVerification.NonceBindingSHA256 = nonceBinding
	sample.ProvSQLVerification.PhysicalSQLSHA256 = physical
	sample.ProvSQLVerification.LogicalSQLSHA256 = sample.LogicalSQLSHA256
	sample.ProvSQLVerification.ExecutionOrderSHA256 = sha256Hex([]byte(strings.Join([]string{
		provsqlfixture.Version, "execution-order-v2", sample.PairID, sample.PairedSystemOrder,
		strconv.Itoa(sample.OrderPosition), mode, strconv.FormatInt(nonce, 10),
	}, "\x00")))
	return sample
}

func cloneProvSQLValidationSample(source Sample) Sample {
	result := source
	evidence := *source.ProvSQLVerification
	evidence.FieldOIDs = append([]uint32(nil), source.ProvSQLVerification.FieldOIDs...)
	if evidence.BusinessBefore != nil {
		value := *evidence.BusinessBefore
		evidence.BusinessBefore = &value
	}
	if evidence.BusinessAfter != nil {
		value := *evidence.BusinessAfter
		evidence.BusinessAfter = &value
	}
	if evidence.RootBefore != nil {
		value := *evidence.RootBefore
		evidence.RootBefore = &value
	}
	if evidence.RootAfter != nil {
		value := *evidence.RootAfter
		evidence.RootAfter = &value
	}
	if evidence.ObserverBefore != nil {
		value := *evidence.ObserverBefore
		evidence.ObserverBefore = &value
	}
	if evidence.ObserverAfter != nil {
		value := *evidence.ObserverAfter
		evidence.ObserverAfter = &value
	}
	result.ProvSQLVerification = &evidence
	return result
}

func TestProvSQLIndependentValidatorRejectsCriticalMutations(t *testing.T) {
	for _, mode := range []string{"direct", "provsql", "taskgate"} {
		if err := ValidateProvSQLEvidence(validProvSQLValidationSample(t, mode)); err != nil {
			t.Fatalf("valid %s evidence: %v", mode, err)
		}
	}
	base := validProvSQLValidationSample(t, "direct")
	mutations := map[string]func(*Sample){
		"fixture version": func(sample *Sample) { sample.ProvSQLVerification.FixtureVersion = "other" },
		"dataset":         func(sample *Sample) { sample.ProvSQLVerification.DatasetSHA256 = strings.Repeat("0", 64) },
		"nonce":           func(sample *Sample) { sample.ProvSQLVerification.Nonce++ },
		"nonce binding":   func(sample *Sample) { sample.ProvSQLVerification.NonceBindingSHA256 = strings.Repeat("0", 64) },
		"physical SQL":    func(sample *Sample) { sample.ProvSQLVerification.PhysicalSQLSHA256 = strings.Repeat("0", 64) },
		"logical SQL":     func(sample *Sample) { sample.ProvSQLVerification.LogicalSQLSHA256 = strings.Repeat("0", 64) },
		"result oracle":   func(sample *Sample) { sample.ProvSQLVerification.ExpectedResultSHA256 = strings.Repeat("0", 64) },
		"dependency":      func(sample *Sample) { sample.ProvSQLVerification.ExpectedDependencyFacts = 0 },
		"typed drain":     func(sample *Sample) { sample.ProvSQLVerification.TypedDrainSHA256 = "bad" },
		"binding":         func(sample *Sample) { sample.ProvSQLVerification.BindingSHA256 = "bad" },
		"cache":           func(sample *Sample) { sample.ProvSQLVerification.CacheConditionSHA256 = strings.Repeat("0", 64) },
		"order":           func(sample *Sample) { sample.ProvSQLVerification.ExecutionOrderSHA256 = strings.Repeat("0", 64) },
		"failure stage":   func(sample *Sample) { sample.ProvSQLVerification.FailureStage = "unexpected" },
	}
	for name, mutate := range mutations {
		sample := cloneProvSQLValidationSample(base)
		mutate(&sample)
		if err := ValidateProvSQLEvidence(sample); err == nil {
			t.Fatalf("%s mutation was accepted", name)
		}
	}

	prov := validProvSQLValidationSample(t, "provsql")
	for name, mutate := range map[string]func(*Sample){
		"root type": func(sample *Sample) { sample.ProvSQLVerification.RootTypesVerified = false },
		"gate type": func(sample *Sample) { sample.ProvSQLVerification.CarrierGateType = "delta" },
		"token count": func(sample *Sample) {
			sample.ProvSQLVerification.AggregateTokens--
		},
		"gate growth": func(sample *Sample) {
			sample.ProvSQLVerification.GatesAfter = sample.ProvSQLVerification.GatesBefore
		},
		"byte regression": func(sample *Sample) {
			sample.ProvSQLVerification.ArtifactBytesAfter = sample.ProvSQLVerification.ArtifactBytesBefore - 1
		},
		"representation": func(sample *Sample) { sample.ProvSQLVerification.RepresentationSHA256 = "bad" },
	} {
		sample := cloneProvSQLValidationSample(prov)
		mutate(&sample)
		if err := ValidateProvSQLEvidence(sample); err == nil {
			t.Fatalf("ProvSQL %s mutation was accepted", name)
		}
	}

	taskgate := validProvSQLValidationSample(t, "taskgate")
	for name, mutate := range map[string]func(*Sample){
		"FactSet count":        func(sample *Sample) { sample.ActualDependencyFacts-- },
		"FactSet digest":       func(sample *Sample) { sample.DependencySetSHA256 = strings.Repeat("0", 64) },
		"full boundary":        func(sample *Sample) { sample.FullTaskGateMS-- },
		"circuit leak":         func(sample *Sample) { sample.ProvSQLVerification.GatesAfter = 1 },
		"missing SQL snapshot": func(sample *Sample) { sample.ProvSQLVerification.BusinessBefore = nil },
		"targeted visible SQL": func(sample *Sample) { sample.ProvSQLVerification.BusinessAfter.VisibleCalls++ },
		"observer total SQL": func(sample *Sample) {
			sample.ProvSQLVerification.ObserverAfter.BusinessSQLQueries++
		},
		"observer identity": func(sample *Sample) {
			sample.ProvSQLVerification.ObserverAfter.RuntimeIdentitySHA256 = strings.Repeat("6", 64)
		},
		"root transition": func(sample *Sample) { sample.ProvSQLVerification.RootAfter.Epoch++ },
	} {
		sample := cloneProvSQLValidationSample(taskgate)
		mutate(&sample)
		if err := ValidateProvSQLEvidence(sample); err == nil {
			t.Fatalf("TaskGate %s mutation was accepted", name)
		}
	}

	directWithTaskGateEvidence := cloneProvSQLValidationSample(base)
	value := BusinessSQLSnapshot{StatsResetUnixMicro: 1}
	directWithTaskGateEvidence.ProvSQLVerification.BusinessBefore = &value
	if err := ValidateProvSQLEvidence(directWithTaskGateEvidence); err == nil {
		t.Fatal("direct arm accepted manufactured TaskGate runtime evidence")
	}
}

func TestProvSQLWarmupUsesSameIndependentGateAndDisjointNonceDomain(t *testing.T) {
	for _, mode := range []string{"direct", "provsql", "taskgate"} {
		sample := validProvSQLWarmupValidationSample(t, mode)
		if err := ValidateProvSQLWarmupEvidence(sample); err != nil {
			t.Fatalf("valid %s warmup evidence: %v", mode, err)
		}
		if err := ValidateProvSQLEvidence(sample); err == nil {
			t.Fatalf("measured validator accepted %s warmup nonce", mode)
		}
		mutated := cloneProvSQLValidationSample(sample)
		mutated.ProvSQLVerification.TypedDrainSHA256 = "bad"
		if err := ValidateProvSQLWarmupEvidence(mutated); err == nil {
			t.Fatalf("warmup validator accepted %s typed-drain mutation", mode)
		}
	}
	if err := ValidateProvSQLWarmupEvidence(validProvSQLValidationSample(t, "direct")); err == nil {
		t.Fatal("warmup validator accepted a measured nonce")
	}
}

func TestProvSQLFinalizerCrossEvidenceRejectsPairAndSequenceMutations(t *testing.T) {
	direct := validProvSQLValidationSample(t, "direct").ProvSQLVerification
	prov := validProvSQLValidationSample(t, "provsql").ProvSQLVerification
	taskgate := validProvSQLValidationSample(t, "taskgate").ProvSQLVerification
	pairs := map[string]map[string]*ProvSQLVerificationEvidence{"pair": {
		"direct": direct, "provsql": prov, "taskgate": taskgate,
	}}
	sequences := map[string][]provSQLSequenceObservation{"deployment/provsql": {
		{order: 2, iteration: 1, nonce: 101, gatesBefore: 10, gatesAfter: 20,
			artifactBytesBefore: 100, artifactBytesAfter: 200, representation: strings.Repeat("d", 64)},
		{order: 5, iteration: 2, nonce: 102, gatesBefore: 20, gatesAfter: 30,
			artifactBytesBefore: 200, artifactBytesAfter: 300, representation: strings.Repeat("e", 64)},
	}}
	if reasons := validateProvSQLCrossEvidence(pairs, sequences); len(reasons) != 0 {
		t.Fatalf("valid cross evidence = %v", reasons)
	}
	plateauThenGrowth := map[string][]provSQLSequenceObservation{"deployment/provsql": {
		{order: 2, iteration: 1, nonce: 101, gatesBefore: 10, gatesAfter: 20,
			artifactBytesBefore: 100, artifactBytesAfter: 100, representation: strings.Repeat("d", 64)},
		{order: 5, iteration: 2, nonce: 102, gatesBefore: 20, gatesAfter: 30,
			artifactBytesBefore: 100, artifactBytesAfter: 300, representation: strings.Repeat("e", 64)},
	}}
	if reasons := validateProvSQLCrossEvidence(pairs, plateauThenGrowth); len(reasons) != 0 {
		t.Fatalf("valid mmap allocation plateau = %v", reasons)
	}
	if reasons := validateProvSQLCrossEvidence(map[string]map[string]*ProvSQLVerificationEvidence{
		"pair": {"direct": direct, "provsql": prov},
	}, sequences); !containsReason(reasons, "incomplete") {
		t.Fatalf("incomplete three-arm pair reasons = %v", reasons)
	}
	badTaskGate := *taskgate
	badTaskGate.Nonce++
	if reasons := validateProvSQLCrossEvidence(map[string]map[string]*ProvSQLVerificationEvidence{"pair": {
		"direct": direct, "provsql": prov, "taskgate": &badTaskGate,
	}}, sequences); !containsReason(reasons, "binding mismatch") {
		t.Fatalf("nonce mismatch reasons = %v", reasons)
	}
	badSequence := append([]provSQLSequenceObservation(nil), sequences["deployment/provsql"]...)
	badSequence[1].nonce = badSequence[0].nonce
	badSequence[1].representation = badSequence[0].representation
	badSequence[1].gatesBefore = 19
	if reasons := validateProvSQLCrossEvidence(pairs, map[string][]provSQLSequenceObservation{"bad": badSequence}); !containsReason(reasons, "unique and strict") || !containsReason(reasons, "regress") {
		t.Fatalf("sequence mutation reasons = %v", reasons)
	}
	allPlateau := map[string][]provSQLSequenceObservation{"deployment/provsql": {
		{order: 2, iteration: 1, nonce: 101, gatesBefore: 10, gatesAfter: 20,
			artifactBytesBefore: 100, artifactBytesAfter: 100, representation: strings.Repeat("d", 64)},
		{order: 5, iteration: 2, nonce: 102, gatesBefore: 20, gatesAfter: 30,
			artifactBytesBefore: 100, artifactBytesAfter: 100, representation: strings.Repeat("e", 64)},
	}}
	if reasons := validateProvSQLCrossEvidence(pairs, allPlateau); !containsReason(reasons, "never grew") {
		t.Fatalf("all-plateau mmap sequence reasons = %v", reasons)
	}
}

func TestProvSQLAllThreeArmsBindToEnvironmentSectionDigest(t *testing.T) {
	digest := strings.Repeat("a", 64)
	observed := map[string]map[string]bool{"deployment-01": {}}
	for _, mode := range []string{"direct", "provsql", "taskgate"} {
		sample := validProvSQLValidationSample(t, mode)
		got := sampleAdapterBindingSHA256(sample)
		if got != digest {
			t.Fatalf("%s arm binding = %q, want %q", mode, got, digest)
		}
		observed["deployment-01"][got] = true
	}
	if reasons := validatePublicationSampleBindingDigests("provsql",
		map[string]string{"deployment-01": digest}, observed); len(reasons) != 0 {
		t.Fatalf("three-arm binding reasons = %v", reasons)
	}
	observed["deployment-01"][strings.Repeat("f", 64)] = true
	if reasons := validatePublicationSampleBindingDigests("provsql",
		map[string]string{"deployment-01": digest}, observed); !containsReason(reasons, "strict adapter binding mismatch") {
		t.Fatalf("mutated three-arm binding reasons = %v", reasons)
	}
}

func containsReason(reasons []string, fragment string) bool {
	for _, reason := range reasons {
		if strings.Contains(reason, fragment) {
			return true
		}
	}
	return false
}
