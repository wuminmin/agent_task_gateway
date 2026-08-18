package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/finalv5contracts"
	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5binding"
)

func testBoundTask(columns int) boundTaskRequest {
	values := make([]string, columns)
	for index := range values {
		values[index] = "column_" + string(rune('a'+index))
	}
	return boundTaskRequest{Objective: "source-controlled test", DataProducts: []string{"result_heavy"},
		Columns: map[string][]string{"result_heavy": values}, Scopes: map[string][]string{},
		VisibleRelation: "reporting.result_heavy", CompanionRelation: "taskgate_ordinal.result_heavy_v1"}
}

func TestRetainScaleOutcomeCandidateVerificationCopiesOnlyFinalizerOutput(t *testing.T) {
	members := []string{
		fmt.Sprintf("%064x", 1), fmt.Sprintf("%064x", 2), fmt.Sprintf("%064x", 3),
		fmt.Sprintf("%064x", 4), fmt.Sprintf("%064x", 5),
	}
	summary, err := finalv5oracle.SummarizeSemanticSet("candidate", func(yield func(string) error) error {
		for _, member := range members {
			if err := yield(member); err != nil {
				return err
			}
		}
		return nil
	}, finalv5oracle.StreamSetOptions{MaxInMemoryMembers: 5, CaptureMembers: 5})
	if err != nil {
		t.Fatal(err)
	}
	expectation := experiment.OutcomeCandidateExpectationV1{
		Cardinality: summary.Cardinality, Members: summary.Members, OrdinarySetSHA256: summary.SetSHA256,
	}
	finalized := experiment.FinalizationV3{OutcomeCandidateVerification: &experiment.OutcomeCandidateVerificationV1{
		Version:  experiment.OutcomeCandidateVerificationV1Version,
		Expected: expectation, Observed: expectation,
	}}
	evidence := &experiment.ScaleVerificationEvidence{Version: scaleDependencyVerificationVersion}
	if err := retainScaleOutcomeCandidateVerification(evidence, finalized); err != nil {
		t.Fatalf("retain finalizer verification: %v", err)
	}
	if evidence.Version != scaleDependencyVerificationV4 ||
		evidence.ExpectedOutcomeMemberCardinality != 5 || evidence.ObservedOutcomeMemberCardinality != 5 ||
		evidence.ExpectedOutcomeCandidateSetSHA256 != summary.SetSHA256 ||
		evidence.ObservedOutcomeCandidateSetSHA256 != summary.SetSHA256 {
		t.Fatalf("retained Scale evidence did not copy the finalizer result exactly: %+v", evidence)
	}
	if err := retainScaleOutcomeCandidateVerification(&experiment.ScaleVerificationEvidence{},
		experiment.FinalizationV3{}); err == nil {
		t.Fatal("Adapter accepted a finalization with no Outcome member verification")
	}
	mutated := finalized
	verification := *finalized.OutcomeCandidateVerification
	verification.Observed = expectation
	verification.Observed.Members = append([]string(nil), expectation.Members...)
	verification.Observed.Members[0] = fmt.Sprintf("%064x", 99)
	mutated.OutcomeCandidateVerification = &verification
	if err := retainScaleOutcomeCandidateVerification(&experiment.ScaleVerificationEvidence{}, mutated); err == nil {
		t.Fatal("Adapter retained an invalid finalizer Outcome member verification")
	}
}

func testBoundQuery(rows int64, columns int, dependencies int64) boundQueryExpectation {
	digest := strings.Repeat("a", 64)
	return boundQueryExpectation{SQL: "SELECT column_a FROM result_heavy ORDER BY column_a LIMIT 100",
		ExpectedRows: rows, ExpectedColumns: columns, ExpectedResultSHA256: digest,
		DependencyFacts: dependencies, DependencySetSHA256: digest}
}

func testBoundOutcomeCandidate(t *testing.T) finalv5binding.BoundOutcomeCandidateExpectation {
	t.Helper()
	members := []string{
		fmt.Sprintf("%064x", 1), fmt.Sprintf("%064x", 2), fmt.Sprintf("%064x", 3),
		fmt.Sprintf("%064x", 4), fmt.Sprintf("%064x", 5),
	}
	summary, err := finalv5oracle.SummarizeSemanticSet("candidate", func(yield func(string) error) error {
		for _, member := range members {
			if err := yield(member); err != nil {
				return err
			}
		}
		return nil
	}, finalv5oracle.StreamSetOptions{MaxInMemoryMembers: 5})
	if err != nil {
		t.Fatal(err)
	}
	return finalv5binding.BoundOutcomeCandidateExpectation{Cardinality: summary.Cardinality,
		Members: members, OrdinarySetSHA256: summary.SetSHA256}
}

// The Artifact experiment no longer carries a hand-written cell binding: its
// shape comes from the verified Contract Index. Cross-check that the contract
// and the frozen scale parser still agree on every cell.
func TestFrozenArtifactCellsMatchTheContractIndex(t *testing.T) {
	runtime, err := finalv5contracts.LoadRuntime()
	if err != nil {
		t.Fatal(err)
	}
	cells, err := runtime.ArtifactCells()
	if err != nil {
		t.Fatal(err)
	}
	if len(cells) != len(artifactPublicationRequirements) || len(cells) != 6 {
		t.Fatalf("contract resolved %d artifact cells", len(cells))
	}
	for _, cell := range cells {
		spec, err := experiment.ParseArtifactScale(cell.Identity.Scale)
		if err != nil {
			t.Fatalf("%s: %v", cell.Identity, err)
		}
		if spec.Rows != cell.ExpectedRows || spec.Columns != cell.ExpectedColumns {
			t.Fatalf("%s contract shape %dx%d differs from the frozen scale parser %dx%d",
				cell.Identity, cell.ExpectedRows, cell.ExpectedColumns, spec.Rows, spec.Columns)
		}
		query, err := runtime.QueryContract(cell)
		if err != nil {
			t.Fatalf("%s: %v", cell.Identity, err)
		}
		if query.BDG.PublicTool != finalv5contracts.PublicBDGTool {
			t.Fatalf("%s does not resolve to the public BDG entrypoint", cell.Identity)
		}
	}
}

func TestFrozenCellBindingsCrossCheckExactScale(t *testing.T) {
	digest := strings.Repeat("b", 64)
	dependency := dependencyCellBinding{
		Task: testBoundTask(4), Candidate: testBoundQuery(10, 4, 10_000),
		History: testBoundQuery(5, 4, 10_000),
		Union: finalv5binding.BoundDependencySetExpectation{
			DependencyFacts: 15_000, DependencySetSHA256: digest,
		},
		OutcomeCandidate: testBoundOutcomeCandidate(t),
	}
	if err := validateDependencyCellBinding("10k-overlap-50", dependency); err != nil {
		t.Fatal(err)
	}
	dependency.History.DependencyFacts = 9_999
	if err := validateDependencyCellBinding("10k-overlap-50", dependency); err == nil {
		t.Fatal("dependency binding with a shrunken existing set was accepted")
	}
	dependency.History.DependencyFacts = 10_000
	dependency.OutcomeCandidate.Members = dependency.OutcomeCandidate.Members[:4]
	if err := validateDependencyCellBinding("10k-overlap-50", dependency); err == nil {
		t.Fatal("dependency binding without the exact Outcome candidate members was accepted")
	}
}

func TestOutcomeOperandGeneratorRealizesRecordedX1Rounding(t *testing.T) {
	spec, err := experiment.ParseOutcomeMerkleScale("10k-x1-o50")
	if err != nil {
		t.Fatal(err)
	}
	operands, err := buildOutcomeOperands(20260801, "10k-x1-o50", spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(operands.root) != 10_000 || len(operands.candidate) != 1 || operands.candidate[0] != operands.root[0] {
		t.Fatalf("x1 nearest-integer operand mismatch: roots=%d candidate=%v", len(operands.root), operands.candidate)
	}
	if !validDigest(operands.fixtureSHA256) || !validDigest(operands.rootOracleSHA256) ||
		!validDigest(operands.candidateOracleSHA256) || !validDigest(operands.unionOracleSHA256) {
		t.Fatal("Outcome operand generator omitted reconstructable oracle digests")
	}
}

func TestAdapterFailureTaxonomyRetainsAttemptedFailureAndInvalidBinding(t *testing.T) {
	operation := experiment.AdapterOperation{SchemaVersion: 1, CampaignClass: "publication", CampaignID: "c",
		DeploymentID: "deployment-01", ExperimentID: "artifact", CellID: "result-heavy/100x4/novel",
		SampleID: "s", Iteration: 1, ProcessReplicate: 1, OrderPosition: 1, RandomSeed: 1,
		PairID: "p", PairedSystemOrder: "novel", RootGroupID: "novel", WorkloadID: "result-heavy",
		Scale: "100x4", Mode: "novel"}
	invalid := invalidSample(operation, "artifact_binding_invalid")
	failed := failedSample(operation, "artifact_measurement_failed")
	if invalid.Status != "invalid" || failed.Status != "fail" || invalid.ErrorCode == failed.ErrorCode ||
		!invalid.PublicationEligible || !failed.PublicationEligible {
		t.Fatalf("invalid=%+v failed=%+v", invalid, failed)
	}
	if err := invalid.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := failed.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRetainTaskGateRejectionDistinguishesFinalizerRefusal(t *testing.T) {
	operation := experiment.AdapterOperation{SchemaVersion: 1, CampaignClass: "pilot", CampaignID: "c",
		DeploymentID: "deployment-01", ExperimentID: "artifact", CellID: "result-heavy/100x4/novel",
		SampleID: "s", Iteration: 1, ProcessReplicate: 1, OrderPosition: 1, RandomSeed: 1,
		PairID: "p", PairedSystemOrder: "novel", RootGroupID: "novel", WorkloadID: "result-heavy",
		Scale: "100x4", Mode: "novel"}
	failed := failedSample(operation, "artifact_measurement_failed")
	failed.TaskGateAcceptanceV3 = &experiment.FinalizationV3{}

	var unopened *experiment.RuntimeFinalizerV3
	_, refusal := unopened.FinalizeTaskGateObservationV3(context.Background(), experiment.FinalizationRequestV3{})
	retained := retainTaskGateRejection(failed, refusal)
	if retained.SchemaVersion != experiment.TaskGateRejectionSampleSchemaVersion ||
		retained.Status != "fail" || retained.TaskGateRejectionV1 == nil ||
		retained.TaskGateAcceptanceV3 != nil {
		t.Fatalf("finalizer refusal did not become an exclusive sample-v2 rejection: %+v", retained)
	}
	if _, ok := experiment.TaskGateRejectionFromError(refusal); !ok {
		t.Fatal("finalizer refusal omitted the typed rejection marker")
	}
	secretSentinels := []string{
		"TASKGATE_FULL_SAMPLE_SENTINEL_7f18c3",
		"postgres://taxonomy:Only-In-Error@private.invalid/evidence",
		"-----BEGIN PRIVATE KEY-----taxonomy-only-----END PRIVATE KEY-----",
	}
	wrapped := errors.Join(errors.New(secretSentinels[0]),
		fmt.Errorf("operator DSN %s and key %s: %w", secretSentinels[1], secretSentinels[2], refusal))
	credentialFree := retainTaskGateRejection(failed, wrapped)
	if err := credentialFree.Validate(); err != nil {
		t.Fatalf("credential-free retained Sample does not validate: %v", err)
	}
	encoded, err := json.Marshal(credentialFree)
	if err != nil {
		t.Fatal(err)
	}
	for _, sentinel := range secretSentinels {
		if strings.Contains(string(encoded), sentinel) {
			t.Fatalf("serialized retained Sample contains error-chain sentinel %q", sentinel)
		}
	}

	ordinary := retainTaskGateRejection(failed, errors.New("failure before finalization"))
	if ordinary.SchemaVersion != experiment.SampleSchemaVersion || ordinary.TaskGateRejectionV1 != nil ||
		ordinary.TaskGateAcceptanceV3 == nil {
		t.Fatalf("pre-finalizer error changed the sample wire state: %+v", ordinary)
	}
}

func TestScaleAndArtifactInvariantFailuresRetainMeasuredEvidence(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, test := range []struct {
		name       string
		experiment string
		validate   func(experiment.Sample) experiment.Sample
		wantCode   string
	}{
		{name: "scale", experiment: "scale", validate: validateScalePass, wantCode: "scale_evidence_invariant_failed"},
		{name: "artifact", experiment: "artifact", validate: validateArtifactPass, wantCode: "artifact_evidence_invariant_failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			operation := experiment.AdapterOperation{SchemaVersion: 1, CampaignClass: "publication", CampaignID: "c",
				DeploymentID: "deployment-01", ExperimentID: test.experiment, CellID: "cell", SampleID: "sample",
				Iteration: 1, ProcessReplicate: 1, OrderPosition: 1, RandomSeed: 1, PairID: "pair",
				PairedSystemOrder: "novel", RootGroupID: "novel", WorkloadID: "mutated", Scale: "bad", Mode: "novel"}
			sample := baseSample(operation, "taskgate")
			sample.Status = "pass"
			sample.ClientFullDrainMS = 7
			sample.RowCount, sample.ColumnCount, sample.ResultSHA256 = 3, 4, digest
			sample.Counters = map[string]int64{"retained_marker": 13}
			if test.experiment == "scale" {
				sample.ScaleVerification = &experiment.ScaleVerificationEvidence{Version: scaleVerificationVersion}
			} else {
				sample.ArtifactVerification = &experiment.ArtifactVerificationEvidence{Version: artifactVerificationVersion}
			}
			failed := test.validate(sample)
			if failed.Status != "fail" || failed.ErrorCode != test.wantCode || failed.ClientFullDrainMS != 7 ||
				failed.RowCount != 3 || failed.ResultSHA256 != digest || failed.Counters["retained_marker"] != 13 ||
				(test.experiment == "scale" && failed.ScaleVerification == nil) ||
				(test.experiment == "artifact" && failed.ArtifactVerification == nil) {
				t.Fatalf("invariant failure discarded measured evidence: %+v", failed)
			}
		})
	}
}

func TestPostAcceptanceV3InvariantFailureSurvivesJSONL(t *testing.T) {
	digest := strings.Repeat("a", 64)
	probe := strings.Repeat("b", 64)
	operation := experiment.AdapterOperation{SchemaVersion: 1, CampaignClass: "publication", CampaignID: "c",
		DeploymentID: "deployment-01", ExperimentID: "artifact", CellID: "result-heavy/100x4/novel",
		SampleID: "sample", Iteration: 1, ProcessReplicate: 1, OrderPosition: 1, RandomSeed: 1,
		PairID: "pair", PairedSystemOrder: "novel", RootGroupID: "novel", WorkloadID: "result-heavy",
		Scale: "100x4", Mode: "novel"}
	sample := baseSample(operation, "taskgate")
	sample.SchemaVersion = experiment.FinalizedSampleSchemaVersion
	sample.Status = "pass"
	sample.ClientFullDrainMS = 7
	sample.RowCount, sample.ColumnCount, sample.ResultSHA256 = 3, 4, digest
	sample.Counters = map[string]int64{"retained_marker": 13}
	sample.TaskGateAcceptanceV3 = &experiment.FinalizationV3{ReceiptSHA256: digest}
	sample.ArtifactVerification = &experiment.ArtifactVerificationEvidence{
		Version: artifactVerificationVersion, BindingSHA256: digest,
		DatasetSHA256: digest, CatalogSHA256: digest, DatasetProbeSHA256: probe, QuerySHA256: digest,
		ExpectedRows: 3, ExpectedColumns: 4, ExpectedResultSHA256: digest,
		ObservedRows: 4, ObservedColumns: 4, ObservedResultSHA256: digest,
	}

	failed := validateArtifactPass(sample)
	if failed.Status != "fail" || failed.ErrorCode != "artifact_evidence_invariant_failed" {
		t.Fatalf("post-acceptance invariant failure was not retained: %+v", failed)
	}
	path := filepath.Join(t.TempDir(), "post-acceptance-v3.jsonl")
	writer, err := experiment.NewJSONLWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Write(failed); err != nil {
		t.Fatalf("JSONL writer would force the runner to replace v3 evidence: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	retained, err := experiment.ReadSamples([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	if len(retained) != 1 || retained[0].SchemaVersion != experiment.FinalizedSampleSchemaVersion ||
		retained[0].Status != "fail" || retained[0].TaskGateAcceptanceV3 == nil ||
		retained[0].ArtifactVerification == nil || retained[0].ArtifactVerification.ObservedRows != 4 ||
		retained[0].Counters["retained_marker"] != 13 || retained[0].ClientFullDrainMS != 7 {
		t.Fatalf("v3 JSONL round trip discarded post-acceptance evidence: %+v", retained)
	}
}

func TestPostAssertionFailuresRetainIndependentRuntimeSnapshots(t *testing.T) {
	digest := strings.Repeat("a", 64)
	operation := experiment.AdapterOperation{SchemaVersion: 1, CampaignClass: "publication", CampaignID: "c",
		DeploymentID: "deployment-01", ExperimentID: "scale", CellID: "cell", SampleID: "sample",
		Iteration: 1, ProcessReplicate: 1, OrderPosition: 1, RandomSeed: 1, PairID: "pair",
		PairedSystemOrder: "novel", RootGroupID: "novel", WorkloadID: "dependency-e2e",
		Scale: "10k-overlap-0", Mode: "novel"}
	businessBefore := experiment.BusinessSQLSnapshot{StatsResetUnixMicro: 100, VisibleCalls: 10, CompanionCalls: 20}
	businessAfter := businessBefore
	businessAfter.VisibleCalls++
	businessAfter.CompanionCalls++
	rootBefore := experiment.RootLedgerSnapshot{RootObservationSetSHA256: digest}
	rootAfter := experiment.RootLedgerSnapshot{Epoch: 1, DependencySetSHA256: digest}
	window := experiment.ObserverWindowV2{
		Before: experiment.ObserverSnapshotV2{Phase: "before", Total: 11},
		After:  experiment.ObserverSnapshotV2{Phase: "after", Total: 29},
	}

	scale := baseSample(operation, "taskgate")
	scale.BusinessSQLDelta = 2
	scale.ScaleVerification = &experiment.ScaleVerificationEvidence{BindingSHA256: digest,
		BusinessBefore: businessBefore, BusinessAfter: businessAfter, RootBefore: rootBefore, RootAfter: rootAfter,
		ObserverWindow: &window}
	failedScale := retainedScaleFailure(operation, scale, "dependency_e2e_measurement_failed")
	if failedScale.ScaleVerification == nil || failedScale.ScaleVerification.ObserverWindow == nil ||
		failedScale.ScaleVerification.ObserverWindow.After.Total != window.After.Total ||
		failedScale.ScaleVerification.BusinessAfter != businessAfter || failedScale.ScaleVerification.RootAfter != rootAfter {
		t.Fatalf("scale post-assertion failure discarded safe snapshots: %+v", failedScale)
	}

	operation.ExperimentID, operation.WorkloadID, operation.Scale = "artifact", "result-heavy", "100x4"
	artifact := baseSample(operation, "taskgate")
	artifact.BusinessSQLDelta = 2
	// The artifact arm retains a V2 window rather than a pair of v1 snapshots
	// since the v3 cutover. What is asserted is unchanged: a post-assertion
	// failure keeps every safely collected boundary instead of discarding it.
	window = experiment.ObserverWindowV2{
		Before: experiment.ObserverSnapshotV2{Phase: "before", Total: 11},
		After:  experiment.ObserverSnapshotV2{Phase: "after", Total: 29},
	}
	artifact.ArtifactVerification = &experiment.ArtifactVerificationEvidence{BindingSHA256: digest,
		BusinessBefore: businessBefore, BusinessAfter: businessAfter, RootBefore: rootBefore, RootAfter: rootAfter,
		ObserverWindow: window}
	failedArtifact := retainedArtifactFailure(operation, artifact, "artifact_measurement_failed")
	if failedArtifact.ArtifactVerification == nil ||
		failedArtifact.ArtifactVerification.ObserverWindow.After.Total != window.After.Total ||
		failedArtifact.ArtifactVerification.BusinessAfter != businessAfter || failedArtifact.ArtifactVerification.RootAfter != rootAfter {
		t.Fatalf("artifact post-assertion failure discarded safe snapshots: %+v", failedArtifact)
	}

	// ProvSQL carries the same pre-registered V2 interval after its v3 cutover.
	// Failure conversion must retain the exact window pointer alongside the
	// independently sampled Business and root boundaries.
	retained := &experiment.ProvSQLVerificationEvidence{BusinessBefore: &businessBefore, BusinessAfter: &businessAfter,
		RootBefore: &rootBefore, RootAfter: &rootAfter, ObserverWindow: &window}
	finalEvidence := &experiment.ProvSQLVerificationEvidence{}
	copyProvSQLTaskGateSnapshots(finalEvidence, retained)
	if finalEvidence.BusinessAfter == nil || finalEvidence.RootAfter == nil || finalEvidence.ObserverWindow == nil ||
		finalEvidence.ObserverWindow.After.Total != window.After.Total {
		t.Fatal("ProvSQL failure conversion discarded independently sampled runtime boundaries")
	}
}

func TestObservedTaskGatePrefixNeverClaimsUnverifiedArtifact(t *testing.T) {
	digest := strings.Repeat("b", 64)
	operation := experiment.AdapterOperation{SchemaVersion: 1, CampaignClass: "publication", CampaignID: "c",
		DeploymentID: "deployment-01", ExperimentID: "artifact", CellID: "cell", SampleID: "sample",
		Iteration: 1, ProcessReplicate: 1, OrderPosition: 1, RandomSeed: 1, PairID: "pair",
		PairedSystemOrder: "novel", RootGroupID: "novel", WorkloadID: "result-heavy", Scale: "100x4", Mode: "novel"}
	response := queryResponse{RowCount: 100, ColumnCount: 4, PlanDigest: digest,
		PipelineMS: map[string]float64{"server_total": 2}, DiagnosticMS: map[string]float64{"observed": 1}}
	response.Exposure.ActualInfluenceFacts = 10
	response.Exposure.InfluenceSetSHA256 = digest
	before := experiment.RootLedgerSnapshot{Epoch: 1, DependencySetSHA256: digest, ReleaseSetSHA256: digest, OutcomeSetSHA256: digest}
	after := before
	after.Epoch = 2
	prefix := observedTaskgateQueryPrefix(operation, "task-id", "SELECT 1", time.Now().Add(-time.Millisecond), 0.5,
		response, before, after)
	failed := retainedArtifactFailure(operation, prefix, "artifact_measurement_failed")
	if failed.Status != "fail" || failed.RowCount != 100 || failed.ColumnCount != 4 ||
		failed.ActualDependencyFacts != 10 || failed.DependencySetSHA256 != digest ||
		failed.ClientFullDrainMS <= 0 || failed.QueryPlanSHA256 != digest || failed.RootEpochAfter != 2 ||
		failed.ReceiptVerified || failed.ArtifactAvailable || failed.ResultSHA256 != "" {
		t.Fatalf("observed-but-unverified TaskGate prefix was lost or overclaimed: %+v", failed)
	}
}

func TestScaleAndArtifactConstructorsSatisfyRealFactoryContract(t *testing.T) {
	// Compile-time contract for the real constructors. Capability registration
	// is owned by the integration gate once implementation tests pass; missing
	// private deployment bindings remain a runtime fail-closed invalid outcome.
	var scaleFactory adapterFactory = newScaleAdapter
	var artifactFactory adapterFactory = newArtifactAdapter
	_ = []adapterFactory{scaleFactory, artifactFactory}
}

func TestAdapterRejectsBindingChangedAfterPreStartFreeze(t *testing.T) {
	binding := finalv5binding.Binding{FileSHA256: strings.Repeat("a", 64), SectionSHA256: strings.Repeat("b", 64)}
	t.Setenv("TASKGATE_FINAL_V5_BINDING_FILE_SHA256", binding.FileSHA256)
	t.Setenv("TASKGATE_FINAL_V5_BINDING_SECTION_SHA256", binding.SectionSHA256)
	if err := validateFrozenAdapterBindingIdentity(binding); err != nil {
		t.Fatal(err)
	}
	binding.FileSHA256 = strings.Repeat("c", 64)
	if err := validateFrozenAdapterBindingIdentity(binding); err == nil {
		t.Fatal("binding bytes changed after pre-start freeze were accepted")
	}
}
