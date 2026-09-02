package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/finalv5adversary"
	"taskbound.local/agent-data-gateway/evaluation/finalv5rls"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

// adversaryAdapter runs the P9.C optimizing-adversary cells: one frozen
// extraction strategy on one fresh root under one owner-derived budget tier.
// The whole adaptive trace is pre-derived from the fixture, so the adapter
// replays the frozen probe sequence, verifies every accepted result hash
// against the fixture-derived expectation, and the validator holds the
// realized table to the corpus.
type adversaryAdapter struct {
	real     *realAdapter
	manifest finalv5adversary.Manifest
	rls      finalv5rls.Manifest
}

type adversaryInvariantError struct{ reason string }

func (err *adversaryInvariantError) Error() string { return err.reason }

// adversaryRouteDecoys differentiate the three tiers' exact product sets so
// each routes to its own budget profile; decoys are approved, never queried.
var adversaryRouteDecoys = map[string]map[string][]string{
	"owner":     {"provsql_orders": {"orderkey"}},
	"tightened": {"provsql_lineitem": {"orderkey", "linenumber"}},
	"loosened":  {"provsql_orders": {"orderkey"}, "provsql_lineitem": {"orderkey", "linenumber"}},
}

func newAdversaryAdapter(ctx context.Context) (*adversaryAdapter, error) {
	manifest, err := finalv5adversary.Load()
	if err != nil {
		return nil, err
	}
	rls, err := finalv5rls.Load()
	if err != nil {
		return nil, err
	}
	real, err := newRealAdapter(ctx)
	if err != nil {
		return nil, err
	}
	return &adversaryAdapter{real: real, manifest: manifest, rls: rls}, nil
}

func (adapter *adversaryAdapter) Close() {
	if adapter != nil && adapter.real != nil {
		adapter.real.Close()
	}
}

func validAdversaryCell(operation experiment.AdapterOperation) bool {
	if operation.ExperimentID != "adversary" || operation.WorkloadID != finalv5adversary.WorkloadID {
		return false
	}
	tierKnown, strategyKnown := false, false
	for _, tier := range finalv5adversary.Tiers {
		tierKnown = tierKnown || operation.Mode == tier.Name
	}
	for _, strategy := range finalv5adversary.Strategies {
		strategyKnown = strategyKnown || operation.Scale == strategy
	}
	return tierKnown && strategyKnown
}

func (adapter *adversaryAdapter) Execute(ctx context.Context, operation experiment.AdapterOperation) experiment.Sample {
	if !validAdversaryCell(operation) {
		return invalidSample(operation, "unsupported_source_controlled_adversary_cell")
	}
	sample, err := adapter.runTrace(ctx, operation)
	if err == nil {
		if validationErr := experiment.ValidateAdversaryEvidence(sample); validationErr != nil {
			err = &adversaryInvariantError{reason: validationErr.Error()}
		}
	}
	if err == nil {
		return sample
	}
	return failedAdversarySample(operation, sample, err)
}

func failedAdversarySample(operation experiment.AdapterOperation, sample experiment.Sample, err error) experiment.Sample {
	writeAdapterFailureDiagnostic("adversary", operation, err)
	if sample.SchemaVersion == 0 {
		sample = baseSample(operation, "taskgate")
	}
	sample.Status = "fail"
	var invariant *adversaryInvariantError
	if errors.As(err, &invariant) {
		sample.ErrorCode = "adversary_invariant_violation"
		sample.Reason = "a real adversary trace completed off the a-priori outcome table"
	} else {
		sample.ErrorCode = "adversary_real_execution_failure"
		sample.Reason = "a frozen adversary cell entered the real backend but did not complete its evidence chain"
	}
	return sample
}

// rebuildStep reconstructs the fixture-derived Step for a frozen outcome so
// the adapter can verify the released result hash and scalar against the same
// source of truth the corpus was built from.
func (adapter *adversaryAdapter) rebuildStep(strategy string, expected finalv5adversary.StepOutcome) (finalv5rls.Step, error) {
	if strategy == "bisection" {
		return adapter.rls.AdversaryCountProbeStep(expected.StepID, expected.Threshold)
	}
	return adapter.rls.AdversaryListingStep(expected.StepID, expected.Threshold)
}

func (adapter *adversaryAdapter) tier(name string) (finalv5adversary.Tier, error) {
	for _, tier := range finalv5adversary.Tiers {
		if tier.Name == name {
			return tier, nil
		}
	}
	return finalv5adversary.Tier{}, fmt.Errorf("unknown adversary tier %q", name)
}

func (adapter *adversaryAdapter) runTrace(ctx context.Context, operation experiment.AdapterOperation) (experiment.Sample, error) {
	want, err := adapter.manifest.Trace(operation.Mode, operation.Scale)
	if err != nil {
		return experiment.Sample{}, err
	}
	tier, err := adapter.tier(operation.Mode)
	if err != nil {
		return experiment.Sample{}, err
	}
	products := []string{finalv5adversary.Product}
	columns := map[string][]string{finalv5adversary.Product: {"receipt_no", "amount"}}
	for decoy, decoyColumns := range adversaryRouteDecoys[operation.Mode] {
		products = append(products, decoy)
		columns[decoy] = append([]string(nil), decoyColumns...)
	}
	scopes := map[string]any{"department": []string{"销售部"}}
	for _, product := range products {
		// Every ProvSQL decoy product declares the partition_key scope;
		// approving it without such a product would be an unapproved scope.
		if product == "provsql_orders" || product == "provsql_lineitem" || product == "provsql_nonce" {
			scopes["partition_key"] = []string{"1"}
		}
	}
	created, err := adapter.real.provisionMultiProductTask(ctx,
		fmt.Sprintf("Final V5 adversary %s %s %s", operation.Mode, operation.Scale, operation.PairID),
		products, columns, "", scopes)
	if err != nil {
		return experiment.Sample{}, err
	}
	if created.RootTaskID != created.TaskID || created.BudgetProfile != tier.BudgetProfile {
		return experiment.Sample{}, &adversaryInvariantError{reason: "adversary task provisioning selected an unexpected budget profile"}
	}
	evidence := &experiment.AdversaryVerificationEvidence{
		Version: experiment.AdversaryEvidenceVersion, CorpusID: finalv5adversary.CorpusID,
		CorpusSHA256: finalv5adversary.CorpusSHA256(), Tier: operation.Mode, Strategy: operation.Scale,
		BudgetProfile: tier.BudgetProfile,
		RecoveredLo:   want.RecoveredLo, RecoveredHi: want.RecoveredHi, RecoveredBits: want.RecoveredBits,
		RecoveredValue: cloneInt64(want.RecoveredValue),
	}
	started := time.Now()
	finish := func() experiment.Sample {
		sample := baseSample(operation, "taskgate")
		sample.ClientFullDrainMS = durationMS(time.Since(started))
		sample.RootTaskIDHash = saltedTaskHash(operation, created.RootTaskID)
		sample.AdversaryVerification = evidence
		return sample
	}
	for _, expected := range want.Steps {
		step, err := adapter.rebuildStep(operation.Scale, expected)
		if err != nil {
			return finish(), err
		}
		sql := step.LogicalSQL(finalv5adversary.Product)
		before, err := adapter.real.rootLedgerSnapshot(ctx, created.TaskID)
		if err != nil {
			return finish(), err
		}
		requestID := fmt.Sprintf("final-v5-adversary-%s-%03d", sha(operation.SampleID)[:16], expected.Position)
		record := experiment.AdversaryStepEvidence{Position: expected.Position, StepID: expected.StepID,
			Threshold: expected.Threshold, LogicalSQLSHA256: sha(sql), Before: &before}
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
			continue
		}
		released, err := adapter.real.completeTaskgateSample(ctx, operation, &pairState{taskID: created.TaskID},
			before, after, queryStarted, durationMS(time.Since(queryStarted)), sql, response)
		if err != nil {
			return finish(), err
		}
		if released.ResultSHA256 != finalv5rls.VerifiedResultSHA256(step) ||
			released.RowCount != int64(len(step.ExpectedRows)) {
			return finish(), &adversaryInvariantError{reason: fmt.Sprintf(
				"adversary step %d released a result off the fixture-derived expectation", expected.Position)}
		}
		record.Accepted = true
		record.ReleasedRows = released.RowCount
		record.ScalarCount = cloneInt64(step.Scalar)
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
