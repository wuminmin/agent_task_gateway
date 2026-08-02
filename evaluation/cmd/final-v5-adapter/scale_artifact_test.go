package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
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

func testBoundQuery(rows int64, columns int, dependencies int64) boundQueryExpectation {
	digest := strings.Repeat("a", 64)
	return boundQueryExpectation{SQL: "SELECT column_a FROM result_heavy ORDER BY column_a LIMIT 100",
		ExpectedRows: rows, ExpectedColumns: columns, ExpectedResultSHA256: digest,
		DependencyFacts: dependencies, DependencySetSHA256: digest}
}

func TestFrozenCellBindingsCrossCheckExactScale(t *testing.T) {
	artifact, err := experiment.ParseArtifactScale("100x4")
	if err != nil {
		t.Fatal(err)
	}
	cell := artifactCellBinding{Task: testBoundTask(4), Query: testBoundQuery(100, 4, 100)}
	if err := validateArtifactCellBinding(artifact, cell); err != nil {
		t.Fatal(err)
	}
	cell.Query.ExpectedRows = 99
	if err := validateArtifactCellBinding(artifact, cell); err == nil {
		t.Fatal("artifact binding with a shrunken row count was accepted")
	}

	dependency := dependencyCellBinding{Task: testBoundTask(4), Candidate: testBoundQuery(10, 4, 10_000),
		History: func() *boundQueryExpectation { value := testBoundQuery(5, 4, 5_000); return &value }()}
	if err := validateDependencyCellBinding("10k-overlap-50", dependency); err != nil {
		t.Fatal(err)
	}
	dependency.History.DependencyFacts = 4_999
	if err := validateDependencyCellBinding("10k-overlap-50", dependency); err == nil {
		t.Fatal("dependency binding with a shrunken overlap was accepted")
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

func TestStrictAdapterBindingSectionRejectsUnknownCredentialField(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	executableSHA, err := experiment.FileSHA256(executable)
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	section := map[string]any{"schema_version": 1, "dataset_probe_sql": "SELECT 'fixture'",
		"observer": map[string]any{"argv": []string{executable, "-test.run=^$"}, "executable_sha256": executableSHA},
		"password": "must-never-be-accepted"}
	top := map[string]any{"dataset_sha256": digest, "catalog_sha256": digest, adapterBindingSectionName: section}
	value, err := json.Marshal(top)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "binding.json")
	if err := os.WriteFile(path, value, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TASKGATE_DATASET_BINDINGS", path)
	if _, err := loadAdapterDeploymentBinding(); err == nil {
		t.Fatal("unknown credential field in strict adapter section was accepted")
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
