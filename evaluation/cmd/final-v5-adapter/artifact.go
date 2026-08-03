package main

import (
	"context"
	"errors"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

const artifactVerificationVersion = "taskgate-final-v5-artifact-verification-v1"

type artifactAdapter struct{ real *realAdapter }

// newArtifactAdapter wires all six frozen result-heavy cells to the real
// TaskGate/released-artifact path. Missing deployment bindings or backends
// remain fail-closed at execution time.
func newArtifactAdapter(ctx context.Context) (sourceControlledAdapter, error) {
	real, err := newRealAdapter(ctx)
	if err != nil {
		return nil, err
	}
	real.timeout = 30 * time.Minute
	real.http.Timeout = real.timeout
	return &artifactAdapter{real: real}, nil
}

func (adapter *artifactAdapter) Close() { adapter.real.Close() }

func (adapter *artifactAdapter) Execute(ctx context.Context, operation experiment.AdapterOperation) experiment.Sample {
	if operation.ExperimentID != "artifact" {
		return invalidSample(operation, "experiment_identity_mismatch")
	}
	if operation.WorkloadID != "result-heavy" || operation.Mode != "novel" {
		return invalidSample(operation, "unsupported_frozen_artifact_cell")
	}
	spec, err := experiment.ParseArtifactScale(operation.Scale)
	if err != nil {
		return invalidSample(operation, "unsupported_frozen_artifact_cell")
	}
	binding, err := loadAdapterDeploymentBinding()
	if err != nil || binding.Section.Artifact == nil {
		return invalidSample(operation, "artifact_binding_invalid")
	}
	cell, ok := binding.Section.Artifact.ResultHeavy[operation.Scale]
	if !ok || validateArtifactCellBinding(spec, cell) != nil {
		return invalidSample(operation, "artifact_cell_binding_invalid")
	}
	sample, err := adapter.executeResultHeavy(ctx, operation, binding, cell)
	if err != nil {
		return retainedArtifactFailure(operation, sample, "artifact_measurement_failed")
	}
	return validateArtifactPass(sample)
}

func validateArtifactPass(sample experiment.Sample) experiment.Sample {
	if sample.Status == "pass" {
		if err := experiment.ValidateArtifactEvidence(sample); err != nil {
			sample.Status = "fail"
			sample.ErrorCode = "artifact_evidence_invariant_failed"
			sample.Reason = "the retained real artifact sample failed its independent evidence invariant"
		}
	}
	return sample
}

func retainedArtifactFailure(operation experiment.AdapterOperation, sample experiment.Sample, code string) experiment.Sample {
	if sample.SchemaVersion == 0 {
		sample = failedSample(operation, code)
	} else {
		sample.Status = "fail"
		sample.ErrorCode = code
		sample.Reason = "a real artifact backend operation was attempted and failed; safely collected evidence is retained"
	}
	return sample
}

// observedTaskgateQueryPrefix retains only redacted, already returned service
// telemetry before the composite receipt/artifact verifier finishes. It never
// marks the response verified or available and never stores raw identifiers.
func observedTaskgateQueryPrefix(operation experiment.AdapterOperation, taskID, sqlText string,
	started time.Time, availableMS float64, response queryResponse,
	before, after experiment.RootLedgerSnapshot) experiment.Sample {
	sample := baseSample(operation, "taskgate")
	sample.ClientAvailableMS, sample.ClientFullDrainMS = availableMS, durationMS(time.Since(started))
	for name, value := range response.PipelineMS {
		sample.PipelineMS[name] = value
	}
	sample.DiagnosticMS = make(map[string]float64, len(response.DiagnosticMS))
	for name, value := range response.DiagnosticMS {
		sample.DiagnosticMS[name] = value
	}
	if response.RowCount >= 0 {
		sample.RowCount = response.RowCount
	}
	if response.ColumnCount >= 0 {
		sample.ColumnCount = response.ColumnCount
	}
	sample.PhysicalSQLSHA256, sample.LogicalSQLSHA256 = sha(sqlText), sha(sqlText)
	if validDigest(response.PlanDigest) {
		sample.QueryPlanSHA256 = response.PlanDigest
	}
	sample.ActualReleaseFacts, sample.ChargedReleaseFacts = response.Exposure.ActualReleaseFacts, response.Exposure.ChargedReleaseFacts
	sample.ActualDependencyFacts, sample.ChargedDependencyFacts = response.Exposure.ActualInfluenceFacts, response.Exposure.ChargedInfluenceFacts
	sample.ActualOutcomeFacts, sample.ChargedOutcomeFacts = response.Exposure.ActualOutcomeFacts, response.Exposure.ChargedOutcomeFacts
	if validDigest(response.Exposure.ReleaseSetSHA256) {
		sample.ReleaseSetSHA256 = response.Exposure.ReleaseSetSHA256
	}
	if validDigest(response.Exposure.InfluenceSetSHA256) {
		sample.DependencySetSHA256 = response.Exposure.InfluenceSetSHA256
	}
	if validDigest(response.Exposure.OutcomeSetSHA256) {
		sample.OutcomeSetSHA256 = response.Exposure.OutcomeSetSHA256
	}
	sample.RootEpochBefore, sample.RootEpochAfter = before.Epoch, after.Epoch
	sample.RootSetSHA256Before, sample.RootSetSHA256After = rootSetDigest(before), rootSetDigest(after)
	if taskID != "" {
		sample.RootTaskIDHash = saltedTaskHash(operation, taskID)
	}
	return sample
}

func validateArtifactCellBinding(spec experiment.ArtifactScaleSpec, cell artifactCellBinding) error {
	if validateBoundTask(cell.Task) != nil || validateBoundQuery(cell.Query) != nil ||
		cell.Query.ExpectedRows != spec.Rows || cell.Query.ExpectedColumns != spec.Columns ||
		cell.Query.DependencyFacts <= 0 || !validDigest(cell.Query.DependencySetSHA256) {
		return errors.New("artifact cell binding differs from frozen NxC or Dependency oracle")
	}
	approvedColumns := 0
	for _, columns := range cell.Task.Columns {
		approvedColumns += len(columns)
	}
	if approvedColumns < spec.Columns {
		return errors.New("artifact task approves fewer columns than the frozen result width")
	}
	return nil
}

func (adapter *artifactAdapter) executeResultHeavy(ctx context.Context, operation experiment.AdapterOperation,
	binding adapterDeploymentBinding, cell artifactCellBinding) (experiment.Sample, error) {
	probeDigest, err := adapter.real.verifyDatasetProbe(ctx, binding)
	if err != nil {
		return experiment.Sample{}, err
	}
	state := &pairState{}
	state.taskID, err = adapter.real.provisionBoundTask(ctx, operation, cell.Task)
	if err != nil {
		return experiment.Sample{}, err
	}
	beforeRoot, err := adapter.real.rootLedgerSnapshot(ctx, state.taskID)
	if err != nil || beforeRoot.Epoch != 0 {
		if err == nil {
			err = errors.New("artifact task root is not fresh")
		}
		return experiment.Sample{}, err
	}
	businessBefore, err := adapter.real.businessSQLSnapshotFor(ctx, cell.Task)
	if err != nil {
		return experiment.Sample{}, err
	}
	observerBefore, err := captureBoundObserver(ctx, "before")
	if err != nil {
		return experiment.Sample{}, err
	}
	requestID := "final-v5-artifact-" + sha(operation.SampleID)[:24]
	started := time.Now()
	var response queryResponse
	if err := adapter.real.alice.call(ctx, "query_sql", map[string]any{
		"task_id": state.taskID, "request_id": requestID, "sql": cell.Query.SQL,
	}, &response); err != nil {
		return experiment.Sample{}, err
	}
	availableMS := durationMS(time.Since(started))
	partial := observedTaskgateQueryPrefix(operation, state.taskID, cell.Query.SQL, started, availableMS,
		response, beforeRoot, beforeRoot)
	businessAfter, err := adapter.real.businessSQLSnapshotFor(ctx, cell.Task)
	if err != nil {
		return partial, err
	}
	afterRoot, err := adapter.real.rootLedgerSnapshot(ctx, state.taskID)
	if err != nil {
		return partial, err
	}
	partial = observedTaskgateQueryPrefix(operation, state.taskID, cell.Query.SQL, started, availableMS,
		response, beforeRoot, afterRoot)
	sample, err := adapter.real.completeTaskgateSample(ctx, operation, state, beforeRoot, afterRoot,
		started, availableMS, cell.Query.SQL, response)
	if err != nil {
		return partial, err
	}
	sample.ArtifactVerification = &experiment.ArtifactVerificationEvidence{
		Version: artifactVerificationVersion, BindingSHA256: binding.SectionSHA256,
		DatasetSHA256: binding.DatasetSHA256, CatalogSHA256: binding.CatalogSHA256,
		DatasetProbeSHA256: probeDigest, QuerySHA256: sha(cell.Query.SQL),
		ExpectedRows: cell.Query.ExpectedRows, ExpectedColumns: cell.Query.ExpectedColumns,
		ExpectedResultSHA256: cell.Query.ExpectedResultSHA256, ObservedRows: sample.RowCount,
		ObservedColumns: sample.ColumnCount, ObservedResultSHA256: sample.ResultSHA256,
		BusinessBefore: businessBefore, BusinessAfter: businessAfter, RootBefore: beforeRoot,
		RootAfter: afterRoot, ObserverBefore: observerBefore,
	}
	observerAfter, err := captureBoundObserver(ctx, "after")
	if err != nil {
		return sample, err
	}
	sample.ArtifactVerification.ObserverAfter = observerAfter
	visibleDelta := businessAfter.VisibleCalls - businessBefore.VisibleCalls
	companionDelta := businessAfter.CompanionCalls - businessBefore.CompanionCalls
	sample.BusinessSQLDelta = visibleDelta + companionDelta
	if visibleDelta != 1 || companionDelta != 1 {
		return sample, errors.New("artifact query did not execute exactly one visible and companion Business statement")
	}
	if err := applyObserverDelta(&sample, observerBefore, observerAfter, sample.BusinessSQLDelta); err != nil {
		return sample, err
	}
	if err := validateBoundSampleResult(sample, cell.Query); err != nil {
		return sample, err
	}
	return sample, nil
}
