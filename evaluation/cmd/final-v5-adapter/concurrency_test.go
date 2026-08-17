package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/concurrencyfixture"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	gatewayapp "taskbound.local/agent-data-gateway/internal/gateway"
	"taskbound.local/agent-data-gateway/internal/querybinding"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

type fakeConcurrencyBackend struct {
	capacity gatewayapp.ConcurrencyProbeCapacity
	capErr   error
	run      func(experiment.AdapterOperation, concurrencyfixture.Cell) (experiment.Sample, error)
	cells    []concurrencyfixture.Cell
	closed   bool
}

func (backend *fakeConcurrencyBackend) Capacity(context.Context) (gatewayapp.ConcurrencyProbeCapacity, error) {
	return backend.capacity, backend.capErr
}

func (backend *fakeConcurrencyBackend) Run(_ context.Context, operation experiment.AdapterOperation,
	cell concurrencyfixture.Cell) (experiment.Sample, error) {
	backend.cells = append(backend.cells, cell)
	if backend.run != nil {
		return backend.run(operation, cell)
	}
	return invalidSample(operation, "retained_fake_backend_sample"), nil
}

func (backend *fakeConcurrencyBackend) Close() { backend.closed = true }

func TestConcurrencyAdapterRecognizesExactlyFrozenMatrix(t *testing.T) {
	backend := &fakeConcurrencyBackend{capacity: validConcurrencyTestCapacity()}
	adapter, err := newConcurrencyAdapterWithBackend(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	for index, cell := range concurrencyfixture.FrozenCells {
		operation := concurrencyTestOperation(cell)
		sample := adapter.Execute(context.Background(), operation)
		if sample.Status != "invalid" || sample.ErrorCode != "retained_fake_backend_sample" {
			t.Fatalf("cell %d was not routed to the source-controlled backend: %+v", index, sample)
		}
	}
	if !reflect.DeepEqual(backend.cells, concurrencyfixture.FrozenCells) {
		t.Fatalf("routed cells differ from frozen matrix: %+v", backend.cells)
	}
	unsupported := concurrencyTestOperation(concurrencyfixture.Cell{WorkloadID: "shared-root", Scale: "500", Mode: "client_barrier", Width: 500})
	before := len(backend.cells)
	sample := adapter.Execute(context.Background(), unsupported)
	if sample.Status != "invalid" || sample.ErrorCode != "unsupported_source_controlled_concurrency_cell" || len(backend.cells) != before {
		t.Fatalf("unsupported client-only cell reached backend: %+v", sample)
	}
	adapter.Close()
	if !backend.closed {
		t.Fatal("concurrency backend was not closed")
	}
}

func TestConcurrencyReceiptCatalogIsCheckedBeforeCompression(t *testing.T) {
	publication, err := experiment.CanonicalPublicationSetSHA256([]string{"publication-a"})
	if err != nil {
		t.Fatal(err)
	}
	binding := &experiment.ProfileBinding{
		Version: experiment.ProfileBindingVersion, ProfileID: "profile-a86cd4df5cad6e26",
		ClosureSHA256: strings.Repeat("a", 64), CatalogSHA256: strings.Repeat("b", 64),
		DatasetBindingSHA256: strings.Repeat("c", 64), PublicationIdentity: publication,
	}
	adapterSampleProfileBinder, err = experiment.NewSampleProfileBinder(binding)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adapterSampleProfileBinder = nil })
	sample := experiment.Sample{BaselineVerification: &experiment.BaselineVerificationEvidence{
		Receipt: queryreceipt.QueryReceiptV1{CatalogDigest: strings.Repeat("d", 64),
			ExecutionBindingV2: &querybinding.QueryExecutionBindingV2{}},
	}}
	if err := validateConcurrencyContenderReceiptBeforeCompression(sample); err == nil {
		t.Fatal("a contender with the wrong observed Catalog reached compact evidence")
	}
	sample.BaselineVerification.Receipt.CatalogDigest = binding.CatalogSHA256
	if err := validateConcurrencyContenderReceiptBeforeCompression(sample); err != nil {
		t.Fatalf("matching contender Catalog was rejected: %v", err)
	}
}

func TestConcurrencyAdapterRetainsInvalidAndFailedEvidence(t *testing.T) {
	cell := concurrencyfixture.FrozenCells[1]
	operation := concurrencyTestOperation(cell)
	tests := []struct {
		name       string
		run        func(experiment.AdapterOperation, concurrencyfixture.Cell) (experiment.Sample, error)
		wantStatus string
		wantCode   string
	}{
		{
			name: "offered width invalid",
			run: func(operation experiment.AdapterOperation, _ concurrencyfixture.Cell) (experiment.Sample, error) {
				retained := invalidSample(operation, "preexisting")
				retained.Counters = map[string]int64{"authenticated_partial_arrivals": 7}
				return retained, &concurrencyRunError{code: "offered_concurrency_not_observed", invalid: true, sample: retained}
			},
			wantStatus: "invalid", wantCode: "offered_concurrency_not_observed",
		},
		{
			name: "typed real failure",
			run: func(operation experiment.AdapterOperation, _ concurrencyfixture.Cell) (experiment.Sample, error) {
				retained := invalidSample(operation, "preexisting")
				retained.Counters = map[string]int64{"authenticated_partial_arrivals": 8}
				return retained, &concurrencyRunError{code: "production_invariant_failed", sample: retained, cause: errors.New("private detail")}
			},
			wantStatus: "fail", wantCode: "production_invariant_failed",
		},
		{
			name: "generic real failure with sample",
			run: func(operation experiment.AdapterOperation, _ concurrencyfixture.Cell) (experiment.Sample, error) {
				retained := invalidSample(operation, "preexisting")
				retained.Counters = map[string]int64{"authenticated_partial_arrivals": 9}
				return retained, errors.New("private backend detail")
			},
			wantStatus: "fail", wantCode: "real_concurrency_measurement_failed",
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			diagnostics := captureAdapterDiagnostics(t)
			backend := &fakeConcurrencyBackend{capacity: validConcurrencyTestCapacity(), run: test.run}
			adapter, err := newConcurrencyAdapterWithBackend(context.Background(), backend)
			if err != nil {
				t.Fatal(err)
			}
			sample := adapter.Execute(context.Background(), operation)
			if sample.Status != test.wantStatus || sample.ErrorCode != test.wantCode ||
				sample.Counters["authenticated_partial_arrivals"] != int64(index+7) {
				t.Fatalf("retained sample was discarded or relabeled incorrectly: %+v", sample)
			}
			if strings.Contains(sample.Reason, "private") {
				t.Fatalf("private backend error leaked into retained evidence: %q", sample.Reason)
			}
			if got := diagnostics.String(); got == "" {
				t.Fatal("backend failure was absent from adapter stderr")
			}
			if test.name != "offered width invalid" && !strings.Contains(diagnostics.String(), "private") {
				t.Fatalf("private backend detail was absent from adapter stderr: %q", diagnostics.String())
			}
		})
	}
}

func TestConcurrencyAdapterRetainsPassEvidenceThatFailsInvariantGate(t *testing.T) {
	cell := concurrencyfixture.FrozenCells[1]
	backend := &fakeConcurrencyBackend{capacity: validConcurrencyTestCapacity(), run: func(operation experiment.AdapterOperation,
		_ concurrencyfixture.Cell) (experiment.Sample, error) {
		sample := baseSample(operation, "taskgate")
		sample.Status = "pass"
		sample.Counters = map[string]int64{"retained_marker": 11}
		return sample, nil
	}}
	adapter, err := newConcurrencyAdapterWithBackend(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	sample := adapter.Execute(context.Background(), concurrencyTestOperation(cell))
	if sample.Status != "fail" || sample.ErrorCode != "concurrency_evidence_invariant_failed" || sample.Counters["retained_marker"] != 11 {
		t.Fatalf("failed post-run evidence was discarded: %+v", sample)
	}
}

func TestObservedConcurrencyServiceSampleDoesNotFabricateLaterEvidence(t *testing.T) {
	cell := concurrencyfixture.FrozenCells[1]
	operation := concurrencyTestOperation(cell)
	capacity := validConcurrencyTestCapacity()
	roundSHA := concurrencyfixture.RoundSHA256(concurrencyRoundIdentity(operation))
	initial := experiment.RootLedgerSnapshot{Epoch: 0, OutcomeCardinality: 0}
	before := experiment.RootLedgerSnapshot{Epoch: 3, OutcomeCardinality: concurrencyfixture.UsageBeforeBoundary}
	snapshot := gatewayapp.ConcurrencyProbeSnapshot{
		ConcurrencyProbeCapacity: capacity, RoundSHA256: roundSHA, Mode: operation.Mode,
		ExpectedWidth: int64(cell.Width), Arrived: int64(cell.Width), UniqueParticipants: int64(cell.Width),
		ParticipantSetSHA256: concurrencyfixture.ParticipantSetSHA256(roundSHA, cell.Width),
		PeakBarrierWaiting:   int64(cell.Width), PeakActive: 8, PeakQueued: 2,
		Completed: int64(cell.Width), PeakControlPoolInUse: 4, Released: true,
	}
	sample := observedConcurrencyServiceSample(operation, capacity, snapshot, roundSHA, strings.Repeat("b", 64),
		initial, before, 3)
	evidence := sample.ConcurrencyVerification
	if evidence == nil || sample.Counters["offered_concurrency_observed"] != 1 ||
		evidence.ServiceUniqueParticipants != int64(cell.Width) || evidence.RootLockWaitersObserved != 3 {
		t.Fatalf("authenticated service prefix was not retained: %+v", sample)
	}
	if len(evidence.Contenders) != 0 || evidence.Accepted != 0 || evidence.FinalRootSetSHA256 != "" ||
		evidence.Overflow != (experiment.ConcurrencyOverflowEvidence{}) {
		t.Fatalf("unobserved contender/root/overflow evidence was fabricated: %+v", evidence)
	}
	if evidence.AtBoundary != before || evidence.AfterRejectedOverflow != before {
		t.Fatalf("last independently observed root prefix was not retained: %+v", evidence)
	}
}

func TestConcurrencyLateFailureRetainsRootContenderAndOverflowPrefixes(t *testing.T) {
	cell := concurrencyfixture.FrozenCells[1]
	operation := concurrencyTestOperation(cell)
	capacity := validConcurrencyTestCapacity()
	roundSHA := concurrencyfixture.RoundSHA256(concurrencyRoundIdentity(operation))
	rootTaskIDHash := strings.Repeat("b", 64)
	before := experiment.RootLedgerSnapshot{Epoch: 3, OutcomeCardinality: concurrencyfixture.UsageBeforeBoundary}
	atBoundary := experiment.RootLedgerSnapshot{Epoch: 4, OutcomeCardinality: concurrencyfixture.ExpectedFinalOutcome,
		OutcomeSetSHA256: strings.Repeat("c", 64)}
	snapshot := gatewayapp.ConcurrencyProbeSnapshot{
		ConcurrencyProbeCapacity: capacity, RoundSHA256: roundSHA, Mode: operation.Mode,
		ExpectedWidth: int64(cell.Width), Arrived: int64(cell.Width), UniqueParticipants: int64(cell.Width),
		ParticipantSetSHA256: concurrencyfixture.ParticipantSetSHA256(roundSHA, cell.Width),
		PeakBarrierWaiting:   int64(cell.Width), PeakActive: 8, PeakQueued: 2,
		Completed: int64(cell.Width), PeakControlPoolInUse: 4, Released: true,
	}
	servicePrefix := observedConcurrencyServiceSample(operation, capacity, snapshot, roundSHA, rootTaskIDHash,
		experiment.RootLedgerSnapshot{}, before, 0)
	representative := baseSample(operation, "taskgate")
	representative.Status = "pass"
	representative.RowCount, representative.ColumnCount = 1, 2
	representative.ResultSHA256 = concurrencyfixture.ExpectedContenderResultSHA256()
	representative.ReceiptVersion = queryreceipt.Version
	representative.ReceiptSHA256 = strings.Repeat("d", 64)
	representative.ArtifactIntentSHA256 = strings.Repeat("e", 64)
	representative.AvailabilityAuditSHA256 = strings.Repeat("f", 64)
	representative.ReceiptVerified, representative.ArtifactAvailable = true, true
	representative.ClientAvailableMS, representative.ClientFullDrainMS = 11, 17
	contender := experiment.ConcurrencyContenderEvidence{
		Index: 1, RequestIDHash: strings.Repeat("1", 64), ChargedOutcomeFacts: 1,
		CASAttempts: 2, CASConflicts: 1, CASRetries: 1,
	}
	retained := retainVerifiedConcurrencyPrefix(servicePrefix, representative,
		[]experiment.ConcurrencyContenderEvidence{contender}, atBoundary)
	retained.ConcurrencyVerification.FinalRootFactHashes = []string{strings.Repeat("2", 64)}
	retained.ConcurrencyVerification.FinalRootSetSHA256 = atBoundary.OutcomeSetSHA256
	retained.ConcurrencyVerification.Overflow = experiment.ConcurrencyOverflowEvidence{
		Attempts: 1, Rejected: 1, ErrorCode: "EXPOSURE_BUDGET_EXHAUSTED",
	}

	backend := &fakeConcurrencyBackend{capacity: capacity, run: func(experiment.AdapterOperation,
		concurrencyfixture.Cell) (experiment.Sample, error) {
		return retained, &concurrencyRunError{code: "concurrency_overflow_verification_failed", sample: retained,
			cause: errors.New("private late failure")}
	}}
	adapter, err := newConcurrencyAdapterWithBackend(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	sample := adapter.Execute(context.Background(), operation)
	evidence := sample.ConcurrencyVerification
	if sample.Status != "fail" || sample.ErrorCode != "concurrency_overflow_verification_failed" || evidence == nil {
		t.Fatalf("late failure was not retained as a typed failed sample: %+v", sample)
	}
	if sample.ResultSHA256 != representative.ResultSHA256 || !sample.ReceiptVerified || !sample.ArtifactAvailable ||
		sample.ClientAvailableMS != 11 || sample.ClientFullDrainMS != 17 {
		t.Fatalf("verified representative result/artifact/timing evidence was discarded: %+v", sample)
	}
	if evidence.AtBoundary != atBoundary || len(evidence.Contenders) != 1 || evidence.Accepted != 1 ||
		evidence.ProductionCASAttempts != 2 || evidence.ProductionCASConflicts != 1 ||
		evidence.FinalRootSetSHA256 != atBoundary.OutcomeSetSHA256 {
		t.Fatalf("root/contender/final-root prefix was discarded: %+v", evidence)
	}
	if evidence.Overflow.Attempts != 1 || evidence.Overflow.Rejected != 1 || evidence.Overflow.Found ||
		evidence.Overflow.Receipts != 0 {
		t.Fatalf("partial overflow evidence was discarded or unobserved completion was fabricated: %+v", evidence.Overflow)
	}
	if strings.Contains(sample.Reason, "private") {
		t.Fatalf("private backend error leaked into retained evidence: %q", sample.Reason)
	}
}

func TestConcurrencyConstructorRequiresAuthenticatedProductionCapacity(t *testing.T) {
	valid := validConcurrencyTestCapacity()
	tests := []struct {
		name   string
		mutate func(*gatewayapp.ConcurrencyProbeCapacity)
	}{
		{name: "wrong probe", mutate: func(value *gatewayapp.ConcurrencyProbeCapacity) { value.Version = "other" }},
		{name: "no instance", mutate: func(value *gatewayapp.ConcurrencyProbeCapacity) { value.GatewayInstanceSHA256 = "" }},
		{name: "no queue", mutate: func(value *gatewayapp.ConcurrencyProbeCapacity) { value.HTTPQueueCapacity = 0 }},
		{name: "width below 500", mutate: func(value *gatewayapp.ConcurrencyProbeCapacity) {
			value.HTTPActiveCapacity, value.HTTPQueueCapacity = 32, 467
		}},
		{name: "small control pool", mutate: func(value *gatewayapp.ConcurrencyProbeCapacity) { value.ControlPoolCapacity = 31 }},
		{name: "small connector pool", mutate: func(value *gatewayapp.ConcurrencyProbeCapacity) { value.ConnectorPoolCapacity = 31 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			capacity := valid
			test.mutate(&capacity)
			if _, err := newConcurrencyAdapterWithBackend(context.Background(), &fakeConcurrencyBackend{capacity: capacity}); err == nil {
				t.Fatal("unsafe concurrency capacity was accepted")
			}
		})
	}
	if _, err := newConcurrencyAdapterWithBackend(context.Background(), &fakeConcurrencyBackend{
		capacity: valid, capErr: errors.New("authenticated preflight unavailable"),
	}); err == nil {
		t.Fatal("failed authenticated preflight was accepted")
	}
}

func TestConcurrencyRealConstructorFailsClosedWithoutOptInToken(t *testing.T) {
	t.Setenv(concurrencyProbeTokenEnv, "")
	if _, err := newConcurrencyAdapter(context.Background()); err == nil || !strings.Contains(err.Error(), concurrencyProbeTokenEnv) {
		t.Fatalf("missing opt-in concurrency token was not rejected first: %v", err)
	}
}

func TestConcurrencySampleRetainsOnlyCompactPerContenderVerifierEvidence(t *testing.T) {
	representative := experiment.Sample{BaselineVerification: &experiment.BaselineVerificationEvidence{}}
	sample := buildConcurrencySample(experiment.AdapterOperation{}, gatewayapp.ConcurrencyProbeCapacity{},
		gatewayapp.ConcurrencyProbeSnapshot{}, concurrencyCreatedTask{}, "",
		experiment.RootLedgerSnapshot{}, experiment.RootLedgerSnapshot{}, experiment.RootLedgerSnapshot{},
		experiment.RootLedgerSnapshot{}, 0, nil, nil, representative, 0, 0,
		experiment.ConcurrencyOverflowEvidence{})
	if sample.BaselineVerification != nil {
		t.Fatal("concurrency sample retained raw receipt/audit proof instead of compact contender manifests")
	}
}

func validConcurrencyTestCapacity() gatewayapp.ConcurrencyProbeCapacity {
	return gatewayapp.ConcurrencyProbeCapacity{
		Version: gatewayapp.ConcurrencyProbeVersion, GatewayInstanceSHA256: strings.Repeat("a", 64),
		HTTPActiveCapacity: 64, HTTPQueueCapacity: 512, ControlPoolCapacity: 64, ConnectorPoolCapacity: 64,
	}
}

func concurrencyTestOperation(cell concurrencyfixture.Cell) experiment.AdapterOperation {
	return experiment.AdapterOperation{
		SchemaVersion: 1, CampaignID: "campaign", DeploymentID: "deployment-01", ExperimentID: "concurrency",
		CellID: cell.WorkloadID + "/" + cell.Scale + "/" + cell.Mode, SampleID: "sample", Iteration: 1,
		ProcessReplicate: 1, PairID: "pair", RootGroupID: "root", WorkloadID: cell.WorkloadID,
		Scale: cell.Scale, Mode: cell.Mode,
	}
}
