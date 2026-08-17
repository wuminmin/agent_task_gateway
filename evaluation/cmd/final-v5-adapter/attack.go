package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/evaluation/finalv5attack"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

const attackEvidenceVersion = "taskgate-final-v5-attack-evidence-v1"

type attackAdapter struct {
	real   *realAdapter
	corpus finalv5attack.Manifest
	states map[string]*attackState
}

type attackState struct {
	taskID           string
	childTaskID      string
	rootTaskID       string
	product          string
	budgetProfile    string
	novelRequestID   string
	novelQueryID     string
	novelResultID    string
	novelRequestHash string
	novelQueryHash   string
	novelResultHash  string
}

type attackInvariantError struct{ reason string }

func (err *attackInvariantError) Error() string { return err.reason }

type attackCatalogBinding struct {
	Product, BudgetProfile      string
	MaxQueries, MaxOutcomeFacts int64
}

func expectedAttackCatalogBinding(workloadID string) attackCatalogBinding {
	if workloadID == "E-threshold" {
		return attackCatalogBinding{Product: "expense_detail", BudgetProfile: "detail-manual-v5", MaxQueries: 5, MaxOutcomeFacts: 5}
	}
	return attackCatalogBinding{Product: "final_v5_attack_expense_detail", BudgetProfile: "final-v5-attack-medium-v1", MaxQueries: 10, MaxOutcomeFacts: 10}
}

// newAttackAdapter constructs the real public-query/OA adapter for the frozen
// A--E corpus. Every cell still passes its independent strict validator before
// a sample can be reported as successful.
func newAttackAdapter(ctx context.Context) (*attackAdapter, error) {
	corpus, err := finalv5attack.Load()
	if err != nil {
		return nil, err
	}
	real, err := newRealAdapter(ctx)
	if err != nil {
		return nil, err
	}
	return &attackAdapter{real: real, corpus: corpus, states: map[string]*attackState{}}, nil
}

func (adapter *attackAdapter) Close() {
	if adapter != nil && adapter.real != nil {
		adapter.real.Close()
	}
}

func (adapter *attackAdapter) Execute(ctx context.Context, operation experiment.AdapterOperation) experiment.Sample {
	if operation.ExperimentID != "attack" {
		return invalidSample(operation, "attack_experiment_identity_mismatch")
	}
	attackCase, found := adapter.corpus.Lookup(operation.WorkloadID, operation.Scale)
	if !found || !validAttackMode(operation.WorkloadID, operation.Mode) {
		return invalidSample(operation, "unsupported_source_controlled_attack_cell")
	}
	stateKey := operation.PairID + "\x00" + operation.RootGroupID
	state := adapter.states[stateKey]
	if state == nil {
		state = &attackState{}
		adapter.states[stateKey] = state
	}
	var sample experiment.Sample
	var err error
	if operation.Mode == "direct" {
		sample, err = adapter.runDirect(ctx, operation, attackCase)
	} else {
		sample, err = adapter.runTaskgate(ctx, operation, attackCase, state)
	}
	if err == nil {
		if validationErr := experiment.ValidateAttackEvidence(sample); validationErr != nil {
			err = &attackInvariantError{reason: validationErr.Error()}
		}
	}
	if err == nil {
		return sample
	}
	return failedAttackSample(operation, sample, err)
}

// failedAttackSample is intentionally lossless. Once a valid frozen cell has
// entered a real backend, query, delivery, cryptographic verification, timeout,
// and resource failures are measured failures. Any safely collected prefix is
// retained; only pre-execution identity/binding rejection remains invalid.
func failedAttackSample(operation experiment.AdapterOperation, sample experiment.Sample, err error) experiment.Sample {
	writeAdapterFailureDiagnostic("attack", operation, err)
	if sample.SchemaVersion == 0 {
		sample = baseSample(operation, attackSystem(operation.Mode))
	}
	sample.Status = "fail"
	var invariant *attackInvariantError
	if errors.As(err, &invariant) {
		sample.ErrorCode = "attack_invariant_violation"
		sample.Reason = "a real attack execution completed with an observed preregistered invariant mismatch"
	} else {
		sample.ErrorCode = "attack_real_execution_failure"
		sample.Reason = "a frozen attack cell entered the real backend but did not complete its authenticated evidence chain"
	}
	return sample
}

func validAttackMode(workloadID, mode string) bool {
	if workloadID == "C-request-id" {
		return mode == "novel" || mode == "semantic_replay" || mode == "idempotent_replay"
	}
	return mode == "direct" || mode == "novel"
}

func attackSystem(mode string) string {
	if mode == "direct" {
		return "postgresql"
	}
	return "taskgate"
}

func (adapter *attackAdapter) runDirect(ctx context.Context, operation experiment.AdapterOperation,
	attackCase finalv5attack.Case) (experiment.Sample, error) {
	started := time.Now()
	steps := make([]experiment.AttackStepEvidence, 0, len(attackCase.Steps))
	for index, corpusStep := range attackCase.Steps {
		rows, err := adapter.directQuery(ctx, corpusStep.DirectSQL)
		if err != nil {
			return finalizePartialDirectAttackSample(operation, attackCase, steps, durationMS(time.Since(started))), err
		}
		step, err := directAttackStep(index+1, corpusStep, rows)
		if err != nil {
			return finalizePartialDirectAttackSample(operation, attackCase, steps, durationMS(time.Since(started))), err
		}
		steps = append(steps, step)
	}
	elapsed := durationMS(time.Since(started))
	sample, err := finalizeAttackSample(operation, attackCase, steps, nil, elapsed)
	if err != nil {
		return sample, err
	}
	sample.PipelineMS["execute_and_derive"] = elapsed
	sample.PipelineMS["server_total"] = elapsed
	return sample, nil
}

func (adapter *attackAdapter) directQuery(ctx context.Context, sqlText string) ([][]any, error) {
	tx, err := adapter.real.business.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	rows, err := tx.Query(ctx, sqlText)
	if err != nil {
		return nil, err
	}
	var values [][]any
	for rows.Next() {
		value, valueErr := rows.Values()
		if valueErr != nil {
			rows.Close()
			return nil, valueErr
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return values, nil
}

func directAttackStep(index int, corpusStep finalv5attack.Step, rows [][]any) (experiment.AttackStepEvidence, error) {
	resultSHA256, rowSHA256, rowSetSHA256, scalar, err := attackRowEvidence(rows)
	if err != nil {
		return experiment.AttackStepEvidence{}, err
	}
	return experiment.AttackStepEvidence{
		Index: index, VariantID: corpusStep.ID, Classification: corpusStep.Classification, Role: corpusStep.Role,
		LogicalSQLSHA256: sha(corpusStep.LogicalSQL), DirectSQLSHA256: sha(corpusStep.DirectSQL),
		Accepted: true, RowCount: int64(len(rows)), ColumnCount: attackColumnCount(rows),
		ResultSHA256: resultSHA256, RowSHA256: rowSHA256, RowSetSHA256: rowSetSHA256, ScalarInt64: scalar,
	}, nil
}

func attackRowEvidence(rows [][]any) (string, []string, string, *int64, error) {
	resultSHA256, err := experiment.CanonicalResultHash(rows)
	if err != nil {
		return "", nil, "", nil, err
	}
	rowSHA256 := make([]string, 0, len(rows))
	for _, row := range rows {
		one, rowErr := experiment.CanonicalResultHash([][]any{row})
		if rowErr != nil {
			return "", nil, "", nil, rowErr
		}
		bound, rowErr := finalv5attack.RowSHA256(one)
		if rowErr != nil {
			return "", nil, "", nil, rowErr
		}
		rowSHA256 = append(rowSHA256, bound)
	}
	rowSetSHA256, err := finalv5attack.RowSetSHA256(rowSHA256)
	if err != nil {
		return "", nil, "", nil, err
	}
	var scalar *int64
	if len(rows) == 1 && len(rows[0]) == 1 {
		if value, ok := attackInt64(rows[0][0]); ok {
			scalar = &value
		}
	}
	return resultSHA256, rowSHA256, rowSetSHA256, scalar, nil
}

func attackColumnCount(rows [][]any) int {
	if len(rows) == 0 {
		return 0
	}
	return len(rows[0])
}

func attackInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case int64:
		return typed, true
	case int32:
		return int64(typed), true
	case int:
		return int64(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func finalizeAttackSample(operation experiment.AdapterOperation, attackCase finalv5attack.Case,
	steps []experiment.AttackStepEvidence, state *attackState, elapsedMS float64) (experiment.Sample, error) {
	sample := baseSample(operation, attackSystem(operation.Mode))
	sample.ClientAvailableMS, sample.ClientFullDrainMS = elapsedMS, elapsedMS
	evidence := &experiment.AttackVerificationEvidence{
		Version: attackEvidenceVersion, CorpusID: finalv5attack.CorpusID, CorpusSHA256: finalv5attack.CorpusSHA256,
		DatasetID: finalv5attack.DatasetID, Product: expectedAttackCatalogBinding(operation.WorkloadID).Product, Steps: steps,
		ExpectedThresholds: append([]int64(nil), attackCase.Thresholds...), OutcomeCeiling: attackCase.OutcomeCeiling,
	}
	primary := make([]string, 0, len(steps))
	var primaryRows int64
	for index, step := range steps {
		if index < len(attackCase.Steps) && attackCase.Steps[index].Primary && step.Accepted {
			primary = append(primary, step.ResultSHA256)
			primaryRows += step.RowCount
			if sample.ColumnCount == 0 {
				sample.ColumnCount = step.ColumnCount
			}
		}
		if step.Accepted && step.Classification == "accepted_equivalent" && sample.System == "taskgate" {
			evidence.NormalFormSHA256 = append(evidence.NormalFormSHA256, step.PlanSHA256)
		}
		if step.Accepted && index < len(attackCase.Steps) && attackCase.Steps[index].Threshold > 0 && step.ScalarInt64 != nil {
			evidence.ObservedThresholdResults = append(evidence.ObservedThresholdResults, *step.ScalarInt64)
		}
	}
	primarySHA256, err := finalv5attack.PrimaryResultSHA256(primary)
	if err != nil {
		return sample, err
	}
	evidence.PrimaryResultSHA256 = primarySHA256
	sample.ResultSHA256, sample.RowCount = primarySHA256, primaryRows
	if strings.HasPrefix(operation.WorkloadID, "A-") || strings.HasPrefix(operation.WorkloadID, "D-") {
		completeRows, decomposedRows := attackDecompositionRows(steps)
		evidence.CompleteRowSetSHA256, err = finalv5attack.RowSetSHA256(completeRows)
		if err == nil {
			evidence.DecomposedRowSetSHA256, err = finalv5attack.RowSetSHA256(decomposedRows)
		}
		if err != nil {
			return sample, err
		}
	}
	if state != nil {
		evidence.AnchorRequestIDHash, evidence.AnchorQueryIDHash, evidence.AnchorResultIDHash =
			state.novelRequestHash, state.novelQueryHash, state.novelResultHash
		evidence.BudgetProfile = state.budgetProfile
		if state.rootTaskID != "" {
			evidence.RootTaskIDHash = saltedTaskHash(operation, state.rootTaskID)
		}
	}
	if sample.System == "taskgate" {
		var firstBefore *experiment.AttackControlSnapshot
		var lastAfter *experiment.AttackControlSnapshot
		var lastReleased *experiment.AttackStepEvidence
		for index := range steps {
			step := &steps[index]
			if firstBefore == nil && step.Before != nil {
				firstBefore = step.Before
			}
			if step.After != nil {
				lastAfter = step.After
			}
			if step.Accepted {
				lastReleased = step
			}
			if step.Rejected && attackCase.Steps[index].Threshold > 0 {
				for thresholdIndex, threshold := range attackCase.Thresholds {
					if threshold == attackCase.Steps[index].Threshold {
						evidence.ThresholdRejectionIndex = thresholdIndex + 1
					}
				}
			}
		}
		if firstBefore == nil || lastAfter == nil || lastReleased == nil {
			return sample, errors.New("TaskGate attack trace lacks real Control/release evidence")
		}
		finalRoot := lastAfter.Root
		evidence.FinalRoot = &finalRoot
		evidence.ObservedOutcome = attackObservedThresholdOutcome(operation.WorkloadID, finalRoot)
		sample.RootEpochBefore, sample.RootEpochAfter = firstBefore.Root.Epoch, finalRoot.Epoch
		sample.RootSetSHA256Before = rootSetDigest(firstBefore.Root)
		sample.RootSetSHA256After = rootSetDigest(finalRoot)
		sample.ReleaseSetSHA256, sample.DependencySetSHA256, sample.OutcomeSetSHA256 =
			finalRoot.ReleaseSetSHA256, finalRoot.DependencySetSHA256, finalRoot.OutcomeSetSHA256
		sample.ActualReleaseFacts, sample.ActualDependencyFacts, sample.ActualOutcomeFacts =
			finalRoot.ReleaseCardinality, finalRoot.DependencyCardinality, finalRoot.OutcomeCardinality
		sample.ChargedReleaseFacts, sample.ChargedDependencyFacts, sample.ChargedOutcomeFacts =
			finalRoot.ReleaseCardinality-firstBefore.Root.ReleaseCardinality,
			finalRoot.DependencyCardinality-firstBefore.Root.DependencyCardinality,
			finalRoot.OutcomeCardinality-firstBefore.Root.OutcomeCardinality
		sample.RootTaskIDHash = evidence.RootTaskIDHash
		sample.ArtifactSHA256, sample.ObjectSHA256 = lastReleased.ArtifactSHA256, lastReleased.ObjectSHA256
		sample.ParquetBytes, sample.EncryptedObjectBytes = lastReleased.ParquetBytes, lastReleased.EncryptedObjectBytes
		sample.ReceiptVersion, sample.ReceiptSHA256 = lastReleased.ReceiptVersion, lastReleased.ReceiptSHA256
		sample.ArtifactIntentSHA256, sample.AvailabilityAuditSHA256 = lastReleased.ArtifactIntentSHA256, lastReleased.AvailabilitySHA256
		sample.ReceiptVerified, sample.ArtifactAvailable = true, true
		sample.SemanticReplay = operation.Mode == "semantic_replay"
		sample.IdempotentReplay = operation.Mode == "idempotent_replay"
		if sample.SemanticReplay || sample.IdempotentReplay {
			sample.BusinessSQLDelta = 0
		} else {
			sample.BusinessSQLDelta = lastAfter.Business.VisibleCalls - firstBefore.Business.VisibleCalls +
				lastAfter.Business.CompanionCalls - firstBefore.Business.CompanionCalls
		}
	}
	sample.PhysicalSQLSHA256 = attackSQLSequenceSHA256(attackCase, sample.System == "postgresql")
	sample.LogicalSQLSHA256 = attackSQLSequenceSHA256(attackCase, false)
	sample.QueryPlanSHA256 = sha(finalv5attack.CorpusSHA256 + "\x00" + operation.WorkloadID + "\x00" + operation.Scale)
	sample.Trace = buildAttackTrace(sample.System, attackCase, steps)
	sample.AttackVerification = evidence
	sample.Status = "pass"
	return sample, nil
}

func attackObservedThresholdOutcome(workloadID string, root experiment.RootLedgerSnapshot) int64 {
	if workloadID == "E-threshold" {
		return root.OutcomeCardinality
	}
	return 0
}

func attackDecompositionRows(steps []experiment.AttackStepEvidence) (complete, decomposed []string) {
	for _, step := range steps {
		if step.Accepted && step.Role == "complete" {
			complete = append(complete, step.RowSHA256...)
		}
		if step.Accepted && step.Role == "partition" {
			decomposed = append(decomposed, step.RowSHA256...)
		}
	}
	return complete, decomposed
}

func attackSQLSequenceSHA256(attackCase finalv5attack.Case, direct bool) string {
	values := make([]string, 0, len(attackCase.Steps))
	for _, step := range attackCase.Steps {
		value := step.LogicalSQL
		if direct {
			value = step.DirectSQL
		}
		values = append(values, value)
	}
	return sha(strings.Join(values, "\x00"))
}

func buildAttackTrace(system string, attackCase finalv5attack.Case, steps []experiment.AttackStepEvidence) []experiment.TraceStep {
	trace := make([]experiment.TraceStep, 0, len(steps))
	state := sha("TASKGATE-FINAL-V5-ATTACK-TRACE-V1\x00" + finalv5attack.CorpusSHA256)
	for index, step := range steps {
		sqlText := attackCase.Steps[index].LogicalSQL
		if system == "postgresql" {
			sqlText = attackCase.Steps[index].DirectSQL
		}
		next := "TASKGATE-FINAL-V5-ATTACK-END-V1"
		if index+1 < len(attackCase.Steps) {
			next = attackCase.Steps[index+1].LogicalSQL
			if system == "postgresql" {
				next = attackCase.Steps[index+1].DirectSQL
			}
		}
		transition := step.ResultSHA256
		if step.Rejected {
			transition = step.ObservedErrorCode + "\x00" + step.ObservedErrorReason
		}
		trace = append(trace, experiment.TraceStep{
			Index: index + 1, ConcreteSQL: sqlText, PriorStateSHA256: state, ResultSHA256: step.ResultSHA256,
			NextSQLSHA256: sha(next), PlanSHA256: step.PlanSHA256, ObservationSHA256: step.ObservationSHA256,
			ReleaseSetSHA256: step.ReleaseSetSHA256, DependencySetSHA256: step.DependencySetSHA256,
			OutcomeSetSHA256: step.OutcomeSetSHA256, Rejected: step.Rejected,
			NoResult: step.Rejected, NoAvailableArtifact: step.Rejected, NoSuccessfulAudit: step.Rejected,
		})
		state = sha(state + "\x00" + transition)
	}
	return trace
}

func (adapter *attackAdapter) runTaskgate(ctx context.Context, operation experiment.AdapterOperation,
	attackCase finalv5attack.Case, state *attackState) (experiment.Sample, error) {
	binding := expectedAttackCatalogBinding(operation.WorkloadID)
	if state.taskID == "" {
		created, err := adapter.real.provisionCatalogTask(ctx,
			"Final V5 adaptive attack "+operation.WorkloadID+" "+operation.Scale+" "+operation.PairID,
			binding.Product, []string{"receipt_no", "amount"}, "")
		if err != nil {
			return experiment.Sample{}, err
		}
		if created.RootTaskID != created.TaskID || created.BudgetProfile != binding.BudgetProfile ||
			created.Budget.MaxQueries != binding.MaxQueries || created.Budget.MaxOutcomeFacts != binding.MaxOutcomeFacts {
			return experiment.Sample{}, &attackInvariantError{reason: "real task provisioning selected an unexpected product profile/root budget"}
		}
		state.taskID, state.rootTaskID = created.TaskID, created.RootTaskID
		state.product, state.budgetProfile = binding.Product, created.BudgetProfile
	}
	// The E ceiling probe executes its first and sixth queries on one real child
	// task. The intervening four queries remain on the root so their exact and
	// semantic replays retain the root task's committed materialization cache.
	// This leaves both ordinary resource-query ledgers below their ceilings
	// while the shared root exposure ledger alone rejects B+1.
	if operation.WorkloadID == "E-threshold" && state.childTaskID == "" {
		child, err := adapter.real.provisionCatalogTask(ctx,
			"Final V5 adaptive attack E ceiling terminal child "+operation.PairID,
			binding.Product, []string{"receipt_no", "amount"}, state.taskID)
		if err != nil {
			return experiment.Sample{}, err
		}
		if child.RootTaskID != state.rootTaskID || child.BudgetProfile != binding.BudgetProfile ||
			child.Budget.MaxQueries != binding.MaxQueries || child.Budget.MaxOutcomeFacts != binding.MaxOutcomeFacts {
			return experiment.Sample{}, &attackInvariantError{reason: "E child task did not retain the root/profile/budget binding"}
		}
		state.childTaskID = child.TaskID
	}
	started := time.Now()
	steps := make([]experiment.AttackStepEvidence, 0, len(attackCase.Steps))
	for index, corpusStep := range attackCase.Steps {
		taskID := state.taskID
		if corpusStep.TaskRoute == finalv5attack.TaskRouteDelegatedChild {
			if state.childTaskID == "" {
				return retainedAttackPrefix(operation, attackCase, steps, state, durationMS(time.Since(started)),
					errors.New("E child was not provisioned before the shared-root sequence"))
			}
			taskID = state.childTaskID
		}
		requestID, err := attackRequestID(operation, corpusStep, state)
		if err != nil {
			return retainedAttackPrefix(operation, attackCase, steps, state, durationMS(time.Since(started)), err)
		}
		step, response, err := adapter.taskgateStep(ctx, operation, taskID, state.budgetProfile, requestID, index+1, corpusStep)
		steps = append(steps, step)
		if err != nil {
			return retainedAttackPrefix(operation, attackCase, steps, state, durationMS(time.Since(started)), err)
		}
		if operation.WorkloadID == "C-request-id" && operation.Mode == "novel" {
			state.novelRequestID, state.novelQueryID, state.novelResultID = requestID, response.QueryID, response.ResultID
			state.novelRequestHash = saltedIdentityHash(operation, "request", requestID)
			state.novelQueryHash = saltedIdentityHash(operation, "query", response.QueryID)
			state.novelResultHash = saltedIdentityHash(operation, "result", response.ResultID)
		}
	}
	elapsed := durationMS(time.Since(started))
	sample, err := finalizeAttackSample(operation, attackCase, steps, state, elapsed)
	if err != nil {
		return sample, err
	}
	// Correctness is primary for this campaign. Pipeline components remain the
	// authenticated per-query values in each receipt; the aggregate operation
	// reports its end-to-end wall boundary without double-counting overlaps.
	sample.PipelineMS["execute_and_derive"] = elapsed
	sample.PipelineMS["server_total"] = elapsed
	return sample, nil
}

func finalizePartialAttackSample(operation experiment.AdapterOperation, attackCase finalv5attack.Case,
	steps []experiment.AttackStepEvidence, state *attackState, elapsed float64) (experiment.Sample, error) {
	sample := baseSample(operation, "taskgate")
	sample.ClientAvailableMS, sample.ClientFullDrainMS = elapsed, elapsed
	sample.PipelineMS["execute_and_derive"], sample.PipelineMS["server_total"] = elapsed, elapsed
	sample.Trace = buildAttackTrace("taskgate", attackCase, steps)
	sample.AttackVerification = &experiment.AttackVerificationEvidence{
		Version: attackEvidenceVersion, CorpusID: finalv5attack.CorpusID, CorpusSHA256: finalv5attack.CorpusSHA256,
		DatasetID: finalv5attack.DatasetID, Product: expectedAttackCatalogBinding(operation.WorkloadID).Product,
		Steps: steps, ExpectedThresholds: append([]int64(nil), attackCase.Thresholds...),
		OutcomeCeiling: attackCase.OutcomeCeiling,
	}
	if state != nil {
		sample.AttackVerification.BudgetProfile = state.budgetProfile
		if state.rootTaskID != "" {
			sample.AttackVerification.RootTaskIDHash = saltedTaskHash(operation, state.rootTaskID)
		}
		sample.AttackVerification.AnchorRequestIDHash = state.novelRequestHash
		sample.AttackVerification.AnchorQueryIDHash = state.novelQueryHash
		sample.AttackVerification.AnchorResultIDHash = state.novelResultHash
	}
	return sample, nil
}

func retainedAttackPrefix(operation experiment.AdapterOperation, attackCase finalv5attack.Case,
	steps []experiment.AttackStepEvidence, state *attackState, elapsed float64, cause error) (experiment.Sample, error) {
	partial, err := finalizePartialAttackSample(operation, attackCase, steps, state, elapsed)
	if err != nil {
		return partial, errors.Join(cause, err)
	}
	return partial, cause
}

func finalizePartialDirectAttackSample(operation experiment.AdapterOperation, attackCase finalv5attack.Case,
	steps []experiment.AttackStepEvidence, elapsed float64) experiment.Sample {
	sample := baseSample(operation, "postgresql")
	sample.ClientAvailableMS, sample.ClientFullDrainMS = elapsed, elapsed
	sample.PipelineMS["execute_and_derive"], sample.PipelineMS["server_total"] = elapsed, elapsed
	sample.Trace = buildAttackTrace("postgresql", attackCase, steps)
	sample.AttackVerification = &experiment.AttackVerificationEvidence{
		Version: attackEvidenceVersion, CorpusID: finalv5attack.CorpusID, CorpusSHA256: finalv5attack.CorpusSHA256,
		DatasetID: finalv5attack.DatasetID, Product: expectedAttackCatalogBinding(operation.WorkloadID).Product,
		Steps:              append([]experiment.AttackStepEvidence(nil), steps...),
		ExpectedThresholds: append([]int64(nil), attackCase.Thresholds...), OutcomeCeiling: attackCase.OutcomeCeiling,
	}
	return sample
}

func attackRequestID(operation experiment.AdapterOperation, step finalv5attack.Step, state *attackState) (string, error) {
	if operation.WorkloadID == "C-request-id" && operation.Mode == "idempotent_replay" {
		if state.novelRequestID == "" {
			return "", errors.New("request-ID idempotent replay lacks a novel anchor")
		}
		return state.novelRequestID, nil
	}
	suffix := operation.Mode + "\x00" + step.ID
	return "final-v5-attack-" + sha(operation.PairID)[:16] + "-" + sha(suffix)[:16], nil
}

func (adapter *attackAdapter) taskgateStep(ctx context.Context, operation experiment.AdapterOperation,
	taskID, budgetProfile, requestID string, index int, corpusStep finalv5attack.Step) (experiment.AttackStepEvidence, queryResponse, error) {
	step := experiment.AttackStepEvidence{
		Index: index, VariantID: corpusStep.ID, Classification: corpusStep.Classification, Role: corpusStep.Role,
		LogicalSQLSHA256: sha(corpusStep.LogicalSQL), DirectSQLSHA256: sha(corpusStep.DirectSQL),
		RequestIDHash: saltedIdentityHash(operation, "request", requestID),
	}
	before, err := adapter.attackControlSnapshot(ctx, operation, taskID, budgetProfile)
	if err != nil {
		return step, queryResponse{}, err
	}
	step.Before = &before
	started := time.Now()
	var response queryResponse
	callErr := adapter.real.alice.call(ctx, "query_sql", map[string]any{
		"task_id": taskID, "request_id": requestID, "sql": corpusStep.LogicalSQL,
	}, &response)
	if callErr != nil {
		var structured *mcpCallError
		if !errors.As(callErr, &structured) {
			return step, response, callErr
		}
		after, snapshotErr := adapter.attackControlSnapshot(ctx, operation, taskID, budgetProfile)
		if snapshotErr != nil {
			return step, response, snapshotErr
		}
		rejected, projectionErr := adapter.attackRejectedQuery(ctx, operation, taskID, requestID)
		if projectionErr != nil {
			return step, response, projectionErr
		}
		step.After, step.RejectedQuery = &after, &rejected
		step.Rejected = true
		step.ObservedErrorCode = structured.Code
		step.ObservedErrorReason = safeAttackErrorReason(structured.Code, structured.Reason)
		if structured.TraceID != "" {
			step.TraceIDHash = saltedIdentityHash(operation, "trace", structured.TraceID)
		}
		if corpusStep.Classification != "expected_rejection" || structured.Code != corpusStep.ExpectedErrorCode ||
			(corpusStep.ExpectedErrorReason != "" && structured.Reason != corpusStep.ExpectedErrorReason) {
			return step, response, &attackInvariantError{reason: "observed structured rejection differs from the frozen attack corpus"}
		}
		return step, response, nil
	}
	rootAfterQuery, err := adapter.real.rootLedgerSnapshot(ctx, taskID)
	if err != nil {
		return step, response, err
	}
	state := &pairState{taskID: taskID}
	availableMS := durationMS(time.Since(started))
	released, err := adapter.real.completeTaskgateSample(ctx, operation, state, before.Root, rootAfterQuery,
		started, availableMS, corpusStep.LogicalSQL, response)
	if err != nil {
		return step, response, err
	}
	rows, err := adapter.downloadAttackRows(ctx, response)
	if err != nil {
		return step, response, err
	}
	resultSHA256, rowSHA256, rowSetSHA256, scalar, err := attackRowEvidence(rows)
	if err != nil || resultSHA256 != released.ResultSHA256 {
		if err == nil {
			err = errors.New("attack row evidence differs from verified released result")
		}
		return step, response, err
	}
	after, err := adapter.attackControlSnapshot(ctx, operation, taskID, budgetProfile)
	if err != nil {
		return step, response, err
	}
	if after.Root != rootAfterQuery {
		return step, response, errors.New("root changed while verifying the released attack artifact")
	}
	metadata, err := adapter.attackResultMetadata(ctx, response.QueryID)
	if err != nil {
		return step, response, err
	}
	step.Accepted, step.After, step.Verification = true, &after, released.BaselineVerification
	step.QueryIDHash = saltedIdentityHash(operation, "query", response.QueryID)
	step.ResultIDHash = saltedIdentityHash(operation, "result", response.ResultID)
	step.RowCount, step.ColumnCount = released.RowCount, released.ColumnCount
	step.ResultSHA256, step.RowSHA256, step.RowSetSHA256, step.ScalarInt64 = resultSHA256, rowSHA256, rowSetSHA256, scalar
	step.PlanSHA256, step.ResultMetadataJSON = response.PlanDigest, metadata
	step.ObservationSHA256 = response.Exposure.ObservationSHA256
	step.ReleaseSetSHA256, step.DependencySetSHA256, step.OutcomeSetSHA256 =
		response.Exposure.ReleaseSetSHA256, response.Exposure.InfluenceSetSHA256, response.Exposure.OutcomeSetSHA256
	step.ActualReleaseFacts, step.ChargedReleaseFacts = released.ActualReleaseFacts, released.ChargedReleaseFacts
	step.ActualDependencyFacts, step.ChargedDependencyFacts = released.ActualDependencyFacts, released.ChargedDependencyFacts
	step.ActualOutcomeFacts, step.ChargedOutcomeFacts = released.ActualOutcomeFacts, released.ChargedOutcomeFacts
	step.PredicateAtomCount, step.CompositeCount = released.PredicateAtomCount, released.CompositeCount
	step.SemanticReplay, step.IdempotentReplay = response.SemanticReplay, response.IdempotentReplay
	step.RootTaskIDHash, step.RootEpochAfter = released.RootTaskIDHash, released.RootEpochAfter
	step.ArtifactSHA256, step.ObjectSHA256 = released.ArtifactSHA256, released.ObjectSHA256
	step.ParquetBytes, step.EncryptedObjectBytes = released.ParquetBytes, released.EncryptedObjectBytes
	step.ReceiptVersion, step.ReceiptSHA256 = released.ReceiptVersion, released.ReceiptSHA256
	step.ArtifactIntentSHA256, step.AvailabilitySHA256 = released.ArtifactIntentSHA256, released.AvailabilityAuditSHA256
	if corpusStep.Classification == "expected_rejection" {
		return step, response, &attackInvariantError{reason: "an expected fail-closed attack step released a result"}
	}
	return step, response, nil
}

func safeAttackErrorReason(code, structuredReason string) string {
	if code == "EXPOSURE_BUDGET_EXHAUSTED" {
		return "ROOT_OUTCOME_CEILING_EXCEEDED"
	}
	// SQL lowering reasons are already a source-controlled, client-safe enum.
	// Unknown raw message text is never copied into experiment evidence.
	if code == "SQL_NOT_LOWERABLE" {
		return structuredReason
	}
	return "UNREGISTERED_STRUCTURED_REJECTION"
}

func (adapter *attackAdapter) downloadAttackRows(ctx context.Context, response queryResponse) ([][]any, error) {
	if response.Receipt.ArtifactIntent == nil {
		return nil, errors.New("attack response omitted signed artifact intent")
	}
	var delivery struct {
		DownloadURL    string `json:"download_url"`
		ArtifactSHA256 string `json:"artifact_sha256"`
	}
	if err := adapter.real.alice.call(ctx, "deliver_result", map[string]any{
		"result_id": response.ResultID, "format": "parquet",
	}, &delivery); err != nil {
		return nil, err
	}
	intent := response.Receipt.ArtifactIntent
	if delivery.DownloadURL == "" || delivery.ArtifactSHA256 != intent.ParquetSHA256 {
		return nil, errors.New("attack delivery metadata differs from signed artifact intent")
	}
	encoded, err := httpGet(ctx, adapter.real.http, delivery.DownloadURL, 1<<30)
	if err != nil {
		return nil, err
	}
	if shaBytes(encoded) != intent.ParquetSHA256 {
		return nil, errors.New("attack delivery bytes differ from signed artifact intent")
	}
	return parseParquet(encoded, intent.ResultID, intent.RowCount)
}

func (adapter *attackAdapter) attackResultMetadata(ctx context.Context, queryID string) (json.RawMessage, error) {
	var encoded []byte
	if err := adapter.real.control.QueryRow(ctx, `
SELECT result_metadata_json FROM result_artifacts WHERE query_id=$1 AND status='AVAILABLE'`, queryID).Scan(&encoded); err != nil {
		return nil, err
	}
	return normalizeAttackResultMetadata(encoded)
}

func normalizeAttackResultMetadata(encoded []byte) (json.RawMessage, error) {
	// result_metadata_json is JSONB, whose wire formatting is not the byte
	// representation normalized and hashed by Control before the V8 receipt is
	// signed. Reapply Control's unmarshal/marshal normalization so the retained
	// evidence is compared to the exact signed bytes rather than PostgreSQL's
	// presentation of the same JSON value.
	var value any
	if err := json.Unmarshal(encoded, &value); err != nil {
		return nil, errors.New("attack result metadata is not valid JSON")
	}
	normalized, err := json.Marshal(value)
	if err != nil {
		return nil, errors.New("attack result metadata cannot be normalized")
	}
	return json.RawMessage(normalized), nil
}

func (adapter *attackAdapter) attackControlSnapshot(ctx context.Context, operation experiment.AdapterOperation,
	taskID, budgetProfile string) (experiment.AttackControlSnapshot, error) {
	var snapshot experiment.AttackControlSnapshot
	tx, err := adapter.real.control.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return snapshot, err
	}
	defer tx.Rollback(context.Background())
	snapshot.Root, err = loadRootLedgerSnapshot(ctx, tx, taskID)
	if err != nil {
		return snapshot, err
	}
	var rawTaskID, rawRootTaskID string
	var productsJSON []byte
	err = tx.QueryRow(ctx, `
SELECT t.id,t.root_task_id,g.approved_products_json,b.max_queries,b.used_queries,b.reserved_queries,g.max_outcome_facts
FROM tasks t
JOIN task_grants g ON g.task_id=t.id
JOIN budget_ledger b ON b.task_id=t.id
WHERE t.id=$1`, taskID).Scan(
		&rawTaskID, &rawRootTaskID, &productsJSON, &snapshot.MaxQueries, &snapshot.UsedQueries,
		&snapshot.ReservedQueries, &snapshot.MaxOutcomeFacts)
	if err != nil {
		return snapshot, err
	}
	var products []string
	if err := json.Unmarshal(productsJSON, &products); err != nil || len(products) != 1 || strings.TrimSpace(products[0]) == "" {
		return snapshot, errors.New("attack task grant does not bind exactly one product")
	}
	snapshot.TaskIDHash = saltedTaskHash(operation, rawTaskID)
	snapshot.RootTaskIDHash = saltedTaskHash(operation, rawRootTaskID)
	snapshot.Product, snapshot.BudgetProfile = products[0], budgetProfile
	err = tx.QueryRow(ctx, `
SELECT
 (SELECT count(*) FROM query_records q WHERE q.task_id=t.id),
 (SELECT count(*) FROM v5_query_exposure_reservations r WHERE r.task_id=t.id AND r.status='SETTLED'),
 (SELECT count(*) FROM v5_query_observations o JOIN query_records q ON q.id=o.query_id WHERE q.task_id=t.id),
 (SELECT count(*) FROM query_receipts r JOIN query_records q ON q.id=r.query_id WHERE q.task_id=t.id),
 (SELECT count(*) FROM result_artifacts a WHERE a.task_id=t.id),
 (SELECT count(*) FROM result_artifacts a WHERE a.task_id=t.id AND a.status='AVAILABLE'),
 (SELECT count(*) FROM audit_events e WHERE e.task_id=t.id AND e.event_type IN
   ('QUERY_COMPLETED','QUERY_BUDGET_RELEASED','QUERY_RESULT_OBJECT_REGISTERED','QUERY_RESULT_CONSUMED',
    'QUERY_V5_EXPOSURE_SETTLED')),
 (SELECT count(*) FROM audit_events e WHERE e.task_id=t.id AND e.event_type IN
    ('QUERY_V5_EXPOSURE_RELEASED','QUERY_FAILED'))
FROM tasks t WHERE t.id=$1`, taskID).Scan(
		&snapshot.QueryRecords, &snapshot.Settlements, &snapshot.Observations, &snapshot.Receipts,
		&snapshot.Artifacts, &snapshot.AvailableArtifacts, &snapshot.SuccessfulAudits, &snapshot.FailureAudits)
	if err != nil {
		return snapshot, err
	}
	if err := tx.Commit(ctx); err != nil {
		return snapshot, err
	}
	snapshot.Business, err = adapter.real.businessSQLSnapshot(ctx)
	if err != nil {
		return snapshot, err
	}
	snapshot.CanonicalObjects, err = adapter.real.canonicalObjectCount(ctx)
	return snapshot, err
}

func (adapter *attackAdapter) attackRejectedQuery(ctx context.Context, operation experiment.AdapterOperation,
	taskID, requestID string) (experiment.AttackRejectedQueryEvidence, error) {
	var evidence experiment.AttackRejectedQueryEvidence
	var queryID string
	err := adapter.real.control.QueryRow(ctx, `
SELECT q.id,q.status,COALESCE(q.error_code,''),COALESCE(q.result_sha256,''),
 COALESCE((SELECT status FROM v5_query_exposure_reservations r WHERE r.query_id=q.id),''),
 (SELECT count(*) FROM encrypted_query_results r WHERE r.query_id=q.id),
 (SELECT count(*) FROM encrypted_query_result_chunks c WHERE c.query_id=q.id),
 (SELECT count(*) FROM v5_committed_materializations m WHERE m.source_query_id=q.id),
 (SELECT count(*) FROM v5_query_observations o WHERE o.query_id=q.id),
 (SELECT count(*) FROM v5_root_observations o WHERE o.first_query_id=q.id),
 (SELECT count(*) FROM result_artifacts a WHERE a.query_id=q.id),
 (SELECT count(*) FROM result_artifacts a WHERE a.query_id=q.id AND a.status='AVAILABLE'),
 (SELECT count(*) FROM audit_events e WHERE e.query_id=q.id AND e.event_type='QUERY_RESULT_CONSUMED'),
 (SELECT count(*) FROM audit_events e WHERE e.query_id=q.id AND e.event_type IN
   ('QUERY_COMPLETED','QUERY_BUDGET_RELEASED','QUERY_RESULT_OBJECT_REGISTERED','QUERY_RESULT_CONSUMED',
    'QUERY_V5_EXPOSURE_SETTLED')),
 (SELECT count(*) FROM audit_events e WHERE e.query_id=q.id AND e.event_type IN
    ('QUERY_V5_EXPOSURE_RELEASED','QUERY_FAILED')),
 (SELECT count(*) FROM query_receipts r WHERE r.query_id=q.id)
FROM query_records q WHERE q.task_id=$1 AND q.request_id=$2`, taskID, requestID).Scan(
		&queryID, &evidence.Status, &evidence.ErrorCode, &evidence.ResultSHA256, &evidence.ReservationStatus,
		&evidence.EncryptedResults, &evidence.EncryptedChunks, &evidence.Materializations,
		&evidence.QueryObservations, &evidence.RootObservations, &evidence.Artifacts,
		&evidence.AvailableArtifacts, &evidence.AvailabilityAudits, &evidence.SuccessfulAudits,
		&evidence.FailureAudits, &evidence.Receipts)
	if errors.Is(err, pgx.ErrNoRows) {
		return evidence, nil
	}
	if err != nil {
		return evidence, err
	}
	evidence.Found = true
	evidence.QueryIDHash = saltedIdentityHash(operation, "query", queryID)
	return evidence, nil
}
