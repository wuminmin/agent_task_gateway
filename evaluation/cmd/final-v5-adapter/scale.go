package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/finalv5contracts"
	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5binding"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

const (
	scaleVerificationVersion           = "taskgate-final-v5-scale-verification-v1"
	scaleDependencyVerificationVersion = "taskgate-final-v5-scale-verification-v2"
	scaleDependencyVerificationV4      = "taskgate-final-v5-scale-verification-v4"
	scaleDependencyVerificationV5      = "taskgate-final-v5-scale-verification-v5"
)

type scaleAdapter struct {
	real *realAdapter

	// Only dependency-e2e is a governed TaskGate arm. Delay construction of its
	// acceptance authority until that arm is selected so Outcome-radix and the
	// kernel/storage microbenchmark do not acquire a Catalog, qualification, or
	// Control-Store dependency they never use.
	finalizerOnce sync.Once
	finalizer     *experiment.RuntimeFinalizerV3
	finalizerErr  error
}

// newScaleAdapter wires the frozen Scale workloads to real TaskGate,
// PostgreSQL Outcome-radix, and production ordinal implementations. Missing
// deployment bindings or backends remain fail-closed at execution time.
func newScaleAdapter(ctx context.Context) (sourceControlledAdapter, error) {
	real, err := newRealAdapter(ctx)
	if err != nil {
		return nil, err
	}
	real.timeout = 30 * time.Minute
	real.http.Timeout = real.timeout
	return &scaleAdapter{real: real}, nil
}

func (adapter *scaleAdapter) Close() { adapter.real.Close() }

func (adapter *scaleAdapter) dependencyFinalizer(ctx context.Context) (*experiment.RuntimeFinalizerV3, error) {
	adapter.finalizerOnce.Do(func() {
		adapter.finalizer, adapter.finalizerErr = experiment.OpenDeploymentFinalizerV3(ctx)
		if adapter.finalizerErr != nil {
			adapter.finalizerErr = fmt.Errorf("open the Scale v3 acceptance authority: %w", adapter.finalizerErr)
		}
	})
	return adapter.finalizer, adapter.finalizerErr
}

func (adapter *scaleAdapter) Execute(ctx context.Context, operation experiment.AdapterOperation) experiment.Sample {
	if operation.ExperimentID != "scale" {
		return invalidSample(operation, "experiment_identity_mismatch")
	}
	switch operation.WorkloadID {
	case "dependency-e2e":
		if _, err := experiment.ParseDependencyScale(operation.Scale); err != nil ||
			(operation.Mode != "novel" && operation.Mode != "semantic_replay") {
			return invalidSample(operation, "unsupported_frozen_scale_cell")
		}
		binding, err := loadAdapterDeploymentBinding()
		if err != nil || binding.Section.Scale == nil {
			return invalidSample(operation, "scale_binding_invalid")
		}
		cell, ok := binding.Section.Scale.DependencyE2E[operation.Scale]
		if !ok || validateDependencyCellBinding(operation.Scale, cell) != nil {
			return invalidSample(operation, "dependency_cell_binding_invalid")
		}
		if operation.Mode == "semantic_replay" {
			state := adapter.real.pairs[scaleStateKey(operation)]
			if state == nil || state.taskID == "" || state.novelObservationSHA256 == "" {
				return invalidSample(operation, "semantic_replay_lacks_novel_anchor")
			}
		}
		sample, err := adapter.executeDependencyE2E(ctx, operation, binding, cell)
		if err != nil {
			writeAdapterFailureDiagnostic("scale", operation, err)
			return retainTaskGateRejection(
				retainedScaleFailure(operation, sample, "dependency_e2e_measurement_failed"), err)
		}
		return validateScalePass(sample)
	case "outcome-merkle":
		if _, err := experiment.ParseOutcomeMerkleScale(operation.Scale); err != nil || operation.Mode != "merkle_control" {
			return invalidSample(operation, "unsupported_frozen_scale_cell")
		}
		binding, err := loadAdapterDeploymentBinding()
		if err != nil || binding.Section.Scale == nil || !binding.Section.Scale.EnableOutcomeMerkle ||
			strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_CONTROL_DSN")) == "" {
			return invalidSample(operation, "outcome_merkle_backend_invalid")
		}
		sample, err := executeOutcomeMerkle(ctx, operation)
		if err != nil {
			writeAdapterFailureDiagnostic("scale", operation, err)
			return retainedScaleFailure(operation, sample, "outcome_merkle_measurement_failed")
		}
		return validateScalePass(sample)
	case "taskgate_scale_extreme":
		if _, err := experiment.ParseExtremeScale(operation.Scale); err != nil || operation.Mode != "kernel_storage_only" {
			return invalidSample(operation, "unsupported_frozen_scale_cell")
		}
		binding, err := loadAdapterDeploymentBinding()
		if err != nil || binding.Section.Scale == nil || !binding.Section.Scale.EnableExtreme {
			return invalidSample(operation, "kernel_storage_binding_invalid")
		}
		sample, err := executeKernelStorage(operation)
		if err != nil {
			writeAdapterFailureDiagnostic("scale", operation, err)
			return retainedScaleFailure(operation, sample, "kernel_storage_measurement_failed")
		}
		return validateScalePass(sample)
	default:
		return invalidSample(operation, "unsupported_frozen_scale_workload")
	}
}

func validateScalePass(sample experiment.Sample) experiment.Sample {
	if sample.Status == "pass" {
		if err := experiment.ValidateScaleEvidence(sample); err != nil {
			writeAdapterSampleFailureDiagnostic("scale", sample, err)
			sample.Status = "fail"
			sample.ErrorCode = "scale_evidence_invariant_failed"
			sample.Reason = "the retained real scale sample failed its independent evidence invariant"
		}
	}
	return sample
}

func retainedScaleFailure(operation experiment.AdapterOperation, sample experiment.Sample, code string) experiment.Sample {
	if sample.SchemaVersion == 0 {
		sample = failedSample(operation, code)
	} else {
		sample.Status = "fail"
		if sample.ErrorCode == "" {
			sample.ErrorCode = code
		}
		if sample.Reason == "" {
			sample.Reason = "a real scale backend operation was attempted and failed; safely collected evidence is retained"
		}
	}
	return sample
}

func scaleStateKey(operation experiment.AdapterOperation) string {
	return operation.PairID + "\x00" + operation.RootGroupID
}

func validateDependencyCellBinding(scale string, cell dependencyCellBinding) error {
	spec, err := experiment.ParseDependencyScale(scale)
	if err != nil || validateBoundTask(cell.Task) != nil || validateBoundQuery(cell.Candidate) != nil ||
		cell.Candidate.DependencyFacts != spec.CandidateFacts || !validDigest(cell.Candidate.DependencySetSHA256) {
		return errors.New("candidate binding differs from frozen dependency scale")
	}
	if validateBoundQuery(cell.History) != nil || cell.History.DependencyFacts != spec.ExistingFacts ||
		!validDigest(cell.History.DependencySetSHA256) {
		return errors.New("history binding differs from the complete frozen existing set")
	}
	if cell.Union.DependencyFacts != spec.UnionFacts || !validDigest(cell.Union.DependencySetSHA256) {
		return errors.New("union binding differs from the frozen dependency set algebra")
	}
	if err := finalv5binding.ValidateBoundOutcomeCandidate(cell.OutcomeCandidate); err != nil {
		return fmt.Errorf("Outcome candidate binding differs from the strict five-member oracle: %w", err)
	}
	return nil
}

func (adapter *scaleAdapter) executeDependencyE2E(ctx context.Context, operation experiment.AdapterOperation,
	binding adapterDeploymentBinding, cell dependencyCellBinding) (experiment.Sample, error) {
	spec, _ := experiment.ParseDependencyScale(operation.Scale)
	profileTask, err := resolveScaleProfileTask(operation, cell.Task)
	if err != nil {
		return experiment.Sample{}, err
	}
	cell.Task = profileTask
	probeDigest, err := adapter.real.verifyDatasetProbe(ctx, binding)
	if err != nil {
		return experiment.Sample{}, err
	}
	key := scaleStateKey(operation)
	state := adapter.real.pairs[key]
	if state == nil {
		state = &pairState{}
		adapter.real.pairs[key] = state
	}
	if operation.Mode == "novel" {
		if state.taskID != "" {
			return experiment.Sample{}, errors.New("novel dependency pair reused a task")
		}
		state.taskID, err = adapter.real.provisionBoundTask(ctx, operation, cell.Task)
		if err != nil {
			return experiment.Sample{}, err
		}
	}
	if state.taskID == "" {
		return experiment.Sample{}, errors.New("dependency query lacks an approved task")
	}

	// The request identity exists before pre-registration: the one-use observer
	// ticket binds this exact task/request attempt, not merely the stable Scale
	// cell. Opening the deployment finalizer here keeps it lazy and leaves both
	// non-TaskGate Scale boundaries independent of v3 deployment material.
	requestID := "final-v5-scale-" + sha(operation.SampleID)[:24]
	finalizer, err := adapter.dependencyFinalizer(ctx)
	if err != nil {
		return experiment.Sample{}, err
	}
	selector := scaleContractSelector(operation)
	registered, err := finalizer.OpenObserverWindowV3(ctx, selector,
		experiment.ObserverAttemptV3{TaskID: state.taskID, RequestID: requestID})
	if err != nil {
		return experiment.Sample{}, err
	}
	// History is setup for the measured candidate, not part of its observer
	// interval. It nevertheless changes the task root and executes Business SQL,
	// so it may happen only after the finalizer has resolved the frozen cell and
	// its deployment profile. The committed exposure-scale profile is deliberately
	// unroutable; OpenObserverWindowV3 rejects it here before prefill can mutate
	// anything. Task provisioning is the sole allowed predecessor because the
	// finalizer-issued ticket must bind a real task/request pair.
	if operation.Mode == "novel" {
		prefillSample, historyLink, prefillErr := adapter.prefillDependencyHistory(
			ctx, finalizer, selector, operation, state, cell.Task, cell.History)
		if prefillErr != nil {
			if prefillSample.SchemaVersion != 0 {
				prefillSample.ErrorCode = "dependency_history_prefill_failed"
				prefillSample.Reason = "the retained rows/columns/result/dependency observation is from the history prefill"
			}
			return prefillSample, prefillErr
		}
		state.historyDependencyLink = historyLink
	}

	beforeRoot, err := adapter.real.rootLedgerSnapshot(ctx, state.taskID)
	if err != nil {
		return experiment.Sample{}, err
	}
	businessBefore, err := adapter.real.businessSQLSnapshotFor(ctx, cell.Task)
	if err != nil {
		return experiment.Sample{}, err
	}
	observerBefore, err := captureBoundObserverV2(ctx, experiment.ObserverInvocationV3{
		Phase: "before", ObserverWindowID: registered.ObserverWindowID,
		ClassifierManifestSHA256: registered.ClassifierManifestSHA256,
	})
	if err != nil {
		return experiment.Sample{}, err
	}
	started := time.Now()
	var response queryResponse
	if err := adapter.real.alice.call(ctx, "query_sql", map[string]any{
		"task_id": state.taskID, "request_id": requestID, "sql": cell.Candidate.SQL,
	}, &response); err != nil {
		return experiment.Sample{}, err
	}
	availableMS := durationMS(time.Since(started))
	partial := observedTaskgateQueryPrefix(operation, state.taskID, cell.Candidate.SQL, started, availableMS,
		response, beforeRoot, beforeRoot)
	businessAfter, err := adapter.real.businessSQLSnapshotFor(ctx, cell.Task)
	if err != nil {
		return partial, err
	}
	afterRoot, err := adapter.real.rootLedgerSnapshot(ctx, state.taskID)
	if err != nil {
		return partial, err
	}
	partial = observedTaskgateQueryPrefix(operation, state.taskID, cell.Candidate.SQL, started, availableMS,
		response, beforeRoot, afterRoot)
	sample, parquetBytes, err := adapter.real.completeTaskgateSampleWithParquet(ctx, operation, state, beforeRoot, afterRoot,
		started, availableMS, cell.Candidate.SQL, response)
	if err != nil {
		return partial, err
	}
	if err := normalizeScaleTaskGateResult(&sample, parquetBytes,
		finalv5oracle.ExposureScaleCandidateResultColumns()); err != nil {
		return sample, err
	}
	window := experiment.ObserverWindowV2{Before: observerBefore}
	evidence := &experiment.ScaleVerificationEvidence{
		Version: scaleDependencyVerificationVersion, Boundary: "dependency_e2e",
		BindingFileSHA256: binding.FileSHA256, BindingSHA256: binding.SectionSHA256,
		DatasetSHA256: binding.DatasetSHA256,
		CatalogSHA256: binding.CatalogSHA256, DatasetProbeSHA256: probeDigest,
		QuerySHA256: sha(cell.Candidate.SQL), ExpectedRows: cell.Candidate.ExpectedRows,
		ExpectedColumns: cell.Candidate.ExpectedColumns, ExpectedResultSHA256: cell.Candidate.ExpectedResultSHA256,
		ExpectedCandidateFacts: spec.CandidateFacts, ObservedCandidateFacts: sample.ActualDependencyFacts,
		ExpectedExistingFacts: spec.ExistingFacts,
		ExpectedOverlapFacts:  spec.OverlapFacts, ObservedOverlapFacts: spec.OverlapFacts,
		ExpectedUnionFacts:        spec.UnionFacts,
		ExistingDependencySHA256:  cell.History.DependencySetSHA256,
		CandidateDependencySHA256: cell.Candidate.DependencySetSHA256,
		UnionDependencySHA256:     cell.Union.DependencySetSHA256,
		BusinessBefore:            businessBefore, BusinessAfter: businessAfter, RootBefore: beforeRoot, RootAfter: afterRoot,
		ObserverWindow: &window,
	}
	if operation.Mode == "semantic_replay" {
		evidence.SourceObservationSHA256 = state.novelObservationSHA256
		evidence.ReplayObservationSHA256 = response.Exposure.ObservationSHA256
	}
	sample.ScaleVerification = evidence
	observerAfter, err := captureBoundObserverV2(ctx, experiment.ObserverInvocationV3{
		Phase: "after", ObserverWindowID: registered.ObserverWindowID,
		ClassifierManifestSHA256: registered.ClassifierManifestSHA256,
	})
	if err != nil {
		return sample, err
	}
	window.After = observerAfter
	resource, err := window.ResourceDelta()
	if err != nil {
		return sample, err
	}
	sample.GatewayMemoryPeakBytes = resource.GatewayMemoryPeakBytes
	sample.GatewayCPUUsecDelta = resource.GatewayCPUUsecDelta
	sample.GatewayNetworkRXDelta = resource.GatewayNetworkRXDelta
	sample.GatewayNetworkTXDelta = resource.GatewayNetworkTXDelta
	sample.ControlWALBytesDelta = resource.ControlWALBytesDelta
	sample.BusinessWALBytesDelta = resource.BusinessWALBytesDelta
	visibleDelta := businessAfter.VisibleCalls - businessBefore.VisibleCalls
	companionDelta := businessAfter.CompanionCalls - businessBefore.CompanionCalls
	sample.BusinessSQLDelta = visibleDelta + companionDelta
	if err := validateBoundScaleSampleResult(sample, cell.Candidate); err != nil {
		return sample, err
	}
	candidateLink, err := verifyScaleDependencySet(ctx, finalizer, selector,
		experiment.DependencyScaleCandidateSummaryRole, sample.DependencySetSHA256)
	if err != nil {
		return sample, err
	}
	var rootBeforeLink, rootAfterLink *experiment.ScaleDependencySetVerificationV1
	if operation.Mode == "novel" {
		rootBeforeLink = state.historyDependencyLink
		if rootBeforeLink == nil || rootBeforeLink.ProductionSetSHA256 != beforeRoot.DependencySetSHA256 {
			return sample, errors.New("history query and RootBefore do not name the same production dependency set")
		}
		rootAfterLink, err = verifyScaleDependencySet(ctx, finalizer, selector,
			experiment.DependencyScaleUnionSummaryRole, afterRoot.DependencySetSHA256)
	} else {
		rootBeforeLink, err = verifyScaleDependencySet(ctx, finalizer, selector,
			experiment.DependencyScaleUnionSummaryRole, beforeRoot.DependencySetSHA256)
		if err == nil {
			if beforeRoot.DependencySetSHA256 != afterRoot.DependencySetSHA256 {
				return sample, errors.New("semantic replay changed the production dependency root")
			}
			rootAfterLink = rootBeforeLink
		}
	}
	if err != nil {
		return sample, err
	}
	if operation.Mode == "novel" {
		evidence.HistoryDependencyLink = state.historyDependencyLink
	}
	evidence.CandidateDependencyLink = candidateLink
	evidence.RootBeforeDependencyLink = rootBeforeLink
	evidence.RootAfterDependencyLink = rootAfterLink
	if sample.BaselineVerification == nil {
		return sample, errors.New("verified Scale sample omitted its receipt evidence")
	}
	carried, err := carriedScaleEvidence(operation.Mode, registered, window,
		sample.BaselineVerification.Receipt)
	if err != nil {
		return sample, err
	}
	finalized, err := finalizer.FinalizeTaskGateObservationV3(ctx, experiment.FinalizationRequestV3{
		Receipt: sample.BaselineVerification.Receipt, Carried: carried, ContractSelector: selector,
		ObserverWindowTicket: registered.ObserverWindowTicket,
	})
	if err != nil {
		return sample, err
	}
	sample.TaskGateAcceptanceV3 = &finalized
	// Finalization has already accepted this operation. Establish the explicit
	// post-acceptance wire before retaining the Scale summary so any subsequent
	// evidence invariant failure cannot be misclassified as pre-finalization.
	sample.SchemaVersion = experiment.FinalizedSampleSchemaVersion
	if err := retainScaleOutcomeCandidateVerification(evidence, finalized); err != nil {
		return sample, err
	}
	if operation.Mode == "novel" {
		state.novelRequestID = requestID
		state.novelQueryID = response.QueryID
		state.novelResultID = response.ResultID
		state.novelObservationSHA256 = response.Exposure.ObservationSHA256
		state.novelGrantSHA256 = response.Receipt.GrantDigest
	}
	return sample, nil
}

func retainScaleOutcomeCandidateVerification(evidence *experiment.ScaleVerificationEvidence,
	finalized experiment.FinalizationV3) error {
	if evidence == nil {
		return errors.New("the accepted Scale operation has no retained Scale evidence")
	}
	verification := finalized.OutcomeCandidateVerification
	if verification == nil {
		return errors.New("the accepted Scale operation omitted its Outcome candidate member verification")
	}
	if err := verification.Validate(); err != nil {
		return fmt.Errorf("validate the accepted Scale Outcome candidate member verification: %w", err)
	}
	evidence.Version = scaleDependencyVerificationV5
	evidence.ExpectedOutcomeMemberCardinality = verification.Expected.Cardinality
	evidence.ObservedOutcomeMemberCardinality = verification.Observed.Cardinality
	evidence.ExpectedOutcomeCandidateSetSHA256 = verification.Expected.OrdinarySetSHA256
	evidence.ObservedOutcomeCandidateSetSHA256 = verification.Observed.OrdinarySetSHA256
	return nil
}

// scaleContractSelector names one frozen dependency-e2e cell in all four
// coordinates. It is only a hint to the finalizer, but pre-registration happens
// before a receipt exists and therefore requires this hint to admit exactly one
// candidate.
func scaleContractSelector(operation experiment.AdapterOperation) experiment.FrozenContractSelectorV3 {
	return experiment.FrozenContractSelectorV3{
		ExperimentID: operation.ExperimentID, WorkloadID: operation.WorkloadID,
		Scale: operation.Scale, Mode: operation.Mode,
	}
}

// carriedScaleEvidence transcribes the pre-registration and the evidence the
// Adapter observed. A novel query carries the two target identities it actually
// executed. A semantic replay carries neither: its signed binding authorizes the
// pair so the finalizer can independently reproduce it, but this request executes
// zero targets and an authorized-only record is not execution evidence.
func carriedScaleEvidence(mode string, registered experiment.PreRegisteredObservationV3,
	window experiment.ObserverWindowV2,
	receipt queryreceipt.QueryReceiptV1) (experiment.CarriedEvidenceV3, error) {
	carried := experiment.CarriedEvidenceV3{
		Arm: experiment.ArmTaskGate, Operation: registered.Operation, Plan: registered.Plan,
		ClassifierManifestSHA256: registered.ClassifierManifestSHA256,
		ClassifierBindingSHA256:  registered.ClassifierBindingSHA256,
		Window:                   window,
	}
	signed := receipt.ExecutionBindingV2
	if signed == nil {
		return experiment.CarriedEvidenceV3{},
			errors.New("the Scale receipt describes no prepared execution")
	}
	if signed.Companion == nil {
		return experiment.CarriedEvidenceV3{},
			errors.New("a dependency query signs no provenance companion")
	}
	switch mode {
	case "novel":
		if !signed.Visible.Executed || !signed.Companion.Executed {
			return experiment.CarriedEvidenceV3{},
				errors.New("a novel dependency query did not execute both signed targets")
		}
		carried.VisibleStatement = signedTargetStatement(signed.Visible)
		carried.CompanionStatement = signedTargetStatement(*signed.Companion)
		carried.VisiblePreparedTargetBindingSHA256 = signed.Visible.PreparedTargetBindingSHA256
		carried.CompanionPreparedTargetBindingSHA256 = signed.Companion.PreparedTargetBindingSHA256
		return carried, nil
	case "semantic_replay":
		if signed.Visible.Executed || signed.Companion.Executed {
			return experiment.CarriedEvidenceV3{},
				errors.New("a semantic replay signed a target as executed by the current request")
		}
		return carried, nil
	default:
		return experiment.CarriedEvidenceV3{}, fmt.Errorf("unsupported Scale execution mode %q", mode)
	}
}

func (adapter *scaleAdapter) prefillDependencyHistory(ctx context.Context,
	finalizer *experiment.RuntimeFinalizerV3, selector experiment.FrozenContractSelectorV3,
	operation experiment.AdapterOperation, state *pairState, task boundTaskRequest,
	history boundQueryExpectation) (experiment.Sample, *experiment.ScaleDependencySetVerificationV1, error) {
	before, err := adapter.real.rootLedgerSnapshot(ctx, state.taskID)
	if err != nil {
		return experiment.Sample{}, nil, err
	}
	if before.Epoch != 0 || before.DependencyCardinality != 0 {
		return experiment.Sample{}, nil, errors.New("dependency prefill did not start from a fresh root")
	}
	businessBefore, err := adapter.real.businessSQLSnapshotFor(ctx, task)
	if err != nil {
		return experiment.Sample{}, nil, err
	}
	requestID := "final-v5-history-" + sha(operation.SampleID)[:24]
	started := time.Now()
	var response queryResponse
	if err := adapter.real.alice.call(ctx, "query_sql", map[string]any{
		"task_id": state.taskID, "request_id": requestID, "sql": history.SQL,
	}, &response); err != nil {
		return experiment.Sample{}, nil, err
	}
	availableMS := durationMS(time.Since(started))
	businessAfter, err := adapter.real.businessSQLSnapshotFor(ctx, task)
	if err != nil {
		return experiment.Sample{}, nil, err
	}
	after, err := adapter.real.rootLedgerSnapshot(ctx, state.taskID)
	if err != nil {
		return experiment.Sample{}, nil, err
	}
	prefillOperation := operation
	prefillOperation.CellID += "/history-prefill"
	prefillOperation.SampleID += "-history-prefill"
	prefillOperation.Mode = "novel"
	sample, parquetBytes, err := adapter.real.completeTaskgateSampleWithParquet(ctx, prefillOperation, state, before, after,
		started, availableMS, history.SQL, response)
	if err != nil {
		return sample, nil, err
	}
	if err := normalizeScaleTaskGateResult(&sample, parquetBytes,
		finalv5oracle.ExposureScaleHistoryResultColumns()); err != nil {
		return sample, nil, err
	}
	// Retain the parent cell identity while keeping the actually observed history
	// result fields. The caller marks this pre-candidate setup failure with its
	// dedicated error code; it is not sample-v3 acceptance evidence.
	sample.ExperimentID, sample.WorkloadID, sample.CellID, sample.SampleID, sample.Mode =
		operation.ExperimentID, operation.WorkloadID, operation.CellID, operation.SampleID, operation.Mode
	link, linkErr := verifyScaleDependencySet(ctx, finalizer, selector,
		experiment.DependencyScaleExistingSummaryRole, sample.DependencySetSHA256)
	resultErr := validateBoundScaleSampleResult(sample, history)
	rootErr := error(nil)
	if after.DependencyCardinality != history.DependencyFacts || after.DependencySetSHA256 != sample.DependencySetSHA256 ||
		businessAfter.VisibleCalls-businessBefore.VisibleCalls != 1 ||
		businessAfter.CompanionCalls-businessBefore.CompanionCalls != 1 {
		rootErr = errors.New("public TaskGate history prefill differs from its result/root transition")
	}
	if err := errors.Join(resultErr, rootErr, linkErr); err != nil {
		return sample, link, err
	}
	return sample, link, nil
}

// normalizeScaleTaskGateResult replaces the legacy JSON-shaped sample digest
// with the frozen typed logical-result identity. This is the same normalizer
// used by Direct/BDG/Parquet agreement; only the query-specific schema comes
// from the independent Scale contract, and no expected value is consumed.
func normalizeScaleTaskGateResult(sample *experiment.Sample, parquetBytes []byte,
	columns []finalv5oracle.ResultColumn) error {
	if sample == nil || sample.BaselineVerification == nil {
		return errors.New("Scale result normalization lacks verified sample evidence")
	}
	observed, err := finalv5contracts.NormalizeBDG(columns,
		finalv5contracts.ParquetInput(bytes.NewReader(parquetBytes), int64(len(parquetBytes))))
	if err != nil {
		return fmt.Errorf("normalize Scale released result: %w", err)
	}
	if observed.Summary.RowCount != sample.RowCount || observed.Summary.ColumnCount != sample.ColumnCount {
		return errors.New("typed Scale result shape differs from the verified artifact intent")
	}
	sample.ResultSHA256 = observed.Summary.CanonicalResultSHA256
	sample.BaselineVerification.ParsedResultSHA256 = observed.Summary.CanonicalResultSHA256
	return nil
}

func validateBoundScaleSampleResult(sample experiment.Sample, expected boundQueryExpectation) error {
	if sample.Status != "pass" || sample.RowCount != expected.ExpectedRows || sample.ColumnCount != expected.ExpectedColumns ||
		sample.ResultSHA256 != expected.ExpectedResultSHA256 || sample.ActualDependencyFacts != expected.DependencyFacts {
		return errors.New("verified TaskGate result differs from its bound rows/columns/result/dependency-cardinality oracle")
	}
	return nil
}

func verifyScaleDependencySet(ctx context.Context, finalizer *experiment.RuntimeFinalizerV3,
	selector experiment.FrozenContractSelectorV3, role experiment.DependencyScaleSummaryRole,
	productionSetSHA256 string) (*experiment.ScaleDependencySetVerificationV1, error) {
	verification, err := finalizer.VerifyScaleDependencySetV1(ctx,
		experiment.ScaleDependencySetVerificationRequestV1{ContractSelector: selector,
			Role: role, ProductionSetSHA256: productionSetSHA256})
	if verification.Version == "" {
		return nil, err
	}
	return &verification, err
}

type outcomeOperands struct {
	root, candidate       []string
	fixtureSHA256         string
	rootOracleSHA256      string
	candidateOracleSHA256 string
	unionOracleSHA256     string
}

func executeOutcomeMerkle(ctx context.Context, operation experiment.AdapterOperation) (experiment.Sample, error) {
	spec, _ := experiment.ParseOutcomeMerkleScale(operation.Scale)
	operands, err := buildOutcomeOperands(operation.RandomSeed, operation.Scale, spec)
	if err != nil {
		return experiment.Sample{}, err
	}
	result, err := control.EvaluateOutcomeRadixPostgresV5(ctx, strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_CONTROL_DSN")),
		control.OutcomeRadixEvaluationRequest{RootHashes: operands.root, CandidateHashes: operands.candidate})
	if err != nil {
		return experiment.Sample{}, err
	}
	var afterMemory runtime.MemStats
	runtime.ReadMemStats(&afterMemory)
	wantNovel := spec.CandidateFacts - spec.OverlapFacts
	loadMS := durationMS(result.Telemetry.LoadDuration)
	differenceMS := durationMS(result.Telemetry.DifferenceUnionDuration)
	persistMS := durationMS(result.Telemetry.PersistDuration)
	totalMS := durationMS(result.MeasuredDuration)
	settlementMS := totalMS - loadMS - differenceMS
	if settlementMS < 0 {
		settlementMS = 0
	}
	sample := baseSample(operation, "taskgate")
	sample.ClientAvailableMS, sample.ClientFullDrainMS = totalMS, totalMS
	sample.PipelineMS["prepare"] = loadMS
	sample.PipelineMS["execute_and_derive"] = differenceMS
	sample.PipelineMS["control_settlement"] = settlementMS
	sample.PipelineMS["server_total"] = loadMS + differenceMS + settlementMS
	sample.DiagnosticMS = map[string]float64{
		"outcome_radix_load": loadMS, "outcome_radix_difference_union": differenceMS,
		"outcome_radix_persist": persistMS,
	}
	heapAllocAfter := int64(afterMemory.HeapAlloc)
	backendIdentity := saltedTaskHash(operation, fmt.Sprintf("control-txid:%d:%s", result.BackendTransactionID, result.RootSetSHA256))
	sample.ResultSHA256 = result.UnionSetSHA256
	sample.ActualOutcomeFacts, sample.ChargedOutcomeFacts = spec.CandidateFacts, result.NovelCardinality
	sample.OutcomeSetSHA256 = result.UnionSetSHA256
	sample.RootSetSHA256Before, sample.RootSetSHA256After = result.RootSetSHA256, result.UnionSetSHA256
	sample.Counters = map[string]int64{
		"blocks_loaded": result.Telemetry.BlocksLoaded, "leaves_loaded": result.Telemetry.LeavesLoaded,
		"hashes_loaded": result.Telemetry.HashesLoaded, "blocks_reused": result.Telemetry.BlocksReused,
		"leaves_changed": result.Telemetry.LeavesChanged, "novelty": result.NovelCardinality,
		"storage_bytes": result.StorageAfter.Bytes, "heap_alloc_bytes_after": heapAllocAfter,
		"replay_changed_objects": result.ReplayChangedObjects,
	}
	sample.ScaleVerification = &experiment.ScaleVerificationEvidence{
		Version: scaleVerificationVersion, Boundary: "outcome_merkle_control",
		OutcomeMerkle: &experiment.OutcomeMerkleEvidence{
			ProductionPath:     "control.differenceAndUnionV5Tx+persistV5SetObjectsTx",
			ContentCachePolicy: "warm_immutable_content_after_fixture_prefill",
			OverlapRounding:    "nearest_integer_half_up", FixtureSHA256: operands.fixtureSHA256,
			BackendRunSHA256: backendIdentity, RootCardinality: spec.RootFacts,
			CandidateCardinality: spec.CandidateFacts, OverlapCardinality: spec.OverlapFacts,
			NovelCardinality: result.NovelCardinality, UnionCardinality: result.UnionCardinality,
			RootMemberOracleSHA256:      operands.rootOracleSHA256,
			CandidateMemberOracleSHA256: operands.candidateOracleSHA256,
			UnionMemberOracleSHA256:     operands.unionOracleSHA256,
			ObservedUnionMemberSHA256:   result.ObservedUnionMemberSHA256,
			ProductionRootSHA256:        result.RootSetSHA256, ProductionUnionSHA256: result.UnionSetSHA256,
			ReplayUnionSHA256: result.ReplayUnionSHA256, BlocksLoaded: result.Telemetry.BlocksLoaded,
			LeavesLoaded: result.Telemetry.LeavesLoaded, HashesLoaded: result.Telemetry.HashesLoaded,
			BlocksReused: result.Telemetry.BlocksReused, LeavesChanged: result.Telemetry.LeavesChanged,
			ChangedObjects: result.ChangedObjects, ReplayChangedObjects: result.ReplayChangedObjects,
			StorageObjectsBefore: result.StorageBefore.Objects, StorageObjectsAfter: result.StorageAfter.Objects,
			StorageBytesBefore: result.StorageBefore.Bytes, StorageBytesAfter: result.StorageAfter.Bytes,
			HeapAllocBytesAfter: heapAllocAfter, LoadMS: loadMS, DifferenceUnionMS: differenceMS, PersistMS: persistMS,
		},
	}
	if result.RootCardinality != spec.RootFacts || result.CandidateCardinality != spec.CandidateFacts ||
		result.NovelCardinality != wantNovel || result.UnionCardinality != spec.RootFacts+wantNovel ||
		result.ReplayUnionSHA256 != result.UnionSetSHA256 ||
		result.ObservedUnionMemberSHA256 != operands.unionOracleSHA256 {
		return sample, errors.New("production Outcome radix result differs from independent cardinality oracle")
	}
	sample.Status = "pass"
	return sample, nil
}

func buildOutcomeOperands(seed int64, scale string, spec experiment.OutcomeMerkleScaleSpec) (outcomeOperands, error) {
	result := outcomeOperands{root: make([]string, spec.RootFacts), candidate: make([]string, 0, spec.CandidateFacts)}
	seen := make(map[string]bool, spec.RootFacts+spec.CandidateFacts)
	for index := int64(0); index < spec.RootFacts; index++ {
		value := deterministicOutcomeHash("root", seed, index)
		if seen[value] {
			return outcomeOperands{}, errors.New("deterministic root fixture collided")
		}
		seen[value] = true
		result.root[index] = value
	}
	for index := int64(0); index < spec.OverlapFacts; index++ {
		result.candidate = append(result.candidate, result.root[index])
	}
	for index := int64(0); int64(len(result.candidate)) < spec.CandidateFacts; index++ {
		value := deterministicOutcomeHash("candidate", seed, index)
		if seen[value] {
			continue
		}
		seen[value] = true
		result.candidate = append(result.candidate, value)
	}
	union := append([]string(nil), result.root...)
	union = append(union, result.candidate[spec.OverlapFacts:]...)
	result.rootOracleSHA256 = ordinaryHashSetDigest(result.root)
	result.candidateOracleSHA256 = ordinaryHashSetDigest(result.candidate)
	result.unionOracleSHA256 = ordinaryHashSetDigest(union)
	result.fixtureSHA256 = sha(strings.Join([]string{"TASKGATE-FINAL-V5-OUTCOME-FIXTURE-V1", strconv.FormatInt(seed, 10),
		scale, result.rootOracleSHA256, result.candidateOracleSHA256, result.unionOracleSHA256}, "\x00"))
	return result, nil
}

func deterministicOutcomeHash(kind string, seed, index int64) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("TASKGATE-FINAL-V5-OUTCOME-MEMBER-V1\x00" + kind + "\x00"))
	var encoded [16]byte
	binary.BigEndian.PutUint64(encoded[:8], uint64(seed))
	binary.BigEndian.PutUint64(encoded[8:], uint64(index))
	_, _ = hash.Write(encoded[:])
	return hex.EncodeToString(hash.Sum(nil))
}

func ordinaryHashSetDigest(values []string) string {
	ordered := append([]string(nil), values...)
	sort.Strings(ordered)
	hash := sha256.New()
	_, _ = hash.Write([]byte("TASKGATE-FINAL-V5-ORDINARY-HASH-SET-ORACLE-V1\x00"))
	for _, value := range ordered {
		_, _ = hash.Write([]byte(value))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func executeKernelStorage(operation experiment.AdapterOperation) (experiment.Sample, error) {
	facts, _ := experiment.ParseExtremeScale(operation.Scale)
	const segmentCount = int64(4)
	dictionaryDigest := sha(strings.Join([]string{"TASKGATE-FINAL-V5-EXTREME-DICTIONARY-V1", operation.CampaignID,
		operation.DeploymentID, operation.SampleID, strconv.FormatInt(operation.RandomSeed, 10)}, "\x00"))
	fixtureSHA := sha(strings.Join([]string{"TASKGATE-FINAL-V5-EXTREME-FIXTURE-V1", operation.Scale,
		strconv.FormatInt(facts, 10), strconv.FormatInt(segmentCount, 10), strconv.FormatInt(operation.RandomSeed, 10)}, "\x00"))
	fixtureStarted := time.Now()
	builder := ordinal.NewBuilder()
	perSegment := (facts + segmentCount - 1) / segmentCount
	for index := int64(0); index < facts; index++ {
		segment := index / perSegment
		ordinalValue := uint32(index % perSegment)
		if err := builder.Add(ordinal.FactRef{DictionaryDigest: dictionaryDigest,
			SegmentID: fmt.Sprintf("dense-%02d", segment), Ordinal: ordinalValue}); err != nil {
			return experiment.Sample{}, err
		}
	}
	candidate, err := builder.Freeze()
	if err != nil || int64(candidate.Cardinality()) != facts {
		return experiment.Sample{}, errors.New("extreme candidate fixture cardinality mismatch")
	}
	empty, err := ordinal.NewBitmapSet()
	if err != nil {
		return experiment.Sample{}, err
	}
	fixtureMS := durationMS(time.Since(fixtureStarted))
	var memoryBefore runtime.MemStats
	runtime.ReadMemStats(&memoryBefore)
	measuredStarted := time.Now()
	differenceStarted := time.Now()
	difference := candidate.Difference(empty)
	differenceMS := durationMS(time.Since(differenceStarted))
	unionStarted := time.Now()
	union := empty.Union(candidate)
	unionMS := durationMS(time.Since(unionStarted))
	cardinalityStarted := time.Now()
	differenceCardinality, unionCardinality := difference.Cardinality(), union.Cardinality()
	cardinalityMS := durationMS(time.Since(cardinalityStarted))
	availableMS := durationMS(time.Since(measuredStarted))
	storageStarted := time.Now()
	storageMS := float64(0)
	storageBytes, containerCount := int64(0), int64(0)
	retainedKernelPrefix := func() experiment.Sample {
		sample := baseSample(operation, "taskgate")
		sample.KernelOnly = true
		sample.GenerationBoundaryMS = fixtureMS
		sample.ClientAvailableMS = availableMS
		sample.ClientFullDrainMS = durationMS(time.Since(measuredStarted))
		sample.PipelineMS["execute_and_derive"] = differenceMS + unionMS + cardinalityMS
		sample.PipelineMS["artifact_stage"] = storageMS
		sample.PipelineMS["server_total"] = sample.PipelineMS["execute_and_derive"] + storageMS
		sample.DiagnosticMS = map[string]float64{"bitmap_difference": differenceMS, "bitmap_union": unionMS,
			"bitmap_cardinality": cardinalityMS, "portable_container_round_trip": storageMS}
		sample.Counters = map[string]int64{"candidate_facts": facts, "difference_facts": int64(differenceCardinality),
			"union_facts": int64(unionCardinality), "segments": int64(len(union.SegmentBounds())),
			"containers": containerCount, "storage_bytes": storageBytes}
		sample.ActualDependencyFacts, sample.ChargedDependencyFacts = facts, facts
		return sample
	}
	portable, err := union.PortableContainers()
	if err != nil {
		storageMS = durationMS(time.Since(storageStarted))
		return retainedKernelPrefix(), err
	}
	containerCount = int64(len(portable))
	for _, container := range portable {
		storageBytes += int64(len(container.Bitmap))
	}
	roundTrip, err := ordinal.ParsePortableContainers(portable)
	if err != nil {
		storageMS = durationMS(time.Since(storageStarted))
		return retainedKernelPrefix(), err
	}
	storageMS = durationMS(time.Since(storageStarted))
	fullMS := durationMS(time.Since(measuredStarted))
	candidateDigest, err := candidate.Digest()
	if err != nil {
		return retainedKernelPrefix(), err
	}
	differenceDigest, err := difference.Digest()
	if err != nil {
		return retainedKernelPrefix(), err
	}
	unionDigest, err := union.Digest()
	if err != nil {
		return retainedKernelPrefix(), err
	}
	roundTripDigest, err := roundTrip.Digest()
	if err != nil {
		partial := retainedKernelPrefix()
		partial.ResultSHA256 = unionDigest
		partial.DependencySetSHA256 = unionDigest
		return partial, err
	}
	var memoryAfter runtime.MemStats
	runtime.ReadMemStats(&memoryAfter)
	allocated := int64(memoryAfter.TotalAlloc - memoryBefore.TotalAlloc)
	allocations := int64(memoryAfter.Mallocs - memoryBefore.Mallocs)
	heapAllocAfter := int64(memoryAfter.HeapAlloc)
	runIdentity := saltedTaskHash(operation, unionDigest)
	sample := baseSample(operation, "taskgate")
	sample.KernelOnly = true
	sample.GenerationBoundaryMS = fixtureMS
	sample.ClientAvailableMS, sample.ClientFullDrainMS = availableMS, fullMS
	sample.PipelineMS["execute_and_derive"] = differenceMS + unionMS + cardinalityMS
	sample.PipelineMS["artifact_stage"] = storageMS
	sample.PipelineMS["server_total"] = sample.PipelineMS["execute_and_derive"] + storageMS
	sample.DiagnosticMS = map[string]float64{"bitmap_difference": differenceMS, "bitmap_union": unionMS,
		"bitmap_cardinality": cardinalityMS, "portable_container_round_trip": storageMS}
	sample.Counters = map[string]int64{"candidate_facts": facts, "difference_facts": int64(differenceCardinality),
		"union_facts": int64(unionCardinality), "segments": int64(len(union.SegmentBounds())),
		"containers": int64(len(portable)), "storage_bytes": storageBytes, "alloc_bytes": allocated,
		"alloc_objects": allocations, "heap_alloc_bytes_after": heapAllocAfter}
	sample.ResultSHA256 = unionDigest
	sample.ActualDependencyFacts, sample.ChargedDependencyFacts = facts, facts
	sample.DependencySetSHA256 = unionDigest
	emptyDigest, err := empty.Digest()
	if err != nil {
		return sample, err
	}
	sample.RootSetSHA256Before, sample.RootSetSHA256After = emptyDigest, unionDigest
	sample.ScaleVerification = &experiment.ScaleVerificationEvidence{Version: scaleVerificationVersion,
		Boundary: "kernel_storage_only", KernelStorage: &experiment.KernelStorageEvidence{
			ProductionPath: "ordinal.BitmapSet.Difference+Union+PortableContainers", FixtureSHA256: fixtureSHA,
			RunIdentitySHA256: runIdentity, ExpectedCardinality: facts, CandidateCardinality: facts,
			DifferenceCardinality: int64(differenceCardinality), UnionCardinality: int64(unionCardinality),
			CandidateSHA256: candidateDigest, DifferenceSHA256: differenceDigest, UnionSHA256: unionDigest,
			RoundTripSHA256: roundTripDigest, SegmentCount: int64(len(union.SegmentBounds())),
			ContainerCount: int64(len(portable)), StorageBytes: storageBytes, AllocatedBytes: allocated,
			Allocations: allocations, HeapAllocBytesAfter: heapAllocAfter, DifferenceMS: differenceMS, UnionMS: unionMS,
			CardinalityMS: cardinalityMS, StorageRoundTripMS: storageMS,
		}}
	if differenceCardinality != uint64(facts) || unionCardinality != uint64(facts) ||
		candidateDigest != differenceDigest || candidateDigest != unionDigest || unionDigest != roundTripDigest ||
		storageBytes <= 0 || len(portable) == 0 {
		return sample, errors.New("production ordinal algebra/storage round trip differs from exact dense oracle")
	}
	sample.Status = "pass"
	return sample, nil
}
