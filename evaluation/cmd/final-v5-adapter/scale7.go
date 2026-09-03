package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/evaluation/finalv5scale7"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

// scale7Adapter runs the P9.E SUM ladder: four single-query tasks per
// sample, each on a fresh root, whose largest rung settles more than 10^7
// Dependency facts. The direct arm times the same statements against the
// business database's reporting view - the same storage engine and
// materialized data with no governance.
type scale7Adapter struct {
	real     *realAdapter
	manifest finalv5scale7.Manifest
}

type scale7InvariantError struct{ reason string }

func (err *scale7InvariantError) Error() string { return err.reason }

func newScale7Adapter(ctx context.Context) (*scale7Adapter, error) {
	// The byte-identical rebuild gate lives in the corpus pin test; a full
	// rebuild here would hash ~2e7 facts on every adapter start.
	manifest, err := finalv5scale7.Load()
	if err != nil {
		return nil, err
	}
	real, err := newRealAdapter(ctx)
	if err != nil {
		return nil, err
	}
	return &scale7Adapter{real: real, manifest: manifest}, nil
}

func (adapter *scale7Adapter) Close() {
	if adapter != nil && adapter.real != nil {
		adapter.real.Close()
	}
}

func validScale7Cell(operation experiment.AdapterOperation) bool {
	return operation.ExperimentID == "scale7" && operation.WorkloadID == "scale7-ladder-v1" &&
		operation.Scale == "sum-ladder" &&
		(operation.Mode == "direct" || operation.Mode == "novel" || operation.Mode == "replay")
}

func (adapter *scale7Adapter) Execute(ctx context.Context, operation experiment.AdapterOperation) experiment.Sample {
	if !validScale7Cell(operation) {
		return invalidSample(operation, "unsupported_source_controlled_scale7_cell")
	}
	var sample experiment.Sample
	var err error
	if operation.Mode == "direct" {
		sample, err = adapter.runDirect(ctx, operation)
	} else {
		sample, err = adapter.runLadder(ctx, operation)
	}
	if err == nil {
		if validationErr := experiment.ValidateScale7Evidence(sample); validationErr != nil {
			err = &scale7InvariantError{reason: validationErr.Error()}
		}
	}
	if err == nil {
		return sample
	}
	writeAdapterFailureDiagnostic("scale7", operation, err)
	if sample.SchemaVersion == 0 {
		sample = baseSample(operation, "taskgate")
	}
	sample.Status = "fail"
	var invariant *scale7InvariantError
	if errors.As(err, &invariant) {
		sample.ErrorCode = "scale7_invariant_violation"
		sample.Reason = "a real scale ladder completed with a preregistered invariant mismatch"
	} else {
		sample.ErrorCode = "scale7_real_execution_failure"
		sample.Reason = "a frozen scale7 cell entered the real backend but did not complete its evidence chain"
	}
	return sample
}

func (adapter *scale7Adapter) newEvidence(mode string) *experiment.Scale7VerificationEvidence {
	return &experiment.Scale7VerificationEvidence{
		Version: experiment.Scale7EvidenceVersion, CorpusID: finalv5scale7.CorpusID,
		CorpusSHA256: finalv5scale7.CorpusSHA256(), Product: finalv5scale7.Product,
		BudgetProfile: scale7BudgetProfile, Mode: mode,
		MaxDependencyFacts: finalv5scale7.MaxDependencyFacts,
	}
}

const scale7BudgetProfile = "final-v5-scale7-v1"

// runDirect times every rung against the business reporting view over the
// reader connection: identical data, no governance path.
func (adapter *scale7Adapter) runDirect(ctx context.Context, operation experiment.AdapterOperation) (experiment.Sample, error) {
	dsn := strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_BUSINESS_DSN"))
	if dsn == "" {
		return experiment.Sample{}, errors.New("the business DSN is required for the direct arm")
	}
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return experiment.Sample{}, err
	}
	defer connection.Close(context.Background())
	evidence := adapter.newEvidence(operation.Mode)
	started := time.Now()
	for _, want := range adapter.manifest.Rungs {
		sql := strings.Replace(want.DirectSQL, finalv5scale7.CanonicalTable,
			"reporting.final_v5_scale_e7", 1)
		step := experiment.Scale7RungEvidence{Index: want.Index, ID: want.ID, Rows: want.Rows,
			LogicalSQLSHA256: sha(sql), DirectSQLSHA256: sha(want.DirectSQL),
			ExpectedDependencyFacts: want.Dependency.Cardinality}
		queryStarted := time.Now()
		row := connection.QueryRow(ctx, sql)
		targets := make([]any, len(want.Columns))
		values := make([]string, len(want.Columns))
		for i := range targets {
			targets[i] = &values[i]
		}
		if err := row.Scan(targets...); err != nil {
			return experiment.Sample{}, fmt.Errorf("direct rung %d: %w", want.Index, err)
		}
		step.ClientMS = durationMS(time.Since(queryStarted))
		for _, value := range values {
			rational, err := footprintRational(pgTextNumber(value))
			if err != nil {
				return experiment.Sample{}, err
			}
			step.ObservedScalars = append(step.ObservedScalars, rational.RatString())
		}
		step.Accepted = true
		evidence.Rungs = append(evidence.Rungs, step)
		evidence.AcceptedRungs++
	}
	sample := baseSample(operation, "taskgate")
	sample.ClientFullDrainMS = durationMS(time.Since(started))
	sample.Scale7Verification = evidence
	sample.Status = "pass"
	return sample, nil
}

// runLadder executes every rung through the Gateway on a fresh root; the
// replay arm additionally times a zero-charge semantic replay per rung.
func (adapter *scale7Adapter) runLadder(ctx context.Context, operation experiment.AdapterOperation) (experiment.Sample, error) {
	evidence := adapter.newEvidence(operation.Mode)
	started := time.Now()
	finish := func() experiment.Sample {
		sample := baseSample(operation, "taskgate")
		sample.ClientFullDrainMS = durationMS(time.Since(started))
		sample.Scale7Verification = evidence
		if len(evidence.Rungs) > 0 {
			sample.RootTaskIDHash = evidence.Rungs[0].RootTaskIDHash
		}
		return sample
	}
	approvedColumns := append(append([]string(nil), finalv5scale7.PredicateColumns...),
		finalv5scale7.LadderColumns...)
	for _, want := range adapter.manifest.Rungs {
		created, err := adapter.real.provisionScopedCatalogTask(ctx,
			fmt.Sprintf("Final V5 scale7 ladder %s %s %s", operation.Mode, want.ID, operation.PairID),
			finalv5scale7.Product, approvedColumns, "",
			map[string]any{"category": []string{"alpha", "beta", "gamma", "delta"}})
		if err != nil {
			return finish(), err
		}
		if created.RootTaskID != created.TaskID || created.BudgetProfile != scale7BudgetProfile ||
			created.Budget.MaxInfluenceFacts != finalv5scale7.MaxDependencyFacts {
			return finish(), &scale7InvariantError{reason: "scale7 task provisioning selected an unexpected product/profile/exposure budget"}
		}
		before, err := adapter.real.rootLedgerSnapshot(ctx, created.TaskID)
		if err != nil {
			return finish(), err
		}
		logical := want.LogicalSQL(finalv5scale7.Product)
		requestID := fmt.Sprintf("final-v5-scale7-%s-%02d", sha(operation.SampleID)[:16], want.Index)
		step := experiment.Scale7RungEvidence{Index: want.Index, ID: want.ID, Rows: want.Rows,
			LogicalSQLSHA256: sha(logical), DirectSQLSHA256: sha(want.DirectSQL),
			RootTaskIDHash:          saltedTaskHash(operation, created.RootTaskID),
			RequestIDHash:           saltedIdentityHash(operation, "request", requestID),
			ExpectedDependencyFacts: want.Dependency.Cardinality}
		queryStarted := time.Now()
		var response queryResponse
		if err := adapter.real.alice.call(ctx, "query_sql", map[string]any{
			"task_id": created.TaskID, "request_id": requestID, "sql": logical,
		}, &response); err != nil {
			return finish(), err
		}
		step.ClientMS = durationMS(time.Since(queryStarted))
		rootAfter, err := adapter.real.rootLedgerSnapshot(ctx, created.TaskID)
		if err != nil {
			return finish(), err
		}
		released, parquetBytes, err := adapter.real.completeTaskgateSampleWithParquet(ctx, operation,
			&pairState{taskID: created.TaskID}, before, rootAfter, queryStarted,
			step.ClientMS, logical, response)
		if err != nil {
			return finish(), err
		}
		if released.RowCount != 1 || released.ColumnCount != len(want.Columns) {
			return finish(), &scale7InvariantError{reason: "scale7 rung result shape differs from one aggregate row"}
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
		if operation.Mode == "replay" {
			replayID := requestID + "-replay"
			replayStarted := time.Now()
			var replayResponse queryResponse
			if err := adapter.real.alice.call(ctx, "query_sql", map[string]any{
				"task_id": created.TaskID, "request_id": replayID, "sql": logical,
			}, &replayResponse); err != nil {
				return finish(), err
			}
			step.ReplayClientMS = durationMS(time.Since(replayStarted))
			after, err := adapter.real.rootLedgerSnapshot(ctx, created.TaskID)
			if err != nil {
				return finish(), err
			}
			step.ReplayChargedFacts = (after.ReleaseCardinality - rootAfter.ReleaseCardinality) +
				(after.DependencyCardinality - rootAfter.DependencyCardinality) +
				(after.OutcomeCardinality - rootAfter.OutcomeCardinality)
		}
		evidence.Rungs = append(evidence.Rungs, step)
		evidence.AcceptedRungs++
	}
	sample := finish()
	sample.Status = "pass"
	return sample, nil
}

// pgTextNumber adapts pgx text-scanned numerics to the footprint rational
// parser's accepted types.
func pgTextNumber(value string) any {
	return json.Number(value)
}
