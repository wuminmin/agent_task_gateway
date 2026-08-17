package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/rq5fixture"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

type fakeRQ5Backend struct {
	evidence *experiment.RQ5VerificationEvidence
	err      error
	runs     int
	closed   bool
}

func (backend *fakeRQ5Backend) RunCycle(_ context.Context, _ experiment.AdapterOperation,
	_ rq5fixture.Cycle) (*experiment.RQ5VerificationEvidence, error) {
	backend.runs++
	return backend.evidence, backend.err
}

func (backend *fakeRQ5Backend) Close() { backend.closed = true }

func rq5AdapterTestOperation(mode string) experiment.AdapterOperation {
	return experiment.AdapterOperation{
		SchemaVersion: 1, CampaignClass: "publication", CampaignID: "campaign", DeploymentID: "deployment-01",
		ExperimentID: "rq5", CellID: rq5fixture.WorkloadID + "/" + rq5fixture.Scale + "/" + mode,
		SampleID: "sample-" + mode, Iteration: 1, ProcessReplicate: 1, OrderPosition: 1, RandomSeed: 7,
		PairID: "pair", PairedSystemOrder: rq5fixture.BuildMode + "," + rq5fixture.RetainedMode,
		FreshRootRequired: mode == rq5fixture.BuildMode,
		RootGroupID:       rq5fixture.BuildMode + "," + rq5fixture.RetainedMode,
		WorkloadID:        rq5fixture.WorkloadID, Scale: rq5fixture.Scale, Mode: mode,
	}
}

func minimalRQ5AdapterEvidence() *experiment.RQ5VerificationEvidence {
	pipeline := map[string]float64{"prepare": 1, "execute_and_derive": 1, "artifact_stage": 1,
		"control_settlement": 1, "artifact_publication": 1, "response_finalize": 1, "server_total": 6}
	query := func(replay bool, base string) experiment.RQ5QueryEvidence {
		manifest := &experiment.RedactedVerifierManifest{
			ReleaseSetSHA256: sha(base + "-release"), DependencySetSHA256: sha(base + "-dependency"),
			OutcomeSetSHA256: sha(base + "-outcome"), ReleasedParquetSHA256: sha(base + "-parquet"),
			CanonicalCiphertextSHA256: sha(base + "-object"),
		}
		business := int64(1)
		if replay {
			business = 0
		}
		return experiment.RQ5QueryEvidence{
			RootTaskIDHash: sha("root"), ResultSHA256: sha(base + "-result"), RowCount: 5, ColumnCount: 3,
			PipelineMS: pipeline, DiagnosticMS: map[string]float64{}, PhysicalSQLSHA256: sha("physical"),
			LogicalSQLSHA256: sha("logical"), QueryPlanSHA256: sha("plan"),
			ActualReleaseFacts: 1, ChargedReleaseFacts: 1, ActualDependencyFacts: 1, ChargedDependencyFacts: 1,
			ActualOutcomeFacts: 1, ChargedOutcomeFacts: 1, PredicateAtomCount: 1, CompositeCount: 1,
			BusinessSQLDelta: business, RootSetSHA256Before: sha("before"), RootSetSHA256After: sha("after"),
			ParquetBytes: 10, EncryptedObjectBytes: 20, ReceiptVersion: queryreceipt.Version, ReceiptSHA256: sha(base + "-receipt"),
			ArtifactIntentSHA256: sha(base + "-intent"), AvailabilityAuditSHA256: sha(base + "-availability"),
			ReceiptVerified: true, ArtifactAvailable: true, VerifierManifest: manifest,
		}
	}
	return &experiment.RQ5VerificationEvidence{
		BuildManifestSHA256: sha("build-manifest"),
		PhaseImageID:        "sha256:" + sha("phase-image"),
		OnlineImageID:       "sha256:" + sha("runtime-image"),
		OAImageID:           "sha256:" + sha("runtime-image"),
		PhaseBinarySHA256:   sha("phase-binary"),
		OnlineBinarySHA256:  sha("online-binary"),
		OABinarySHA256:      sha("oa-binary"),
		Build:               experiment.RQ5BuildEvidence{CycleWallMS: 30},
		Topology:            experiment.RQ5TopologyEvidence{ServiceStarts: 4, ServiceStops: 4, MaxConcurrentServices: 1},
		Route: experiment.RQ5RouteEvidence{FullRouteWallMS: 20,
			NewInitial: query(false, "initial"), NewRestored: query(true, "restored")},
	}
}

func TestRQ5AdapterPairsOneRealCycleAcrossModes(t *testing.T) {
	backend := &fakeRQ5Backend{evidence: minimalRQ5AdapterEvidence()}
	validations := 0
	adapter, err := newRQ5AdapterWithBackend(backend, func(sample experiment.Sample) error {
		validations++
		if sample.RQ5Verification != backend.evidence || sample.RootTaskIDHash != sha("root") {
			return errors.New("sample did not retain the cycle evidence")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	build := adapter.Execute(t.Context(), rq5AdapterTestOperation(rq5fixture.BuildMode))
	if build.Status != "pass" || build.SemanticReplay || backend.runs != 1 {
		t.Fatalf("build = %#v, runs=%d", build, backend.runs)
	}
	routeOperation := rq5AdapterTestOperation(rq5fixture.RetainedMode)
	routeOperation.OrderPosition = 2
	route := adapter.Execute(t.Context(), routeOperation)
	if route.Status != "pass" || !route.SemanticReplay || backend.runs != 1 ||
		route.RootTaskIDHash != build.RootTaskIDHash || validations != 2 {
		t.Fatalf("route = %#v, build root=%s runs=%d validations=%d", route, build.RootTaskIDHash, backend.runs, validations)
	}
	second := adapter.Execute(t.Context(), routeOperation)
	if second.Status != "invalid" || second.ErrorCode != "rq5_retained_route_lacks_completed_cycle" {
		t.Fatalf("consumed cycle was reused: %#v", second)
	}
	adapter.Close()
	if !backend.closed {
		t.Fatal("backend was not closed")
	}
}

func TestRQ5AdapterRetainsMeasuredFailuresAndInvalids(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		status  string
		code    string
		invalid bool
	}{
		{name: "fail", status: "fail", code: "switch_failed"},
		{name: "invalid", status: "invalid", code: "service_slot_not_observable", invalid: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			diagnostics := captureAdapterDiagnostics(t)
			evidence := minimalRQ5AdapterEvidence()
			backend := &fakeRQ5Backend{evidence: evidence}
			backend.err = &rq5RunError{code: test.code, invalid: test.invalid, evidence: evidence, cause: errors.New("real driver")}
			adapter, err := newRQ5AdapterWithBackend(backend, func(experiment.Sample) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			sample := adapter.Execute(t.Context(), rq5AdapterTestOperation(rq5fixture.BuildMode))
			if sample.Status != test.status || sample.ErrorCode != test.code || sample.RQ5Verification != evidence {
				t.Fatalf("measured outcome was not retained: %#v", sample)
			}
			if strings.Contains(sample.Reason, "real driver") {
				t.Fatalf("RQ5 driver cause leaked into the sample: %q", sample.Reason)
			}
			if got := diagnostics.String(); !strings.Contains(got, "real driver") {
				t.Fatalf("RQ5 driver cause was not retained in adapter stderr: %q", got)
			}
		})
	}
}

func TestRQ5AdapterRetainsEarlyPartialFailureWithSchemaSafePipeline(t *testing.T) {
	tests := []struct {
		name     string
		observed map[string]float64
	}{
		{name: "no query phases observed", observed: nil},
		{name: "some query phases observed", observed: map[string]float64{
			"prepare": 1.25, "execute_and_derive": 2.5, "artifact_stage": -1,
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := &experiment.RQ5VerificationEvidence{
				Version: "partial-real-cycle",
				Build:   experiment.RQ5BuildEvidence{CycleWallMS: 3.75},
				Route: experiment.RQ5RouteEvidence{NewInitial: experiment.RQ5QueryEvidence{
					PipelineMS: test.observed,
				}},
			}
			backend := &fakeRQ5Backend{evidence: evidence, err: &rq5RunError{
				code: "rq5_injected_early_failure", evidence: evidence, cause: errors.New("injected real-cycle failure"),
			}}
			adapter, err := newRQ5AdapterWithBackend(backend, func(experiment.Sample) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			sample := adapter.Execute(t.Context(), rq5AdapterTestOperation(rq5fixture.BuildMode))
			if sample.Status != "fail" || sample.ErrorCode != "rq5_injected_early_failure" ||
				sample.RQ5Verification != evidence {
				t.Fatalf("partial failure was altered: %#v", sample)
			}
			for _, phase := range []string{"prepare", "execute_and_derive", "artifact_stage",
				"control_settlement", "artifact_publication", "response_finalize", "server_total"} {
				if value, ok := sample.PipelineMS[phase]; !ok || value < 0 {
					t.Fatalf("schema-safe pipeline omitted %s: %#v", phase, sample.PipelineMS)
				}
			}
			if test.observed != nil && (sample.PipelineMS["prepare"] != 1.25 ||
				sample.PipelineMS["execute_and_derive"] != 2.5 || sample.PipelineMS["artifact_stage"] != 0) {
				t.Fatalf("partial phase overlay = %#v", sample.PipelineMS)
			}
			if err := sample.Validate(); err != nil {
				t.Fatalf("partial failure cannot cross the runner boundary: %v", err)
			}

			path := filepath.Join(t.TempDir(), "raw.jsonl")
			writer, err := experiment.NewJSONLWriter(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := writer.Write(sample); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			retained, err := experiment.ReadSamples([]string{path})
			if err != nil {
				t.Fatal(err)
			}
			if len(retained) != 1 || retained[0].ErrorCode != sample.ErrorCode ||
				retained[0].RQ5Verification == nil || retained[0].RQ5Verification.Version != evidence.Version {
				t.Fatalf("raw evidence did not retain partial RQ5 failure: %#v", retained)
			}
		})
	}
}

func TestRQ5AdapterConvertsPassInvariantViolationToRetainedFail(t *testing.T) {
	evidence := minimalRQ5AdapterEvidence()
	backend := &fakeRQ5Backend{evidence: evidence}
	adapter, err := newRQ5AdapterWithBackend(backend, func(experiment.Sample) error { return errors.New("mutation") })
	if err != nil {
		t.Fatal(err)
	}
	sample := adapter.Execute(t.Context(), rq5AdapterTestOperation(rq5fixture.BuildMode))
	if sample.Status != "fail" || sample.ErrorCode != "rq5_evidence_invariant_failed" || sample.RQ5Verification != evidence {
		t.Fatalf("validator failure was not retained: %#v", sample)
	}
}

func TestRQ5AdapterRejectsDatasetManifestChangeAcrossCycles(t *testing.T) {
	firstEvidence := minimalRQ5AdapterEvidence()
	firstEvidence.DatasetManifestSHA256 = sha("dataset-one")
	backend := &fakeRQ5Backend{evidence: firstEvidence}
	adapter, err := newRQ5AdapterWithBackend(backend, func(experiment.Sample) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if sample := adapter.Execute(t.Context(), rq5AdapterTestOperation(rq5fixture.BuildMode)); sample.Status != "pass" {
		t.Fatalf("first cycle = %#v", sample)
	}
	secondEvidence := minimalRQ5AdapterEvidence()
	secondEvidence.DatasetManifestSHA256 = sha("dataset-two")
	backend.evidence = secondEvidence
	second := rq5AdapterTestOperation(rq5fixture.BuildMode)
	second.Iteration = 2
	second.PairID = "pair-two"
	second.RootGroupID = "root-two"
	second.SampleID = "sample-two"
	if sample := adapter.Execute(t.Context(), second); sample.Status != "fail" ||
		sample.ErrorCode != "rq5_dataset_manifest_changed_across_cycles" || sample.RQ5Verification != secondEvidence {
		t.Fatalf("changed dataset evidence was not retained: %#v", sample)
	}
}

func TestRQ5AdapterRejectsRuntimeImageChangeAcrossCycles(t *testing.T) {
	firstEvidence := minimalRQ5AdapterEvidence()
	firstEvidence.DatasetManifestSHA256 = sha("dataset")
	backend := &fakeRQ5Backend{evidence: firstEvidence}
	adapter, err := newRQ5AdapterWithBackend(backend, func(experiment.Sample) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if sample := adapter.Execute(t.Context(), rq5AdapterTestOperation(rq5fixture.BuildMode)); sample.Status != "pass" {
		t.Fatalf("first cycle = %#v", sample)
	}
	secondEvidence := minimalRQ5AdapterEvidence()
	secondEvidence.DatasetManifestSHA256 = firstEvidence.DatasetManifestSHA256
	secondEvidence.OnlineImageID = "sha256:" + sha("replacement-runtime-image")
	secondEvidence.OAImageID = secondEvidence.OnlineImageID
	backend.evidence = secondEvidence
	second := rq5AdapterTestOperation(rq5fixture.BuildMode)
	second.Iteration = 2
	second.PairID = "pair-two"
	second.RootGroupID = "root-two"
	second.SampleID = "sample-two"
	if sample := adapter.Execute(t.Context(), second); sample.Status != "fail" ||
		sample.ErrorCode != "rq5_runtime_image_changed_across_cycles" || sample.RQ5Verification != secondEvidence {
		t.Fatalf("changed runtime image evidence was not retained: %#v", sample)
	}
}

func TestRQ5AdapterRejectsUnsupportedAndMissingAnchorWithoutRunning(t *testing.T) {
	backend := &fakeRQ5Backend{evidence: minimalRQ5AdapterEvidence()}
	adapter, err := newRQ5AdapterWithBackend(backend, func(experiment.Sample) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	unsupported := rq5AdapterTestOperation(rq5fixture.BuildMode)
	unsupported.Scale = "2000"
	if sample := adapter.Execute(t.Context(), unsupported); sample.Status != "invalid" {
		t.Fatalf("unsupported cell = %#v", sample)
	}
	if sample := adapter.Execute(t.Context(), rq5AdapterTestOperation(rq5fixture.RetainedMode)); sample.Status != "invalid" || sample.ErrorCode != "rq5_retained_route_lacks_completed_cycle" {
		t.Fatalf("missing anchor = %#v", sample)
	}
	if backend.runs != 0 {
		t.Fatalf("unsupported operations ran backend %d times", backend.runs)
	}
}

func TestRQ5AdapterRequiresBackendAndValidator(t *testing.T) {
	if _, err := newRQ5AdapterWithBackend(nil, func(experiment.Sample) error { return nil }); err == nil {
		t.Fatal("nil backend accepted")
	}
	if _, err := newRQ5AdapterWithBackend(&fakeRQ5Backend{}, nil); err == nil {
		t.Fatal("nil validator accepted")
	}
}

func TestRQ5DriverBackendReattestsBinaryBeforeEveryCycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rq5-driver")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' '{}'"), 0o700); err != nil {
		t.Fatal(err)
	}
	digest, err := experiment.FileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(rq5DriverPathEnv, path)
	t.Setenv(rq5DriverSHAEnv, digest)
	t.Setenv(rq5GeneratorSHAEnv, sha("generator"))
	t.Setenv(rq5ConfigSHAEnv, sha("config"))
	manifestPath := filepath.Join(t.TempDir(), "rq5-driver-build-manifest.json")
	if err := os.WriteFile(manifestPath, []byte("sealed manifest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifestSHA, err := experiment.FileSHA256(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(rq5BuildManifestEnv, manifestPath)
	t.Setenv(rq5BuildManifestSHAEnv, manifestSHA)
	backend, err := newRQ5DriverBackend(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 23\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = backend.RunCycle(t.Context(), rq5AdapterTestOperation(rq5fixture.BuildMode),
		rq5fixture.Cycle{Index: 1, From: "day3", To: "day0"})
	var measured *rq5RunError
	if !errors.As(err, &measured) || measured.code != "rq5_driver_binding_changed" || !measured.invalid {
		t.Fatalf("replaced driver error = %#v", err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' '{}'"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte("replaced manifest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = backend.RunCycle(t.Context(), rq5AdapterTestOperation(rq5fixture.BuildMode),
		rq5fixture.Cycle{Index: 1, From: "day3", To: "day0"})
	measured = nil
	if !errors.As(err, &measured) || measured.code != "rq5_build_manifest_binding_changed" || !measured.invalid {
		t.Fatalf("replaced build manifest error = %#v", err)
	}
}

func TestRQ5DriverBackendBuildsCampaignRequestAndRetainsDriverStderrCause(t *testing.T) {
	directory := t.TempDir()
	capture := filepath.Join(directory, "request.json")
	driver := filepath.Join(directory, "rq5-driver")
	script := `#!/bin/sh
IFS= read -r request
printf '%s\n' "$request" > "$RQ5_REQUEST_CAPTURE"
printf '%s\n' '{"schema_version":1,"driver_version":"taskgate-final-v5-rq5-sequential-driver-v1","status":"invalid","error_code":"rq5_driver_environment_invalid"}'
printf '%s\n' 'RQ5 project prefix does not bind the complete campaign/deployment identity' >&2
`
	if err := os.WriteFile(driver, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	driverSHA, err := experiment.FileSHA256(driver)
	if err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(directory, "rq5-driver-build-manifest.json")
	if err := os.WriteFile(manifest, []byte("sealed manifest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifestSHA, err := experiment.FileSHA256(manifest)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RQ5_REQUEST_CAPTURE", capture)
	backend := &rq5DriverBackend{path: driver, expectedSHA256: driverSHA,
		expectedGeneratorSHA256: sha("campaign-generator"), expectedConfigSHA256: sha("campaign-config"),
		buildManifestPath: manifest, expectedBuildManifestSHA256: manifestSHA}
	adapter, err := newRQ5AdapterWithBackend(backend, func(experiment.Sample) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	diagnostics := captureAdapterDiagnostics(t)
	operation := rq5AdapterTestOperation(rq5fixture.BuildMode)
	operation.CampaignClass = "pilot"
	operation.CampaignID = "p34-mech-partial-04"
	operation.DeploymentID = "deployment-01"
	sample := adapter.Execute(t.Context(), operation)
	if sample.Status != "invalid" || sample.ErrorCode != "rq5_driver_environment_invalid" {
		t.Fatalf("driver response semantics changed: %#v", sample)
	}
	if got := diagnostics.String(); !strings.Contains(got, "project prefix does not bind") {
		t.Fatalf("driver stderr cause did not reach Adapter stderr: %q", got)
	}
	payload, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	var request rq5DriverRequest
	if err := experiment.StrictJSON(payload, &request); err != nil {
		t.Fatal(err)
	}
	if request.SchemaVersion != 1 || request.DriverVersion != rq5DriverVersion ||
		request.FixtureSHA256 != rq5fixture.FixtureSHA256() || request.BuildManifestSHA256 != manifestSHA ||
		request.GeneratorSHA256 != sha("campaign-generator") || request.ConfigSHA256 != sha("campaign-config") ||
		request.Operation.CampaignClass != "pilot" || request.Operation.CampaignID != "p34-mech-partial-04" ||
		request.Operation.DeploymentID != "deployment-01" || request.Operation.ExperimentID != "rq5" ||
		request.Operation.WorkloadID != rq5fixture.WorkloadID || request.Operation.Scale != rq5fixture.Scale ||
		request.Operation.Mode != rq5fixture.BuildMode || request.Operation.Iteration != 1 ||
		request.CycleIndex != 1 || request.FromDay != "day3" || request.ToDay != "day0" {
		t.Fatalf("campaign-shaped driver request is incomplete: %#v", request)
	}
}
