package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/finalv5footprint"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

// footprintAdapter runs the refused-footprint ladder: twelve single-query
// tasks per arm, each on a fresh root, whose acceptance or refusal under the
// Dependency ceiling is decided a priori by the frozen corpus.
type footprintAdapter struct {
	real     *realAdapter
	manifest finalv5footprint.Manifest
}

type footprintInvariantError struct{ reason string }

func (err *footprintInvariantError) Error() string { return err.reason }

func newFootprintAdapter(ctx context.Context) (*footprintAdapter, error) {
	if err := finalv5footprint.VerifyAgainstBuild(); err != nil {
		return nil, err
	}
	manifest, err := finalv5footprint.Load()
	if err != nil {
		return nil, err
	}
	real, err := newRealAdapter(ctx)
	if err != nil {
		return nil, err
	}
	return &footprintAdapter{real: real, manifest: manifest}, nil
}

func (adapter *footprintAdapter) Close() {
	if adapter != nil && adapter.real != nil {
		adapter.real.Close()
	}
}

func validFootprintCell(operation experiment.AdapterOperation) bool {
	return operation.ExperimentID == "footprint" && operation.WorkloadID == "refused-footprint-ladder-v1" &&
		operation.Scale == "1e5-rows" && (operation.Mode == "bounded" || operation.Mode == "unlimited")
}

func (adapter *footprintAdapter) Execute(ctx context.Context, operation experiment.AdapterOperation) experiment.Sample {
	if !validFootprintCell(operation) {
		return invalidSample(operation, "unsupported_source_controlled_footprint_cell")
	}
	sample, err := adapter.runLadder(ctx, operation)
	if err == nil {
		if validationErr := experiment.ValidateFootprintEvidence(sample); validationErr != nil {
			err = &footprintInvariantError{reason: validationErr.Error()}
		}
	}
	if err == nil {
		return sample
	}
	return failedFootprintSample(operation, sample, err)
}

// A supported cell that entered the real backend is always retained as fail.
func failedFootprintSample(operation experiment.AdapterOperation, sample experiment.Sample, err error) experiment.Sample {
	writeAdapterFailureDiagnostic("footprint", operation, err)
	if sample.SchemaVersion == 0 {
		sample = baseSample(operation, "taskgate")
	}
	sample.Status = "fail"
	var invariant *footprintInvariantError
	if errors.As(err, &invariant) {
		sample.ErrorCode = "footprint_invariant_violation"
		sample.Reason = "a real footprint ladder completed with a preregistered invariant mismatch"
	} else {
		sample.ErrorCode = "footprint_real_execution_failure"
		sample.Reason = "a frozen footprint cell entered the real backend but did not complete its evidence chain"
	}
	return sample
}

func (adapter *footprintAdapter) runLadder(ctx context.Context, operation experiment.AdapterOperation) (experiment.Sample, error) {
	product, profile := finalv5footprint.UnlimitedProduct, finalv5footprint.UnlimitedProfile
	maxRelease, maxDependency, maxOutcome := int64(1000), int64(1000000), int64(1000)
	if operation.Mode == "bounded" {
		product, profile = finalv5footprint.BoundedProduct, finalv5footprint.BoundedProfile
		maxRelease, maxDependency, maxOutcome = finalv5footprint.BoundedMaxReleaseFacts,
			finalv5footprint.BoundedMaxDependencyFacts, finalv5footprint.BoundedMaxOutcomeFacts
	}
	evidence := &experiment.FootprintVerificationEvidence{
		Version: experiment.FootprintEvidenceVersion, CorpusID: finalv5footprint.CorpusID,
		CorpusSHA256: finalv5footprint.CorpusSHA256(), Product: product, BudgetProfile: profile,
		BoundedMaxDependencyFacts: finalv5footprint.BoundedMaxDependencyFacts,
	}
	started := time.Now()
	var totalRows int64
	finish := func() experiment.Sample {
		sample := baseSample(operation, "taskgate")
		sample.ClientFullDrainMS = durationMS(time.Since(started))
		sample.RowCount = totalRows
		sample.FootprintVerification = evidence
		// The ladder provisions one fresh root per rung; the sample-level root
		// identity the runner's fresh-root gate tracks is the first rung's.
		if len(evidence.Rungs) > 0 {
			sample.RootTaskIDHash = evidence.Rungs[0].RootTaskIDHash
		}
		return sample
	}
	for _, want := range adapter.manifest.Rungs {
		// Every column the rung's SQL references must be approved: the ladder
		// filter columns row_id and category plus the rung's aggregate columns
		// (pilot-footprint-02: predicate references fail COLUMN_NOT_APPROVED).
		approvedColumns := append([]string{"row_id", "category"}, want.Columns...)
		created, err := adapter.real.provisionScopedCatalogTask(ctx,
			fmt.Sprintf("Final V5 footprint ladder %s %s %s", operation.Mode, want.ID, operation.PairID),
			product, approvedColumns, "",
			map[string]any{"category": []string{"alpha", "beta", "gamma", "delta"}})
		if err != nil {
			return finish(), err
		}
		if created.RootTaskID != created.TaskID || created.BudgetProfile != profile ||
			created.Budget.MaxQueries != 64 || created.Budget.MaxReleaseFacts != maxRelease ||
			created.Budget.MaxInfluenceFacts != maxDependency || created.Budget.MaxOutcomeFacts != maxOutcome {
			return finish(), &footprintInvariantError{reason: "footprint task provisioning selected an unexpected product/profile/exposure budget"}
		}
		before, err := adapter.real.rootLedgerSnapshot(ctx, created.TaskID)
		if err != nil {
			return finish(), err
		}
		requestID := fmt.Sprintf("final-v5-footprint-%s-%02d", sha(operation.SampleID)[:16], want.Index)
		step := experiment.FootprintRungEvidence{
			Index: want.Index, ID: want.ID, Rows: want.Rows,
			Columns:          append([]string(nil), want.Columns...),
			LogicalSQLSHA256: sha(want.LogicalSQL(product)), DirectSQLSHA256: sha(want.DirectSQL),
			RootTaskIDHash:          saltedTaskHash(operation, created.RootTaskID),
			RequestIDHash:           saltedIdentityHash(operation, "request", requestID),
			ExpectedDependencyFacts: want.Dependency.Cardinality,
		}
		queryStarted := time.Now()
		var response queryResponse
		callErr := adapter.real.alice.call(ctx, "query_sql", map[string]any{
			"task_id": created.TaskID, "request_id": requestID, "sql": want.LogicalSQL(product),
		}, &response)
		step.ClientMS = durationMS(time.Since(queryStarted))
		wantRefused := operation.Mode == "bounded" && want.BoundedRefused
		if callErr != nil {
			var structured *mcpCallError
			if !errors.As(callErr, &structured) {
				return finish(), callErr
			}
			evidence.Rungs = append(evidence.Rungs, step)
			if !wantRefused || structured.Code != want.BoundedRefusalCode() {
				return finish(), &footprintInvariantError{reason: fmt.Sprintf(
					"footprint rung %d rejection %q differs from the a-priori ladder design", want.Index, structured.Code)}
			}
			after, snapshotErr := adapter.real.rootLedgerSnapshot(ctx, created.TaskID)
			if snapshotErr != nil {
				return finish(), snapshotErr
			}
			if after.ReleaseCardinality != 0 || after.DependencyCardinality != 0 || after.OutcomeCardinality != 0 {
				return finish(), &footprintInvariantError{reason: "a refused footprint rung charged its fresh root"}
			}
			evidence.Rungs[len(evidence.Rungs)-1].Rejected = true
			evidence.Rungs[len(evidence.Rungs)-1].ObservedErrorCode = structured.Code
			evidence.RefusedRungs++
			continue
		}
		if wantRefused {
			evidence.Rungs = append(evidence.Rungs, step)
			return finish(), &footprintInvariantError{reason: fmt.Sprintf(
				"footprint rung %d completed above the a-priori Dependency ceiling", want.Index)}
		}
		rootAfter, err := adapter.real.rootLedgerSnapshot(ctx, created.TaskID)
		if err != nil {
			return finish(), err
		}
		released, parquetBytes, err := adapter.real.completeTaskgateSampleWithParquet(ctx, operation,
			&pairState{taskID: created.TaskID}, before, rootAfter, queryStarted,
			durationMS(time.Since(queryStarted)), want.LogicalSQL(product), response)
		if err != nil {
			return finish(), err
		}
		if released.RowCount != 1 || released.ColumnCount != len(want.Columns) {
			return finish(), &footprintInvariantError{reason: "footprint rung result shape differs from one aggregate row"}
		}
		scalars, err := footprintScalars(parquetBytes, response, len(want.Columns))
		if err != nil {
			return finish(), err
		}
		step.Accepted = true
		step.ChargedReleaseFacts = released.ChargedReleaseFacts
		step.ChargedDependencyFacts = released.ChargedDependencyFacts
		step.ChargedOutcomeFacts = released.ChargedOutcomeFacts
		step.ObservedScalars = scalars
		totalRows += released.RowCount
		evidence.Rungs = append(evidence.Rungs, step)
		evidence.AcceptedRungs++
	}
	sample := finish()
	sample.Status = "pass"
	return sample, nil
}

// footprintScalars re-reads the released Parquet bytes and canonicalizes the
// single aggregate row as exact rationals, the frozen corpus representation.
func footprintScalars(parquetBytes []byte, response queryResponse, columnCount int) ([]string, error) {
	intent := response.Receipt.ArtifactIntent
	if intent == nil {
		return nil, errors.New("footprint response omitted its artifact intent")
	}
	rows, err := parseParquet(parquetBytes, intent.ResultID, intent.RowCount)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || len(rows[0]) != columnCount {
		return nil, errors.New("footprint rung Parquet shape differs from one aggregate row")
	}
	scalars := make([]string, 0, columnCount)
	for _, value := range rows[0] {
		rational, err := footprintRational(value)
		if err != nil {
			return nil, err
		}
		scalars = append(scalars, rational.RatString())
	}
	return scalars, nil
}

func footprintRational(value any) (*big.Rat, error) {
	switch typed := value.(type) {
	case int64:
		return new(big.Rat).SetInt64(typed), nil
	case json.Number:
		rational, ok := new(big.Rat).SetString(typed.String())
		if !ok {
			return nil, fmt.Errorf("footprint scalar %q is not an exact decimal", typed.String())
		}
		return rational, nil
	}
	return nil, fmt.Errorf("footprint scalar has unsupported type %T", value)
}
