package experiment

import (
	"strconv"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/provsqlfixture"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
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
		ClientAvailableMS: 1, ClientFullDrainMS: 3,
		PipelineMS: map[string]float64{
			"prepare": 0, "execute_and_derive": 3, "artifact_stage": 0,
			"control_settlement": 0, "artifact_publication": 0,
			"response_finalize": 0, "server_total": 3,
		},
		DiagnosticMS: map[string]float64{},
		RowCount:     provsqlfixture.ExpectedRows, ColumnCount: provsqlfixture.ExpectedColumns,
		ResultSHA256: resultSHA, PhysicalSQLSHA256: physical, LogicalSQLSHA256: sha256Hex([]byte(logical)),
		Status: "pass", PublicationEligible: true,
	}
	evidence := &ProvSQLVerificationEvidence{
		Version:           "taskgate-final-v5-provsql-verification-v1",
		BindingFileSHA256: strings.Repeat("7", 64), BindingSHA256: strings.Repeat("a", 64),
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
		evidence.FieldOIDs = []uint32{}
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
		evidence.BusinessBefore, evidence.BusinessAfter = &businessBefore, &businessAfter
		evidence.RootBefore, evidence.RootAfter = &rootBefore, &rootAfter
		sample.BusinessSQLDelta = 2
		sample.RootEpochBefore, sample.RootEpochAfter = rootBefore.Epoch, rootAfter.Epoch
		sample.ReleaseSetSHA256, sample.OutcomeSetSHA256 = rootAfter.ReleaseSetSHA256, rootAfter.OutcomeSetSHA256
		sample.RootSetSHA256Before, sample.RootSetSHA256After = rootLedgerSetSHA256(rootBefore), rootLedgerSetSHA256(rootAfter)
		sample.GatewayMemoryPeakBytes = 200
		sample.GatewayCPUUsecDelta, sample.GatewayNetworkRXDelta, sample.GatewayNetworkTXDelta = 1, 2, 3
		sample.ControlWALBytesDelta, sample.BusinessWALBytesDelta = 4, 5
		attachProvSQLV3ObservationFixture(t, &sample, evidence)
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

func attachProvSQLV3ObservationFixture(t *testing.T, sample *Sample,
	evidence *ProvSQLVerificationEvidence) {
	t.Helper()
	plan := mustPairedNovel(t, 1)
	planSHA256, err := plan.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	row := ObserverStructuralRow{StrictASTSHA256: strings.Repeat("e", 64),
		TopLevel: true, Calls: plan.ExpectedTotal()}
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
	operation := OperationIdentity{
		OperationID:                sampleOperationIDV3(*sample),
		PathKind:                   PathPairedNovel,
		ExpectedSchemaDigest:       plan.ExpectedSchemaDigest,
		AttestationFootprintSHA256: plan.AttestationFootprintSHA256,
	}
	operation.ContractIdentity = provSQLContractIdentityForTest(*sample, evidence)
	classifierBinding, err := classifierBindingSHA256(operation, planSHA256,
		before.ClassifierManifestSHA256)
	if err != nil {
		t.Fatal(err)
	}
	receipt := queryreceipt.QueryReceiptV1{Version: "fixture", RequestID: "provsql-attempt-a"}
	receiptSHA256, err := queryreceipt.DocumentSHA256(receipt)
	if err != nil {
		t.Fatal(err)
	}
	accepted := FinalizationV3{
		Operation: operation, Plan: plan, PlanSHA256: planSHA256,
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
	evidence.ObserverWindow = &window
	sample.ReceiptSHA256 = receiptSHA256
	sample.BaselineVerification = &BaselineVerificationEvidence{Receipt: receipt}
	sample.TaskGateAcceptanceV3 = &accepted
}

func provSQLContractIdentityForTest(sample Sample, evidence *ProvSQLVerificationEvidence) string {
	operationID := sampleOperationIDV3(sample)
	return "final-v5-contracts-v1.4:" + strings.Repeat("9", 64) +
		":binding-file=" + evidence.BindingFileSHA256 +
		":binding-section=" + evidence.BindingSHA256 +
		":binding-key=" + sample.Scale + "/" + strconv.FormatInt(evidence.Nonce, 10) +
		":binding-query=" + evidence.LogicalSQLSHA256 + ":" + operationID
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
	if mode == "taskgate" {
		sample.TaskGateAcceptanceV3.Operation.ContractIdentity =
			provSQLContractIdentityForTest(sample, sample.ProvSQLVerification)
		resealAcceptedClassifierForTest(t, sample.TaskGateAcceptanceV3)
	}
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
	if evidence.ObserverWindow != nil {
		value := *evidence.ObserverWindow
		value.Before.Structural = append([]ObserverStructuralRow(nil), value.Before.Structural...)
		value.After.Structural = append([]ObserverStructuralRow(nil), value.After.Structural...)
		evidence.ObserverWindow = &value
	}
	result.ProvSQLVerification = &evidence
	if source.BaselineVerification != nil {
		value := *source.BaselineVerification
		result.BaselineVerification = &value
	}
	if source.TaskGateAcceptanceV3 != nil {
		value := *source.TaskGateAcceptanceV3
		value.Plan.InternalExpectation = append([]InternalExpectation(nil), value.Plan.InternalExpectation...)
		value.InternalExpectation = append([]InternalExpectation(nil), value.InternalExpectation...)
		value.Delta.PerClass = make(map[GatewayStatementClassV3]int64, len(source.TaskGateAcceptanceV3.Delta.PerClass))
		for class, count := range source.TaskGateAcceptanceV3.Delta.PerClass {
			value.Delta.PerClass[class] = count
		}
		value.Delta.Internal = append([]InternalExpectation(nil), value.Delta.Internal...)
		value.Delta.Unexpected = append([]ObserverStructuralRow(nil), value.Delta.Unexpected...)
		result.TaskGateAcceptanceV3 = &value
	}
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
		"binding file":    func(sample *Sample) { sample.ProvSQLVerification.BindingFileSHA256 = "bad" },
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
		"missing observer window": func(sample *Sample) {
			sample.ProvSQLVerification.ObserverWindow = nil
		},
		"missing acceptance": func(sample *Sample) { sample.TaskGateAcceptanceV3 = nil },
		"retained receipt": func(sample *Sample) {
			sample.BaselineVerification.Receipt.RequestID = "another-attempt"
		},
		"accepted receipt": func(sample *Sample) {
			sample.TaskGateAcceptanceV3.ReceiptSHA256 = strings.Repeat("6", 64)
		},
		"wrong path": func(sample *Sample) { sample.TaskGateAcceptanceV3.Operation.PathKind = PathSemanticReplay },
		"classifier": func(sample *Sample) {
			sample.TaskGateAcceptanceV3.ClassifierManifestSHA256 = strings.Repeat("6", 64)
		},
		"window identity": func(sample *Sample) {
			sample.ProvSQLVerification.ObserverWindow.After.ObserverWindowID = strings.Repeat("6", 64)
		},
		"window total": func(sample *Sample) {
			sample.ProvSQLVerification.ObserverWindow.After.Structural[0].Calls++
			sample.ProvSQLVerification.ObserverWindow.After.Total++
		},
		"accepted delta":  func(sample *Sample) { sample.TaskGateAcceptanceV3.Delta.Total++ },
		"resource":        func(sample *Sample) { sample.GatewayCPUUsecDelta++ },
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
	directWithAcceptance := cloneProvSQLValidationSample(base)
	directWithAcceptance.TaskGateAcceptanceV3 = &FinalizationV3{}
	if err := ValidateProvSQLEvidence(directWithAcceptance); err == nil {
		t.Fatal("direct arm accepted a manufactured v3 acceptance")
	}
}

func TestProvSQLAcceptanceClosesExactPrivateNonceContractIdentity(t *testing.T) {
	base := validProvSQLValidationSample(t, "taskgate")
	original := base.TaskGateAcceptanceV3.Operation.ContractIdentity
	expectedKey := "binding-key=" + base.Scale + "/" +
		strconv.FormatInt(base.ProvSQLVerification.Nonce, 10)
	for name, mutate := range map[string]func(string) string{
		"part count": func(identity string) string { return identity + ":extra" },
		"release": func(identity string) string {
			return strings.TrimPrefix(identity, "final-v5-contracts-v1.4")
		},
		"index": func(identity string) string {
			return strings.Replace(identity, ":"+strings.Repeat("9", 64)+":binding-file=",
				":bad:binding-file=", 1)
		},
		"binding file prefix": func(identity string) string {
			return strings.Replace(identity, ":binding-file=", ":private-file=", 1)
		},
		"binding file": func(identity string) string {
			return strings.Replace(identity, "binding-file="+strings.Repeat("7", 64),
				"binding-file="+strings.Repeat("6", 64), 1)
		},
		"binding section prefix": func(identity string) string {
			return strings.Replace(identity, ":binding-section=", ":private-section=", 1)
		},
		"binding section": func(identity string) string {
			return strings.Replace(identity, "binding-section="+strings.Repeat("a", 64),
				"binding-section="+strings.Repeat("6", 64), 1)
		},
		"binding key prefix": func(identity string) string {
			return strings.Replace(identity, ":binding-key=", ":private-key=", 1)
		},
		"binding key": func(identity string) string {
			return strings.Replace(identity, expectedKey,
				"binding-key="+base.Scale+"/"+strconv.FormatInt(base.ProvSQLVerification.Nonce+1, 10), 1)
		},
		"binding query prefix": func(identity string) string {
			return strings.Replace(identity, ":binding-query=", ":private-query=", 1)
		},
		"binding query": func(identity string) string {
			return strings.Replace(identity, "binding-query="+base.ProvSQLVerification.LogicalSQLSHA256,
				"binding-query="+strings.Repeat("6", 64), 1)
		},
		"operation": func(identity string) string {
			return strings.TrimSuffix(identity, sampleOperationIDV3(base)) +
				"provsql/nonce-join-group/10k/taskgate"
		},
	} {
		t.Run(name, func(t *testing.T) {
			sample := cloneProvSQLValidationSample(base)
			sample.TaskGateAcceptanceV3.Operation.ContractIdentity = mutate(original)
			resealAcceptedClassifierForTest(t, sample.TaskGateAcceptanceV3)
			if err := ValidateProvSQLEvidence(sample); err == nil {
				t.Fatal("a private ProvSQL contract identity mutation survived retained validation")
			}
		})
	}
}

func TestProvSQLV3AcceptanceIsWhitelistedOnlyForTheTaskGateArm(t *testing.T) {
	taskgate := validProvSQLValidationSample(t, "taskgate")
	if reasons, failed := validateExperimentEvidence(taskgate); failed {
		t.Fatalf("valid ProvSQL TaskGate v3 evidence was rejected: %v", reasons)
	}
	direct := validProvSQLValidationSample(t, "direct")
	direct.TaskGateAcceptanceV3 = taskgate.TaskGateAcceptanceV3
	if reasons, failed := validateExperimentEvidence(direct); !failed ||
		!containsReason(reasons, "outside an explicitly cut-over TaskGate path") {
		t.Fatalf("direct ProvSQL arm with v3 acceptance reasons = %v, failed = %v", reasons, failed)
	}
}

func TestProvSQLAcceptanceRefusesAnotherNonceFromTheSamePublicCell(t *testing.T) {
	target := validProvSQLValidationSample(t, "taskgate")
	donor := validProvSQLValidationSample(t, "taskgate")
	retargetProvSQLMeasuredIteration(t, &donor, 2, "8")
	if err := ValidateProvSQLEvidence(donor); err != nil {
		t.Fatalf("valid donor nonce: %v", err)
	}

	spliced := cloneProvSQLValidationSample(target)
	donor = cloneProvSQLValidationSample(donor)
	spliced.TaskGateAcceptanceV3 = donor.TaskGateAcceptanceV3
	spliced.ProvSQLVerification.ObserverWindow = donor.ProvSQLVerification.ObserverWindow
	spliced.BaselineVerification = donor.BaselineVerification
	spliced.ReceiptSHA256 = donor.ReceiptSHA256
	if err := ValidateProvSQLEvidence(spliced); err == nil {
		t.Fatal("one ProvSQL nonce accepted another nonce's coherent acceptance/window/receipt")
	}
}

func retargetProvSQLMeasuredIteration(t *testing.T, sample *Sample, iteration int,
	hexDigit string) {
	t.Helper()
	sample.Iteration = iteration
	nonce, err := provsqlfixture.Nonce(sample.Scale, sample.ProcessReplicate, iteration, false)
	if err != nil {
		t.Fatal(err)
	}
	nonceBinding, err := provsqlfixture.NonceBindingSHA256(sample.Scale,
		sample.ProcessReplicate, iteration, false)
	if err != nil {
		t.Fatal(err)
	}
	logical, err := provsqlfixture.LogicalSQL(sample.Scale, nonce)
	if err != nil {
		t.Fatal(err)
	}
	logicalSHA256 := sha256Hex([]byte(logical))
	sample.PhysicalSQLSHA256, sample.LogicalSQLSHA256 = logicalSHA256, logicalSHA256
	evidence := sample.ProvSQLVerification
	evidence.Nonce, evidence.NonceBindingSHA256 = nonce, nonceBinding
	evidence.PhysicalSQLSHA256, evidence.LogicalSQLSHA256 = logicalSHA256, logicalSHA256
	evidence.ExecutionOrderSHA256 = sha256Hex([]byte(strings.Join([]string{
		provsqlfixture.Version, "execution-order-v2", sample.PairID, sample.PairedSystemOrder,
		strconv.Itoa(sample.OrderPosition), sample.Mode, strconv.FormatInt(nonce, 10),
	}, "\x00")))
	sample.TaskGateAcceptanceV3.Operation.ContractIdentity =
		provSQLContractIdentityForTest(*sample, evidence)
	resealAcceptedClassifierForTest(t, sample.TaskGateAcceptanceV3)

	receipt := sample.BaselineVerification.Receipt
	receipt.RequestID = "provsql-attempt-" + hexDigit
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
	badBindingFile := *taskgate
	badBindingFile.BindingFileSHA256 = strings.Repeat("f", 64)
	if reasons := validateProvSQLCrossEvidence(map[string]map[string]*ProvSQLVerificationEvidence{"pair": {
		"direct": direct, "provsql": prov, "taskgate": &badBindingFile,
	}}, sequences); !containsReason(reasons, "binding mismatch") {
		t.Fatalf("binding-file mismatch reasons = %v", reasons)
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
