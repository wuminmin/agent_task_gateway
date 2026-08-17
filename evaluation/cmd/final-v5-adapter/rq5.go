package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/rq5fixture"
)

const (
	rq5DriverPathEnv       = "TASKGATE_FINAL_V5_RQ5_DRIVER"
	rq5DriverSHAEnv        = "TASKGATE_FINAL_V5_RQ5_DRIVER_SHA256"
	rq5GeneratorSHAEnv     = "TASKGATE_FINAL_V5_RQ5_GENERATOR_SHA256"
	rq5ConfigSHAEnv        = "TASKGATE_FINAL_V5_RQ5_CONFIG_SHA256"
	rq5BuildManifestEnv    = "TASKGATE_FINAL_V5_RQ5_BUILD_MANIFEST"
	rq5BuildManifestSHAEnv = "TASKGATE_FINAL_V5_RQ5_BUILD_MANIFEST_SHA256"
	rq5DriverVersion       = "taskgate-final-v5-rq5-sequential-driver-v1"
	rq5DriverOutputMax     = 16 << 20
)

type rq5CycleBackend interface {
	RunCycle(context.Context, experiment.AdapterOperation, rq5fixture.Cycle) (*experiment.RQ5VerificationEvidence, error)
	Close()
}

type rq5EvidenceValidator func(experiment.Sample) error

type rq5Adapter struct {
	backend               rq5CycleBackend
	validate              rq5EvidenceValidator
	cycles                map[string]*experiment.RQ5VerificationEvidence
	datasetManifestSHA256 string
	runtimeIdentity       string
}

// rq5RunError carries every partially observed real-cycle field across a
// driver failure. It distinguishes an unobservable/invalid environment from a
// measured correctness failure without discarding either one's evidence.
type rq5RunError struct {
	code     string
	invalid  bool
	evidence *experiment.RQ5VerificationEvidence
	cause    error
}

func (err *rq5RunError) Error() string {
	if err == nil {
		return "RQ5 run error"
	}
	if err.cause != nil {
		return err.code + ": " + err.cause.Error()
	}
	return err.code
}

func (err *rq5RunError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.cause
}

// newRQ5Adapter never falls back to historical online-evidence JSON or the
// prohibited four-Service experiment router. It requires the source-built
// sequential driver executable and its deployment-bound SHA-256.
func newRQ5Adapter(ctx context.Context) (*rq5Adapter, error) {
	backend, err := newRQ5DriverBackend(ctx)
	if err != nil {
		return nil, err
	}
	adapter, err := newRQ5AdapterWithBackend(backend, experiment.ValidateRQ5Evidence)
	if err != nil {
		backend.Close()
		return nil, err
	}
	return adapter, nil
}

func newRQ5AdapterWithBackend(backend rq5CycleBackend, validate rq5EvidenceValidator) (*rq5Adapter, error) {
	if backend == nil || validate == nil {
		return nil, errors.New("real RQ5 sequential-cycle backend and strict validator are required")
	}
	if err := rq5fixture.Validate(); err != nil {
		return nil, err
	}
	return &rq5Adapter{backend: backend, validate: validate, cycles: make(map[string]*experiment.RQ5VerificationEvidence)}, nil
}

func (adapter *rq5Adapter) Close() {
	if adapter != nil && adapter.backend != nil {
		adapter.backend.Close()
	}
}

func rq5PairKey(operation experiment.AdapterOperation) string {
	return operation.PairID + "\x00" + operation.RootGroupID
}

func (adapter *rq5Adapter) Execute(ctx context.Context, operation experiment.AdapterOperation) experiment.Sample {
	if adapter == nil || adapter.backend == nil || operation.ExperimentID != "rq5" ||
		!rq5fixture.IsCell(operation.WorkloadID, operation.Scale, operation.Mode, operation.Iteration) {
		return invalidSample(operation, "unsupported_source_controlled_rq5_cell")
	}
	cycle, _ := rq5fixture.LookupCycle(operation.Iteration)
	key := rq5PairKey(operation)
	if operation.Mode == rq5fixture.RetainedMode {
		evidence := adapter.cycles[key]
		if evidence == nil {
			return invalidSample(operation, "rq5_retained_route_lacks_completed_cycle")
		}
		delete(adapter.cycles, key)
		sample := rq5SampleFromEvidence(operation, evidence)
		if err := adapter.validate(sample); err != nil {
			writeAdapterFailureDiagnostic("rq5", operation, err)
			return rq5MeasuredFailure(sample, "rq5_evidence_invariant_failed", false)
		}
		return sample
	}
	if adapter.cycles[key] != nil {
		return invalidSample(operation, "rq5_cycle_anchor_reused")
	}
	evidence, err := adapter.backend.RunCycle(ctx, operation, cycle)
	if err != nil {
		writeAdapterFailureDiagnostic("rq5", operation, err)
		var measured *rq5RunError
		if errors.As(err, &measured) {
			if measured.evidence != nil {
				evidence = measured.evidence
			}
			if evidence != nil {
				sample := rq5SampleFromEvidence(operation, evidence)
				return rq5MeasuredFailure(sample, measured.code, measured.invalid)
			}
			if measured.invalid {
				return invalidSample(operation, measured.code)
			}
			return failedSample(operation, measured.code)
		}
		if evidence != nil {
			return rq5MeasuredFailure(rq5SampleFromEvidence(operation, evidence), "real_rq5_cycle_failed", false)
		}
		return failedSample(operation, "real_rq5_cycle_failed")
	}
	if evidence == nil {
		return failedSample(operation, "rq5_driver_omitted_cycle_evidence")
	}
	sample := rq5SampleFromEvidence(operation, evidence)
	if err := adapter.validate(sample); err != nil {
		writeAdapterFailureDiagnostic("rq5", operation, err)
		return rq5MeasuredFailure(sample, "rq5_evidence_invariant_failed", false)
	}
	if adapter.datasetManifestSHA256 != "" &&
		evidence.DatasetManifestSHA256 != adapter.datasetManifestSHA256 {
		return rq5MeasuredFailure(sample, "rq5_dataset_manifest_changed_across_cycles", false)
	}
	runtimeIdentity := rq5EvidenceRuntimeIdentity(evidence)
	if adapter.runtimeIdentity != "" && runtimeIdentity != adapter.runtimeIdentity {
		return rq5MeasuredFailure(sample, "rq5_runtime_image_changed_across_cycles", false)
	}
	adapter.datasetManifestSHA256 = evidence.DatasetManifestSHA256
	adapter.runtimeIdentity = runtimeIdentity
	adapter.cycles[key] = evidence
	return sample
}

func rq5EvidenceRuntimeIdentity(evidence *experiment.RQ5VerificationEvidence) string {
	if evidence == nil {
		return ""
	}
	return strings.Join([]string{evidence.BuildManifestSHA256, evidence.PhaseImageID,
		evidence.OnlineImageID, evidence.OAImageID, evidence.PhaseBinarySHA256,
		evidence.OnlineBinarySHA256, evidence.OABinarySHA256,
		fmt.Sprintf("%d", evidence.PhaseBinaryMTimeUnix), fmt.Sprintf("%d", evidence.OnlineBinaryMTimeUnix),
		fmt.Sprintf("%d", evidence.OABinaryMTimeUnix)}, "\x00")
}

func rq5MeasuredFailure(sample experiment.Sample, code string, invalid bool) experiment.Sample {
	if sample.SchemaVersion == 0 {
		return sample
	}
	if invalid {
		sample.Status = "invalid"
		sample.Reason = "the real 345,000-row RQ5 cycle could not be observed at its frozen measurement boundary"
	} else {
		sample.Status = "fail"
		sample.Reason = "a real RQ5 build/switch/check/restore cycle was attempted and its retained evidence violated a gate"
	}
	sample.ErrorCode = code
	return sample
}

func rq5SampleFromEvidence(operation experiment.AdapterOperation, evidence *experiment.RQ5VerificationEvidence) experiment.Sample {
	sample := baseSample(operation, "taskgate")
	sample.RQ5Verification = evidence
	if evidence == nil {
		return sample
	}
	query := evidence.Route.NewInitial
	if operation.Mode == rq5fixture.RetainedMode {
		query = evidence.Route.NewRestored
		sample.SemanticReplay = true
		sample.ClientAvailableMS = evidence.Route.FullRouteWallMS
		sample.ClientFullDrainMS = evidence.Route.FullRouteWallMS
	} else {
		sample.ClientAvailableMS = evidence.Build.CycleWallMS
		sample.ClientFullDrainMS = evidence.Build.CycleWallMS
	}
	// A real cycle can fail before every Gateway phase has been observed. Keep
	// the measured non-negative phases, but retain the protocol-required zeroes
	// for phases that were not reached so the runner can persist this partial
	// fail/invalid evidence instead of replacing it with a generic sample.
	sample.PipelineMS = rq5PartialPipeline(query.PipelineMS)
	sample.DiagnosticMS = cloneFloatMap(query.DiagnosticMS)
	sample.Counters = map[string]int64{
		"service_starts": evidence.Topology.ServiceStarts,
		"service_stops":  evidence.Topology.ServiceStops,
		"max_services":   evidence.Topology.MaxConcurrentServices,
	}
	sample.RowCount, sample.ColumnCount, sample.ResultSHA256 = query.RowCount, query.ColumnCount, query.ResultSHA256
	sample.PhysicalSQLSHA256, sample.LogicalSQLSHA256, sample.QueryPlanSHA256 =
		query.PhysicalSQLSHA256, query.LogicalSQLSHA256, query.QueryPlanSHA256
	if query.VerifierManifest != nil {
		sample.ReleaseSetSHA256 = query.VerifierManifest.ReleaseSetSHA256
		sample.DependencySetSHA256 = query.VerifierManifest.DependencySetSHA256
		sample.OutcomeSetSHA256 = query.VerifierManifest.OutcomeSetSHA256
		sample.ArtifactSHA256 = query.VerifierManifest.ReleasedParquetSHA256
		sample.ObjectSHA256 = query.VerifierManifest.CanonicalCiphertextSHA256
	}
	sample.ActualReleaseFacts, sample.ChargedReleaseFacts = query.ActualReleaseFacts, query.ChargedReleaseFacts
	sample.ActualDependencyFacts, sample.ChargedDependencyFacts = query.ActualDependencyFacts, query.ChargedDependencyFacts
	sample.ActualOutcomeFacts, sample.ChargedOutcomeFacts = query.ActualOutcomeFacts, query.ChargedOutcomeFacts
	sample.PredicateAtomCount, sample.CompositeCount = query.PredicateAtomCount, query.CompositeCount
	sample.BusinessSQLDelta = query.BusinessSQLDelta
	sample.RootEpochBefore, sample.RootEpochAfter = query.RootEpochBefore, query.RootEpochAfter
	sample.RootTaskIDHash = query.RootTaskIDHash
	sample.RootSetSHA256Before, sample.RootSetSHA256After = query.RootSetSHA256Before, query.RootSetSHA256After
	sample.ParquetBytes, sample.EncryptedObjectBytes = query.ParquetBytes, query.EncryptedObjectBytes
	sample.ReceiptVersion, sample.ReceiptSHA256 = query.ReceiptVersion, query.ReceiptSHA256
	sample.ArtifactIntentSHA256, sample.AvailabilityAuditSHA256 = query.ArtifactIntentSHA256, query.AvailabilityAuditSHA256
	sample.ReceiptVerified, sample.ArtifactAvailable = query.ReceiptVerified, query.ArtifactAvailable
	sample.Status = "pass"
	return sample
}

func cloneFloatMap(values map[string]float64) map[string]float64 {
	result := make(map[string]float64, len(values))
	for name, value := range values {
		result[name] = value
	}
	return result
}

func rq5PartialPipeline(observed map[string]float64) map[string]float64 {
	result := zeroPipeline()
	for name := range result {
		if value, ok := observed[name]; ok && value >= 0 {
			result[name] = value
		}
	}
	return result
}

type rq5DriverBackend struct {
	path                        string
	expectedSHA256              string
	expectedGeneratorSHA256     string
	expectedConfigSHA256        string
	buildManifestPath           string
	expectedBuildManifestSHA256 string
}

type rq5DriverRequest struct {
	SchemaVersion       int                         `json:"schema_version"`
	DriverVersion       string                      `json:"driver_version"`
	FixtureSHA256       string                      `json:"fixture_sha256"`
	BuildManifestSHA256 string                      `json:"build_manifest_sha256"`
	Operation           experiment.AdapterOperation `json:"operation"`
	CycleIndex          int                         `json:"cycle_index"`
	FromDay             string                      `json:"from_day"`
	ToDay               string                      `json:"to_day"`
	GeneratorSHA256     string                      `json:"generator_sha256"`
	ConfigSHA256        string                      `json:"config_sha256"`
}

type rq5DriverResponse struct {
	SchemaVersion int                                 `json:"schema_version"`
	DriverVersion string                              `json:"driver_version"`
	Status        string                              `json:"status"`
	ErrorCode     string                              `json:"error_code,omitempty"`
	Evidence      *experiment.RQ5VerificationEvidence `json:"evidence,omitempty"`
}

func newRQ5DriverBackend(_ context.Context) (*rq5DriverBackend, error) {
	path := strings.TrimSpace(os.Getenv(rq5DriverPathEnv))
	expectedSHA := strings.TrimSpace(os.Getenv(rq5DriverSHAEnv))
	expectedGeneratorSHA := strings.TrimSpace(os.Getenv(rq5GeneratorSHAEnv))
	expectedConfigSHA := strings.TrimSpace(os.Getenv(rq5ConfigSHAEnv))
	buildManifestPath := strings.TrimSpace(os.Getenv(rq5BuildManifestEnv))
	expectedBuildManifestSHA := strings.TrimSpace(os.Getenv(rq5BuildManifestSHAEnv))
	if !filepath.IsAbs(path) || !validDigest(expectedSHA) || !validDigest(expectedGeneratorSHA) ||
		!validDigest(expectedConfigSHA) || !filepath.IsAbs(buildManifestPath) ||
		!validDigest(expectedBuildManifestSHA) {
		return nil, fmt.Errorf("%s must be absolute and driver/source SHA-256 bindings are required",
			rq5DriverPathEnv)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode()&0o111 == 0 {
		return nil, errors.New("RQ5 sequential driver is absent, non-executable, or a symlink")
	}
	actualSHA, err := experiment.FileSHA256(path)
	if err != nil || actualSHA != expectedSHA {
		return nil, errors.New("RQ5 sequential driver differs from its deployment binding")
	}
	manifestInfo, manifestStatErr := os.Lstat(buildManifestPath)
	manifestSHA, manifestHashErr := experiment.FileSHA256(buildManifestPath)
	if manifestStatErr != nil || !manifestInfo.Mode().IsRegular() || manifestInfo.Mode()&os.ModeSymlink != 0 ||
		manifestHashErr != nil || manifestSHA != expectedBuildManifestSHA {
		return nil, errors.New("RQ5 driver build manifest differs from its deployment binding")
	}
	return &rq5DriverBackend{path: path, expectedSHA256: expectedSHA,
		expectedGeneratorSHA256: expectedGeneratorSHA, expectedConfigSHA256: expectedConfigSHA,
		buildManifestPath: buildManifestPath, expectedBuildManifestSHA256: expectedBuildManifestSHA}, nil
}

func (backend *rq5DriverBackend) Close() {}

func (backend *rq5DriverBackend) RunCycle(ctx context.Context, operation experiment.AdapterOperation,
	cycle rq5fixture.Cycle) (*experiment.RQ5VerificationEvidence, error) {
	if backend == nil || backend.path == "" || !validDigest(backend.expectedSHA256) ||
		!validDigest(backend.expectedGeneratorSHA256) || !validDigest(backend.expectedConfigSHA256) ||
		!filepath.IsAbs(backend.buildManifestPath) || !validDigest(backend.expectedBuildManifestSHA256) {
		return nil, errors.New("RQ5 sequential driver backend is unavailable")
	}
	// Re-attest immediately before every invocation. The campaign sidecar and
	// constructor bind the source-built binary, while this check prevents a
	// path replacement between adapter initialization and a later cycle from
	// silently executing different code.
	info, statErr := os.Lstat(backend.path)
	actualSHA, hashErr := experiment.FileSHA256(backend.path)
	if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode()&0o111 == 0 ||
		hashErr != nil || actualSHA != backend.expectedSHA256 {
		return nil, &rq5RunError{code: "rq5_driver_binding_changed", invalid: true,
			cause: errors.New("RQ5 sequential driver changed after initialization")}
	}
	manifestInfo, manifestStatErr := os.Lstat(backend.buildManifestPath)
	manifestSHA, manifestHashErr := experiment.FileSHA256(backend.buildManifestPath)
	if manifestStatErr != nil || !manifestInfo.Mode().IsRegular() || manifestInfo.Mode()&os.ModeSymlink != 0 ||
		manifestHashErr != nil || manifestSHA != backend.expectedBuildManifestSHA256 {
		return nil, &rq5RunError{code: "rq5_build_manifest_binding_changed", invalid: true,
			cause: errors.New("RQ5 build manifest changed after initialization")}
	}
	request := rq5DriverRequest{SchemaVersion: 1, DriverVersion: rq5DriverVersion,
		FixtureSHA256: rq5fixture.FixtureSHA256(), BuildManifestSHA256: backend.expectedBuildManifestSHA256,
		Operation:  operation,
		CycleIndex: cycle.Index, FromDay: cycle.From, ToDay: cycle.To,
		GeneratorSHA256: backend.expectedGeneratorSHA256, ConfigSHA256: backend.expectedConfigSHA256}
	input, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	stdout, stderr := &boundedRQ5Buffer{maximum: rq5DriverOutputMax}, &boundedRQ5Buffer{maximum: 1 << 20}
	command := exec.CommandContext(ctx, backend.path)
	command.Stdin, command.Stdout, command.Stderr = bytes.NewReader(append(input, '\n')), stdout, stderr
	runErr := command.Run()
	var response rq5DriverResponse
	decodeErr := experiment.StrictJSON(bytes.TrimSpace(stdout.Bytes()), &response)
	if decodeErr != nil {
		return nil, &rq5RunError{code: "rq5_driver_protocol_invalid", invalid: true, cause: decodeErr}
	}
	if response.SchemaVersion != 1 || response.DriverVersion != rq5DriverVersion ||
		(response.Status != "pass" && response.Status != "fail" && response.Status != "invalid") {
		return response.Evidence, &rq5RunError{code: "rq5_driver_protocol_invalid", invalid: true,
			evidence: response.Evidence, cause: errors.New("driver response identity/status is invalid")}
	}
	if runErr != nil || stderr.Len() != 0 || stdout.overflow || stderr.overflow {
		return response.Evidence, &rq5RunError{code: "rq5_driver_process_failed", evidence: response.Evidence, cause: runErr}
	}
	if response.Status != "pass" {
		code := strings.TrimSpace(response.ErrorCode)
		if code == "" {
			code = "rq5_driver_reported_failure"
		}
		return response.Evidence, &rq5RunError{code: code, invalid: response.Status == "invalid", evidence: response.Evidence}
	}
	if response.Evidence == nil {
		return nil, &rq5RunError{code: "rq5_driver_omitted_cycle_evidence", invalid: true}
	}
	return response.Evidence, nil
}

type boundedRQ5Buffer struct {
	buffer   bytes.Buffer
	maximum  int
	overflow bool
}

func (buffer *boundedRQ5Buffer) Write(value []byte) (int, error) {
	if buffer.maximum <= 0 || buffer.buffer.Len()+len(value) > buffer.maximum {
		buffer.overflow = true
		return 0, errors.New("RQ5 driver output exceeded its bound")
	}
	return buffer.buffer.Write(value)
}

func (buffer *boundedRQ5Buffer) Bytes() []byte { return buffer.buffer.Bytes() }
func (buffer *boundedRQ5Buffer) Len() int      { return buffer.buffer.Len() }
