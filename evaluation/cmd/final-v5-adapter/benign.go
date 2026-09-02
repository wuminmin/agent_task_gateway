package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/finalv5benign"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

// benignAdapter runs the (c3) benign agent trace: the 28 frozen statements in
// question order on one fresh root per sample, under one of the three
// a-priori recipe budget profiles. Refusal positions are the measurement, so
// the runner records outcomes and fails only on protocol or corpus-identity
// violations.
type benignAdapter struct {
	real     *realAdapter
	manifest finalv5benign.Manifest
}

type benignInvariantError struct{ reason string }

func (err *benignInvariantError) Error() string { return err.reason }

// benignTraceColumns approves every column of the five trace products (the
// unedited statements may reference any of them).
var benignTraceColumns = map[string][]string{
	"expense_detail": {"receipt_no", "employee_no", "employee_name", "department", "expense_date",
		"expense_type", "amount", "city", "purpose", "status"},
	"expense_summary":  {"month", "department", "expense_type", "total_amount", "request_count"},
	"provsql_orders":   {"orderkey", "status", "partition_key"},
	"provsql_lineitem": {"orderkey", "linenumber", "extendedprice", "partition_key"},
	"final_v5_result_heavy": {"row_id", "category", "amount", "event_date", "sequence_no", "approved",
		"event_timestamp", "description", "quantity", "unit_price", "tax_amount",
		"settled_date", "processed_at", "region", "revision", "active"},
}

// benignRouteDecoys select the arm's budget profile: approval routes match
// exact product sets, so each arm requests a distinct set. The decoy
// products are approved (entity key only) and never queried; they exist
// purely so three exposure budgets can coexist over one immutable statement
// set whose table names cannot change.
var benignRouteDecoys = map[string][]string{
	"recipe": {},
	"x2":     {"provsql_nonce"},
	"x4":     {"provsql_nonce", "final_v5_attack_expense_detail"},
}

var benignDecoyColumns = map[string][]string{
	"provsql_nonce":                  {"nonce_id", "partition_key"},
	"final_v5_attack_expense_detail": {"receipt_no", "amount"},
}

var benignBudgetProfiles = map[string]string{
	"recipe": "final-v5-benign-recipe-v1", "x2": "final-v5-benign-x2-v1", "x4": "final-v5-benign-x4-v1",
}

func newBenignAdapter(ctx context.Context) (*benignAdapter, error) {
	manifest, err := finalv5benign.Load()
	if err != nil {
		return nil, err
	}
	real, err := newRealAdapter(ctx)
	if err != nil {
		return nil, err
	}
	return &benignAdapter{real: real, manifest: manifest}, nil
}

func (adapter *benignAdapter) Close() {
	if adapter != nil && adapter.real != nil {
		adapter.real.Close()
	}
}

func validBenignCell(operation experiment.AdapterOperation) bool {
	_, knownMode := benignBudgetProfiles[operation.Mode]
	return operation.ExperimentID == "benign" && operation.WorkloadID == finalv5benign.TraceWorkloadID &&
		operation.Scale == "28-statements" && knownMode
}

func (adapter *benignAdapter) Execute(ctx context.Context, operation experiment.AdapterOperation) experiment.Sample {
	if !validBenignCell(operation) {
		return invalidSample(operation, "unsupported_source_controlled_benign_cell")
	}
	sample, err := adapter.runTrace(ctx, operation)
	if err == nil {
		if validationErr := experiment.ValidateBenignEvidence(sample); validationErr != nil {
			err = &benignInvariantError{reason: validationErr.Error()}
		}
	}
	if err == nil {
		return sample
	}
	return failedBenignSample(operation, sample, err)
}

func failedBenignSample(operation experiment.AdapterOperation, sample experiment.Sample, err error) experiment.Sample {
	writeAdapterFailureDiagnostic("benign", operation, err)
	if sample.SchemaVersion == 0 {
		sample = baseSample(operation, "taskgate")
	}
	sample.Status = "fail"
	var invariant *benignInvariantError
	if errors.As(err, &invariant) {
		sample.ErrorCode = "benign_invariant_violation"
		sample.Reason = "a real benign trace completed with a preregistered invariant mismatch"
	} else {
		sample.ErrorCode = "benign_real_execution_failure"
		sample.Reason = "a frozen benign cell entered the real backend but did not complete its evidence chain"
	}
	return sample
}

func (adapter *benignAdapter) runTrace(ctx context.Context, operation experiment.AdapterOperation) (experiment.Sample, error) {
	budgetName := map[string]string{"recipe": "benign-recipe", "x2": "benign-x2", "x4": "benign-x4"}[operation.Mode]
	var budget *finalv5benign.RecipeBudget
	for index := range adapter.manifest.Budgets {
		if adapter.manifest.Budgets[index].Name == budgetName {
			budget = &adapter.manifest.Budgets[index]
		}
	}
	if budget == nil {
		return experiment.Sample{}, errors.New("the frozen corpus lacks the arm's budget")
	}
	products := []string{"expense_detail", "expense_summary", "provsql_orders", "provsql_lineitem",
		"final_v5_result_heavy"}
	columns := map[string][]string{}
	for _, name := range products {
		columns[name] = append([]string(nil), benignTraceColumns[name]...)
	}
	for _, decoy := range benignRouteDecoys[operation.Mode] {
		products = append(products, decoy)
		columns[decoy] = append([]string(nil), benignDecoyColumns[decoy]...)
	}
	scopes := map[string]any{
		// All-inclusive scopes: the trace's statements keep their own
		// semantics; the mandatory scope must not narrow them.
		"department":    []string{"销售部", "研发部", "财务部"},
		"partition_key": []string{"1"},
		"category":      []string{"alpha", "beta", "gamma", "delta"},
	}
	created, err := adapter.real.provisionMultiProductTask(ctx,
		fmt.Sprintf("Final V5 benign trace %s %s", operation.Mode, operation.PairID),
		products, columns, "", scopes)
	if err != nil {
		return experiment.Sample{}, err
	}
	wantProfile := benignBudgetProfiles[operation.Mode]
	if created.RootTaskID != created.TaskID || created.BudgetProfile != wantProfile ||
		created.Budget.MaxReleaseFacts != budget.MaxReleaseFacts ||
		created.Budget.MaxInfluenceFacts != budget.MaxInfluence ||
		created.Budget.MaxOutcomeFacts != budget.MaxOutcome ||
		created.Budget.MaxQueries != budget.MaxQueries {
		return experiment.Sample{}, &benignInvariantError{reason: "benign task provisioning selected an unexpected profile or budget"}
	}
	evidence := &experiment.BenignVerificationEvidence{
		Version: experiment.BenignEvidenceVersion, CorpusID: finalv5benign.CorpusID,
		CorpusSHA256: finalv5benign.CorpusSHA256(), BudgetName: budgetName, BudgetProfile: wantProfile,
		MaxReleaseFacts: budget.MaxReleaseFacts, MaxInfluenceFacts: budget.MaxInfluence,
		MaxOutcomeFacts: budget.MaxOutcome,
	}
	started := time.Now()
	finish := func() experiment.Sample {
		sample := baseSample(operation, "taskgate")
		sample.ClientFullDrainMS = durationMS(time.Since(started))
		sample.RootTaskIDHash = saltedTaskHash(operation, created.RootTaskID)
		sample.BenignVerification = evidence
		return sample
	}
	for _, want := range adapter.manifest.Statements {
		before, err := adapter.real.rootLedgerSnapshot(ctx, created.TaskID)
		if err != nil {
			return finish(), err
		}
		requestID := fmt.Sprintf("final-v5-benign-%s-%02d", sha(operation.SampleID)[:16], want.Index)
		step := experiment.BenignStepEvidence{Index: want.Index, ID: want.ID, SQLSHA256: want.SQLSHA256,
			Classification: string(want.Classification),
			RequestIDHash:  saltedIdentityHash(operation, "request", requestID), Before: &before}
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
		step.After = &after
		step.ChargedReleaseFacts = after.ReleaseCardinality - before.ReleaseCardinality
		step.ChargedDependencyFacts = after.DependencyCardinality - before.DependencyCardinality
		step.ChargedOutcomeFacts = after.OutcomeCardinality - before.OutcomeCardinality
		if callErr != nil {
			var structured *mcpCallError
			if !errors.As(callErr, &structured) {
				return finish(), callErr
			}
			step.Rejected, step.ObservedErrorCode = true, structured.Code
			evidence.Steps = append(evidence.Steps, step)
			evidence.RefusedStatements++
			if want.Classification != finalv5benign.ClassPolicyRefused {
				evidence.BudgetRefusals++
				if evidence.FirstBudgetRefusal == 0 {
					evidence.FirstBudgetRefusal = want.Index
				}
			}
			continue
		}
		released, err := adapter.real.completeTaskgateSample(ctx, operation, &pairState{taskID: created.TaskID},
			before, after, queryStarted, durationMS(time.Since(queryStarted)), want.SQL, response)
		if err != nil {
			return finish(), err
		}
		step.Accepted = true
		step.ReleasedRows = released.RowCount
		evidence.Steps = append(evidence.Steps, step)
		evidence.AcceptedStatements++
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
