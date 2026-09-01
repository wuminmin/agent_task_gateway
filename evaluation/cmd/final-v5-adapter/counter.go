package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/finalv5counter"
	"taskbound.local/agent-data-gateway/evaluation/finalv5rls"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

// counterAdapter runs the (a) comparator arms: the frozen 100-step adaptive
// trace in a frozen ordering on one fresh root, under one of the four
// a-priori budget profiles. Every step's outcome is pre-derived; the adapter
// records reality and the validator holds it to the table.
type counterAdapter struct {
	real     *realAdapter
	manifest finalv5counter.Manifest
	steps    []finalv5rls.Step
}

type counterInvariantError struct{ reason string }

func (err *counterInvariantError) Error() string { return err.reason }

func newCounterAdapter(ctx context.Context) (*counterAdapter, error) {
	manifest, err := finalv5counter.Load()
	if err != nil {
		return nil, err
	}
	rls, err := finalv5rls.Load()
	if err != nil {
		return nil, err
	}
	steps, err := rls.Trace()
	if err != nil {
		return nil, err
	}
	real, err := newRealAdapter(ctx)
	if err != nil {
		return nil, err
	}
	return &counterAdapter{real: real, manifest: manifest, steps: steps}, nil
}

func (adapter *counterAdapter) Close() {
	if adapter != nil && adapter.real != nil {
		adapter.real.Close()
	}
}

func validCounterCell(operation experiment.AdapterOperation) bool {
	if operation.ExperimentID != "counter" || operation.WorkloadID != finalv5counter.WorkloadID {
		return false
	}
	armKnown, orderingKnown := false, false
	for _, arm := range finalv5counter.Arms {
		armKnown = armKnown || operation.Mode == arm
	}
	for _, ordering := range finalv5counter.Orderings {
		orderingKnown = orderingKnown || operation.Scale == ordering
	}
	return armKnown && orderingKnown
}

func (adapter *counterAdapter) Execute(ctx context.Context, operation experiment.AdapterOperation) experiment.Sample {
	if !validCounterCell(operation) {
		return invalidSample(operation, "unsupported_source_controlled_counter_cell")
	}
	sample, err := adapter.runTrace(ctx, operation)
	if err == nil {
		if validationErr := experiment.ValidateCounterEvidence(sample); validationErr != nil {
			err = &counterInvariantError{reason: validationErr.Error()}
		}
	}
	if err == nil {
		return sample
	}
	return failedCounterSample(operation, sample, err)
}

func failedCounterSample(operation experiment.AdapterOperation, sample experiment.Sample, err error) experiment.Sample {
	writeAdapterFailureDiagnostic("counter", operation, err)
	if sample.SchemaVersion == 0 {
		sample = baseSample(operation, "taskgate")
	}
	sample.Status = "fail"
	var invariant *counterInvariantError
	if errors.As(err, &invariant) {
		sample.ErrorCode = "counter_invariant_violation"
		sample.Reason = "a real comparator trace completed off the a-priori outcome table"
	} else {
		sample.ErrorCode = "counter_real_execution_failure"
		sample.Reason = "a frozen comparator cell entered the real backend but did not complete its evidence chain"
	}
	return sample
}

func (adapter *counterAdapter) runTrace(ctx context.Context, operation experiment.AdapterOperation) (experiment.Sample, error) {
	want, err := adapter.manifest.Trace(operation.Mode, operation.Scale)
	if err != nil {
		return experiment.Sample{}, err
	}
	created, err := adapter.real.provisionScopedCatalogTask(ctx,
		fmt.Sprintf("Final V5 counter %s %s %s", operation.Mode, operation.Scale, operation.PairID),
		finalv5counter.Product, []string{"receipt_no", "amount"}, "",
		map[string]any{"department": []string{"销售部"}})
	if err != nil {
		return experiment.Sample{}, err
	}
	if created.RootTaskID != created.TaskID ||
		created.BudgetProfile != finalv5counter.ArmProfiles[operation.Mode] {
		return experiment.Sample{}, &counterInvariantError{reason: "counter task provisioning selected an unexpected budget profile"}
	}
	evidence := &experiment.CounterVerificationEvidence{
		Version: experiment.CounterEvidenceVersion, CorpusID: finalv5counter.CorpusID,
		CorpusSHA256: finalv5counter.CorpusSHA256(), Arm: operation.Mode, Ordering: operation.Scale,
		BudgetProfile: finalv5counter.ArmProfiles[operation.Mode],
	}
	started := time.Now()
	finish := func() experiment.Sample {
		sample := baseSample(operation, "taskgate")
		sample.ClientFullDrainMS = durationMS(time.Since(started))
		sample.RootTaskIDHash = saltedTaskHash(operation, created.RootTaskID)
		sample.CounterVerification = evidence
		return sample
	}
	for _, expected := range want.Steps {
		step := adapter.steps[expected.SourceIndex-1]
		sql := step.LogicalSQL(finalv5counter.Product)
		before, err := adapter.real.rootLedgerSnapshot(ctx, created.TaskID)
		if err != nil {
			return finish(), err
		}
		requestID := fmt.Sprintf("final-v5-counter-%s-%03d", sha(operation.SampleID)[:16], expected.Position)
		record := experiment.CounterStepEvidence{Position: expected.Position, SourceIndex: expected.SourceIndex,
			StepID: expected.StepID, LogicalSQLSHA256: sha(sql), Before: &before}
		queryStarted := time.Now()
		var response queryResponse
		callErr := adapter.real.alice.call(ctx, "query_sql", map[string]any{
			"task_id": created.TaskID, "request_id": requestID, "sql": sql,
		}, &response)
		record.ClientMS = durationMS(time.Since(queryStarted))
		after, snapshotErr := adapter.real.rootLedgerSnapshot(ctx, created.TaskID)
		if snapshotErr != nil {
			return finish(), snapshotErr
		}
		record.After = &after
		record.ChargedReleaseFacts = after.ReleaseCardinality - before.ReleaseCardinality
		record.ChargedDependencyFacts = after.DependencyCardinality - before.DependencyCardinality
		record.ChargedOutcomeFacts = after.OutcomeCardinality - before.OutcomeCardinality
		if callErr != nil {
			var structured *mcpCallError
			if !errors.As(callErr, &structured) {
				return finish(), callErr
			}
			record.Rejected, record.ObservedErrorCode = true, structured.Code
			evidence.Steps = append(evidence.Steps, record)
			evidence.RefusedSteps++
			if evidence.FirstRefusal == 0 {
				evidence.FirstRefusal = expected.Position
			}
			continue
		}
		released, err := adapter.real.completeTaskgateSample(ctx, operation, &pairState{taskID: created.TaskID},
			before, after, queryStarted, durationMS(time.Since(queryStarted)), sql, response)
		if err != nil {
			return finish(), err
		}
		record.Accepted = true
		record.ReleasedRows = released.RowCount
		evidence.Steps = append(evidence.Steps, record)
		evidence.AcceptedSteps++
	}
	final, err := adapter.real.rootLedgerSnapshot(ctx, created.TaskID)
	if err != nil {
		return finish(), err
	}
	evidence.FinalRoot = &final
	sample := finish()
	sample.Status = "pass"
	return sample, nil
}
