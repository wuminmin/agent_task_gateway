package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/finalv5compare"
	"taskbound.local/agent-data-gateway/evaluation/finalv5scale7"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

// compare7Adapter runs the P9.F comparison sequence: sixty statements on
// one root task whose Dependency recipe the unique prefix exhausts partway,
// while byte-identical repeats settle as semantic replays at zero charge.
type compare7Adapter struct {
	real     *realAdapter
	manifest finalv5compare.Manifest
}

type compare7InvariantError struct{ reason string }

func (err *compare7InvariantError) Error() string { return err.reason }

func newCompare7Adapter(ctx context.Context) (*compare7Adapter, error) {
	manifest, err := finalv5compare.Load()
	if err != nil {
		return nil, err
	}
	real, err := newRealAdapter(ctx)
	if err != nil {
		return nil, err
	}
	return &compare7Adapter{real: real, manifest: manifest}, nil
}

func (adapter *compare7Adapter) Close() {
	if adapter != nil && adapter.real != nil {
		adapter.real.Close()
	}
}

func validCompare7Cell(operation experiment.AdapterOperation) bool {
	return operation.ExperimentID == "compare7" && operation.WorkloadID == "compare7-sequence-v1" &&
		operation.Scale == "seq-60" && operation.Mode == "bdg"
}

func (adapter *compare7Adapter) Execute(ctx context.Context, operation experiment.AdapterOperation) experiment.Sample {
	if !validCompare7Cell(operation) {
		return invalidSample(operation, "unsupported_source_controlled_compare7_cell")
	}
	sample, err := adapter.runSequence(ctx, operation)
	if err == nil {
		if validationErr := experiment.ValidateCompare7Evidence(sample); validationErr != nil {
			err = &compare7InvariantError{reason: validationErr.Error()}
		}
	}
	if err == nil {
		return sample
	}
	writeAdapterFailureDiagnostic("compare7", operation, err)
	if sample.SchemaVersion == 0 {
		sample = baseSample(operation, "taskgate")
	}
	sample.Status = "fail"
	var invariant *compare7InvariantError
	if errors.As(err, &invariant) {
		sample.ErrorCode = "compare7_invariant_violation"
		sample.Reason = "a real comparison sequence completed with a preregistered invariant mismatch"
	} else {
		sample.ErrorCode = "compare7_real_execution_failure"
		sample.Reason = "a frozen compare7 cell entered the real backend but did not complete its evidence chain"
	}
	return sample
}

func (adapter *compare7Adapter) runSequence(ctx context.Context, operation experiment.AdapterOperation) (experiment.Sample, error) {
	evidence := &experiment.Compare7VerificationEvidence{
		Version: experiment.Compare7EvidenceVersion, CorpusID: finalv5compare.CorpusID,
		CorpusSHA256: finalv5compare.CorpusSHA256(), Product: finalv5compare.Product,
		MaxDependencyFacts: finalv5compare.RecipeMaxDependencyFacts,
	}
	approvedColumns := append(append([]string(nil), finalv5scale7.PredicateColumns...),
		finalv5scale7.LadderColumns...)
	created, err := adapter.real.provisionScopedCatalogTask(ctx,
		fmt.Sprintf("Final V5 compare7 sequence %s", operation.PairID),
		finalv5compare.Product, approvedColumns, "",
		map[string]any{"category": []string{"alpha", "beta", "gamma", "delta"}})
	if err != nil {
		return experiment.Sample{}, err
	}
	if created.RootTaskID != created.TaskID ||
		created.Budget.MaxInfluenceFacts != finalv5compare.RecipeMaxDependencyFacts {
		return experiment.Sample{}, &compare7InvariantError{reason: "compare7 task provisioning selected an unexpected exposure budget"}
	}
	started := time.Now()
	acceptedByIndex := map[int]bool{}
	var ledger int64
	sample := baseSample(operation, "taskgate")
	finish := func() experiment.Sample {
		sample.ClientFullDrainMS = durationMS(time.Since(started))
		sample.Compare7Verification = evidence
		sample.RootTaskIDHash = saltedTaskHash(operation, created.RootTaskID)
		return sample
	}
	for _, want := range adapter.manifest.Steps {
		before, err := adapter.real.rootLedgerSnapshot(ctx, created.TaskID)
		if err != nil {
			return finish(), err
		}
		requestID := fmt.Sprintf("final-v5-compare7-%s-%02d", sha(operation.SampleID)[:16], want.Index)
		step := experiment.Compare7StepEvidence{Index: want.Index, RepeatOf: want.RepeatOf,
			Bound: want.Bound, SQLSHA256: sha(want.SQL),
			RequestIDHash: saltedIdentityHash(operation, "request", requestID)}
		queryStarted := time.Now()
		var response queryResponse
		callErr := adapter.real.alice.call(ctx, "query_sql", map[string]any{
			"task_id": created.TaskID, "request_id": requestID, "sql": want.SQL,
		}, &response)
		step.ClientMS = durationMS(time.Since(queryStarted))
		after, snapshotErr := adapter.real.rootLedgerSnapshot(ctx, created.TaskID)
		if snapshotErr != nil {
			return finish(), snapshotErr
		}
		step.ChargedReleaseFacts = after.ReleaseCardinality - before.ReleaseCardinality
		step.ChargedDependencyFacts = after.DependencyCardinality - before.DependencyCardinality
		step.ChargedOutcomeFacts = after.OutcomeCardinality - before.OutcomeCardinality
		ledger += step.ChargedDependencyFacts
		step.LedgerDependency = ledger
		if callErr != nil {
			var structured *mcpCallError
			if !errors.As(callErr, &structured) {
				return finish(), callErr
			}
			step.Rejected = true
			step.ObservedErrorCode = structured.Code
			evidence.BudgetRefusals++
			if evidence.FirstBudgetRefusal == 0 {
				evidence.FirstBudgetRefusal = want.Index
			}
		} else {
			step.Accepted = true
			evidence.AcceptedStatements++
			if want.RepeatOf != 0 && acceptedByIndex[want.RepeatOf] {
				evidence.RepeatCharges += step.ChargedReleaseFacts + step.ChargedDependencyFacts
			}
			acceptedByIndex[want.Index] = true
		}
		evidence.Steps = append(evidence.Steps, step)
	}
	sample.Status = "pass"
	return finish(), nil
}
