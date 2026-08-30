package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/evaluation/finalv5rls"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

const rlsAdapterEvidenceVersion = "taskgate-final-v5-rls-evidence-v2"

type rlsAdapter struct {
	real         *realAdapter
	manifest     finalv5rls.Manifest
	steps        []finalv5rls.Step
	oracleTrace  []finalv5oracle.Observation
	oracleResult finalv5oracle.TraceUnion
	prefixes     []finalv5oracle.TraceUnion
	boundedStop  finalv5rls.BoundedStop
	datasetSHA   string
}

type rlsInvariantError struct{ reason string }

func (err *rlsInvariantError) Error() string { return err.reason }

type rlsPolicyEvidence struct {
	RelRowSecurity, RelForceRowSecurity bool
	SessionUser, CurrentRole            string
	TableOwnerRole                      string
	RoleCanLogin, RoleSuperuser         bool
	RoleInherit, RoleCreateDB           bool
	RoleCreateRole, RoleReplication     bool
	RoleBypassRLS                       bool
	Policies, Memberships, Grants       json.RawMessage
}

// newRLSAdapter constructs the real PostgreSQL/FORCE-RLS and TaskGate adapter.
// Its constructor fails closed unless the frozen corpus, deployment binding,
// live topology, and source-controlled oracle all agree.
func newRLSAdapter(ctx context.Context) (*rlsAdapter, error) {
	manifest, err := finalv5rls.Load()
	if err != nil {
		return nil, err
	}
	steps, err := manifest.Trace()
	if err != nil {
		return nil, err
	}
	oracleTrace := finalv5rls.OracleTrace(steps)
	oracleResult, err := finalv5oracle.Evaluate(oracleTrace)
	if err != nil {
		return nil, err
	}
	prefixes, err := finalv5oracle.EvaluatePrefixes(oracleTrace)
	if err != nil {
		return nil, err
	}
	boundedStop, err := finalv5rls.ComputeBoundedStop(steps)
	if err != nil {
		return nil, err
	}
	real, err := newRealAdapter(ctx)
	if err != nil {
		return nil, err
	}
	adapter := &rlsAdapter{real: real, manifest: manifest, steps: steps, oracleTrace: oracleTrace,
		oracleResult: oracleResult, prefixes: prefixes, boundedStop: boundedStop, datasetSHA: finalv5rls.DatasetSHA256(manifest)}
	if err := adapter.verifyFixture(ctx); err != nil {
		real.Close()
		return nil, err
	}
	return adapter, nil
}

func (adapter *rlsAdapter) Close() {
	if adapter != nil && adapter.real != nil {
		adapter.real.Close()
	}
}

func (adapter *rlsAdapter) Execute(ctx context.Context, operation experiment.AdapterOperation) experiment.Sample {
	if operation.ExperimentID != "rls" || !validRLSCell(operation) {
		return invalidSample(operation, "unsupported_source_controlled_rls_cell")
	}
	var sample experiment.Sample
	var err error
	if operation.WorkloadID == "policy-denied-control" {
		if operation.Mode == "rls" {
			sample, err = adapter.runDirectPolicyFilter(ctx, operation)
		} else {
			sample, err = adapter.runTaskgatePolicyFilter(ctx, operation)
		}
	} else if operation.Mode == "rls" {
		sample, err = adapter.runDirectTrace(ctx, operation)
	} else {
		sample, err = adapter.runTaskgateTrace(ctx, operation)
	}
	if err == nil {
		if validationErr := experiment.ValidateRLSEvidence(sample); validationErr != nil {
			err = &rlsInvariantError{reason: validationErr.Error()}
		}
	}
	if err == nil {
		return sample
	}
	return failedRLSSample(operation, sample, err)
}

func validRLSCell(operation experiment.AdapterOperation) bool {
	if operation.ExperimentID != "rls" || (operation.Mode != "rls" && operation.Mode != "unlimited" && operation.Mode != "bounded") {
		return false
	}
	return (operation.WorkloadID == "adaptive-100-v1" && operation.Scale == "100-queries") ||
		(operation.WorkloadID == "policy-denied-control" && operation.Scale == "single")
}

// A supported cell that entered a real backend is always retained as fail.
// Unsupported/pre-execution identity rejection remains invalid.
func failedRLSSample(operation experiment.AdapterOperation, sample experiment.Sample, err error) experiment.Sample {
	writeAdapterFailureDiagnostic("rls", operation, err)
	if sample.SchemaVersion == 0 {
		system := "taskgate"
		if operation.Mode == "rls" {
			system = "postgresql"
		}
		sample = baseSample(operation, system)
	}
	sample.Status = "fail"
	var invariant *rlsInvariantError
	if errors.As(err, &invariant) {
		sample.ErrorCode = "rls_invariant_violation"
		sample.Reason = "a real RLS execution completed with a preregistered invariant mismatch"
	} else {
		sample.ErrorCode = "rls_real_execution_failure"
		sample.Reason = "a frozen RLS cell entered a real backend but did not complete its authenticated evidence chain"
	}
	return sample
}

func (adapter *rlsAdapter) verifyFixture(ctx context.Context) error {
	rows, err := adapter.real.observer.Query(ctx, `
SELECT receipt_no,employee_name,department,amount::bigint
FROM final_v5_rls.expense_detail ORDER BY receipt_no`)
	if err != nil {
		return err
	}
	defer rows.Close()
	actual := make([]finalv5rls.FixtureRow, 0, 10)
	for rows.Next() {
		var row finalv5rls.FixtureRow
		if err := rows.Scan(&row.ReceiptNo, &row.EmployeeName, &row.Department, &row.Amount); err != nil {
			return err
		}
		actual = append(actual, row)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, adapter.manifest.Rows) {
		return errors.New("real Final-V5 RLS fixture differs from the source-controlled dataset")
	}
	var unlimited, bounded, mismatch int64
	err = adapter.real.observer.QueryRow(ctx, `
SELECT
 (SELECT count(*) FROM reporting.final_v5_rls_unlimited_expense_detail),
 (SELECT count(*) FROM reporting.final_v5_rls_bounded_expense_detail),
 (SELECT count(*) FROM (
   (SELECT receipt_no,department,amount FROM reporting.final_v5_rls_unlimited_expense_detail
    EXCEPT SELECT receipt_no,department,amount FROM reporting.expense_detail)
   UNION ALL
   (SELECT receipt_no,department,amount FROM reporting.expense_detail
    EXCEPT SELECT receipt_no,department,amount FROM reporting.final_v5_rls_unlimited_expense_detail)
   UNION ALL
   (SELECT receipt_no,department,amount FROM reporting.final_v5_rls_bounded_expense_detail
    EXCEPT SELECT receipt_no,department,amount FROM reporting.expense_detail)
   UNION ALL
   (SELECT receipt_no,department,amount FROM reporting.expense_detail
    EXCEPT SELECT receipt_no,department,amount FROM reporting.final_v5_rls_bounded_expense_detail)
 ) AS mismatch)`).Scan(&unlimited, &bounded, &mismatch)
	if err != nil || unlimited != 10 || bounded != 10 || mismatch != 0 {
		return errors.New("real Final-V5 RLS TaskGate publications differ from the frozen fixture")
	}
	return nil
}

func (adapter *rlsAdapter) beginRLS(ctx context.Context) (pgx.Tx, rlsPolicyEvidence, error) {
	tx, err := adapter.real.observer.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, rlsPolicyEvidence{}, err
	}
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE final_v5_rls_reader`); err != nil {
		_ = tx.Rollback(ctx)
		return nil, rlsPolicyEvidence{}, err
	}
	policy, err := loadRLSPolicyEvidence(ctx, tx)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, rlsPolicyEvidence{}, err
	}
	return tx, policy, nil
}

func loadRLSPolicyEvidence(ctx context.Context, tx pgx.Tx) (rlsPolicyEvidence, error) {
	var result rlsPolicyEvidence
	err := tx.QueryRow(ctx, `
SELECT session_user,current_user,c.relrowsecurity,c.relforcerowsecurity,owner.rolname,
       subject.rolcanlogin,subject.rolsuper,subject.rolinherit,subject.rolcreatedb,
       subject.rolcreaterole,subject.rolreplication,subject.rolbypassrls
FROM pg_class c
JOIN pg_namespace n ON n.oid=c.relnamespace
JOIN pg_roles owner ON owner.oid=c.relowner
JOIN pg_roles subject ON subject.rolname='final_v5_rls_reader'
WHERE n.nspname='final_v5_rls' AND c.relname='expense_detail' AND c.relkind='r'`).Scan(
		&result.SessionUser, &result.CurrentRole, &result.RelRowSecurity, &result.RelForceRowSecurity, &result.TableOwnerRole,
		&result.RoleCanLogin, &result.RoleSuperuser, &result.RoleInherit, &result.RoleCreateDB,
		&result.RoleCreateRole, &result.RoleReplication, &result.RoleBypassRLS)
	if err != nil {
		return result, err
	}
	var schema, table, policy, permissive, command, qual string
	var roles []string
	var withCheck *string
	err = tx.QueryRow(ctx, `
SELECT schemaname,tablename,policyname,permissive,roles,cmd,qual,with_check
FROM pg_policies WHERE schemaname='final_v5_rls' AND tablename='expense_detail'
ORDER BY policyname`).Scan(&schema, &table, &policy, &permissive, &roles, &command, &qual, &withCheck)
	if err != nil {
		return result, err
	}
	result.Policies, _ = json.Marshal([]map[string]any{{
		"schema": schema, "table": table, "policy": policy, "permissive": permissive,
		"roles": roles, "command": command, "qual": qual, "with_check": withCheck,
	}})
	var memberships int64
	if err := tx.QueryRow(ctx, `
SELECT count(*) FROM pg_auth_members m
JOIN pg_roles granted ON granted.oid=m.roleid
JOIN pg_roles member ON member.oid=m.member
WHERE granted.rolname='final_v5_rls_reader' OR member.rolname='final_v5_rls_reader'`).Scan(&memberships); err != nil {
		return result, err
	}
	if memberships != 0 {
		return result, errors.New("Final-V5 RLS role has an unapproved membership")
	}
	result.Memberships = finalv5rls.ExpectedMembershipJSON()
	var schemaUsage, receiptSelect, amountSelect, employeeSelect, tableSelect bool
	err = tx.QueryRow(ctx, `
SELECT has_schema_privilege(current_user,'final_v5_rls','USAGE'),
       has_column_privilege(current_user,'final_v5_rls.expense_detail','receipt_no','SELECT'),
       has_column_privilege(current_user,'final_v5_rls.expense_detail','amount','SELECT'),
       has_column_privilege(current_user,'final_v5_rls.expense_detail','employee_name','SELECT'),
       has_table_privilege(current_user,'final_v5_rls.expense_detail','SELECT')`).Scan(
		&schemaUsage, &receiptSelect, &amountSelect, &employeeSelect, &tableSelect)
	if err != nil {
		return result, err
	}
	if !schemaUsage || !receiptSelect || !amountSelect || employeeSelect || tableSelect {
		return result, errors.New("Final-V5 RLS role grants differ from the exact column-level policy")
	}
	result.Grants = finalv5rls.ExpectedGrantsJSON()
	if !bytesEqual(result.Policies, finalv5rls.ExpectedPoliciesJSON()) {
		return result, errors.New("live pg_policies row differs from the canonical Final-V5 policy")
	}
	return result, nil
}

func bytesEqual(left, right []byte) bool { return string(left) == string(right) }

func (adapter *rlsAdapter) baseEvidence(policy rlsPolicyEvidence) *experiment.RLSVerificationEvidence {
	return &experiment.RLSVerificationEvidence{
		Version: rlsAdapterEvidenceVersion, CorpusID: finalv5rls.CorpusID, CorpusSHA256: finalv5rls.CorpusSHA256,
		TraceSHA256: finalv5rls.TraceSHA256, DatasetID: finalv5rls.DatasetID, DatasetSHA256: adapter.datasetSHA,
		PolicySeed: adapter.manifest.Seed, PolicySchema: finalv5rls.PolicySchema, PolicyTable: finalv5rls.PolicyTable,
		PolicyName: finalv5rls.PolicyName, RelRowSecurity: policy.RelRowSecurity, RelForceRowSecurity: policy.RelForceRowSecurity,
		SessionUser: policy.SessionUser, CurrentRole: policy.CurrentRole, BaselineRole: finalv5rls.PolicyRole,
		TableOwnerRole: policy.TableOwnerRole, BaselineRoleIsOwner: policy.CurrentRole == policy.TableOwnerRole,
		BaselineRoleCanLogin: policy.RoleCanLogin, BaselineRoleSuperuser: policy.RoleSuperuser, BaselineRoleInherit: policy.RoleInherit,
		BaselineRoleCreateDB: policy.RoleCreateDB, BaselineRoleCreateRole: policy.RoleCreateRole,
		BaselineRoleReplication: policy.RoleReplication, BaselineRoleBypassRLS: policy.RoleBypassRLS,
		PoliciesJSON: policy.Policies, PoliciesSHA256: shaBytes(policy.Policies), MembershipsJSON: policy.Memberships,
		MembershipsSHA256: shaBytes(policy.Memberships), GrantsJSON: policy.Grants, GrantsSHA256: shaBytes(policy.Grants),
		OracleComputedBefore: true, OracleTrace: append([]finalv5oracle.Observation(nil), adapter.oracleTrace...),
		OracleResult: adapter.oracleResult, OraclePrefixes: append([]finalv5oracle.TraceUnion(nil), adapter.prefixes...),
		UnrelatedAuthorizationDenials: 0, ResultsAfterBudget: 0,
	}
}

func (adapter *rlsAdapter) runDirectTrace(ctx context.Context, operation experiment.AdapterOperation) (experiment.Sample, error) {
	started := time.Now()
	tx, policy, err := adapter.beginRLS(ctx)
	if err != nil {
		return experiment.Sample{}, err
	}
	defer tx.Rollback(context.Background())
	evidence := adapter.baseEvidence(policy)
	evidence.StopReason = "TRACE_COMPLETED"
	evidence.Steps = make([]experiment.RLSStepEvidence, 0, len(adapter.steps))
	var totalRows int64
	for index, wanted := range adapter.steps {
		queryStarted := time.Now()
		rows, err := queryRows(ctx, tx, wanted.DirectSQL)
		clientMS := durationMS(time.Since(queryStarted))
		if err != nil {
			return adapter.partialDirectSample(operation, evidence, started), err
		}
		verified, err := experiment.CanonicalResultHash(rows)
		if err != nil || verified != finalv5rls.VerifiedResultSHA256(wanted) {
			return adapter.partialDirectSample(operation, evidence, started), &rlsInvariantError{reason: "direct RLS result differs from the frozen typed oracle"}
		}
		step := directRLSStep(index, wanted, adapter.prefixes[index], rows)
		step.ClientMS = clientMS
		evidence.Steps = append(evidence.Steps, step)
		totalRows += int64(len(rows))
	}
	if err := tx.Commit(ctx); err != nil {
		return adapter.partialDirectSample(operation, evidence, started), err
	}
	evidence.SuccessfulQueries = len(adapter.steps)
	elapsed := durationMS(time.Since(started))
	sample := adapter.finishRLSSample(operation, "postgresql", evidence, totalRows, elapsed)
	sample.ResultSHA256 = finalv5rls.TraceResultSHA256(adapter.steps, len(adapter.steps))
	return sample, nil
}

func queryRows(ctx context.Context, tx pgx.Tx, sqlText string) ([][]any, error) {
	rows, err := tx.Query(ctx, sqlText)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([][]any, 0)
	for rows.Next() {
		row, err := rows.Values()
		if err != nil {
			return nil, err
		}
		values = append(values, row)
	}
	return values, rows.Err()
}

func directRLSStep(index int, wanted finalv5rls.Step, prefix finalv5oracle.TraceUnion, rows [][]any) experiment.RLSStepEvidence {
	step := experiment.RLSStepEvidence{
		Index: index + 1, StepID: wanted.ID, Family: wanted.Family, Variant: wanted.Variant,
		LogicalSQLSHA256: sha(wanted.LogicalSQL(finalv5rls.UnlimitedProduct)), DirectSQLSHA256: sha(wanted.DirectSQL),
		ExpectedResultSHA256: wanted.ExpectedSHA256, ObservedResultSHA256: wanted.ExpectedSHA256,
		RowCount: int64(len(rows)), ColumnCount: 1, ScalarInt64: cloneInt64(wanted.Scalar), Accepted: true, OraclePrefix: prefix,
		DecisionPreviousStep: wanted.Decision.PreviousStep, DecisionPreviousValue: wanted.Decision.PreviousValue,
		DecisionRule: wanted.Decision.Rule, DecisionThreshold: wanted.Decision.Threshold,
	}
	return step
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func (adapter *rlsAdapter) partialDirectSample(operation experiment.AdapterOperation,
	evidence *experiment.RLSVerificationEvidence, started time.Time) experiment.Sample {
	return adapter.finishRLSSample(operation, "postgresql", evidence, 0, durationMS(time.Since(started)))
}

func (adapter *rlsAdapter) runTaskgateTrace(ctx context.Context, operation experiment.AdapterOperation) (experiment.Sample, error) {
	product, profile, maxOutcome := rlsTaskgateBinding(operation.Mode)
	created, err := adapter.real.provisionCatalogTask(ctx, "Final V5 RLS "+operation.Mode+" "+operation.PairID,
		product, []string{"receipt_no", "amount"}, "")
	if err != nil {
		return experiment.Sample{}, err
	}
	budgets := modeBudget(operation.Mode)
	if created.RootTaskID != created.TaskID || created.BudgetProfile != profile || created.Budget.MaxQueries != 110 ||
		created.Budget.MaxReleaseFacts != budgets.release || created.Budget.MaxInfluenceFacts != budgets.dependency ||
		created.Budget.MaxOutcomeFacts != maxOutcome {
		return experiment.Sample{}, &rlsInvariantError{reason: "RLS task provisioning selected an unexpected product/profile/resource/exposure budget"}
	}
	policy, err := adapter.policyEvidence(ctx)
	if err != nil {
		return experiment.Sample{}, err
	}
	evidence := adapter.baseEvidence(policy)
	evidence.Product, evidence.BudgetProfile = product, profile
	evidence.RootTaskIDHash = saltedTaskHash(operation, created.RootTaskID)
	evidence.Steps = make([]experiment.RLSStepEvidence, 0, len(adapter.steps))
	started := time.Now()
	var totalRows int64
	for index, wanted := range adapter.steps {
		before, err := adapter.controlSnapshot(ctx, operation, created.TaskID, product, profile)
		if err != nil {
			return adapter.partialTaskgateSample(operation, evidence, started), err
		}
		requestID := fmt.Sprintf("final-v5-rls-%s-%03d", sha(operation.SampleID)[:16], index+1)
		step := experiment.RLSStepEvidence{
			Index: index + 1, StepID: wanted.ID, Family: wanted.Family, Variant: wanted.Variant,
			LogicalSQLSHA256: sha(wanted.LogicalSQL(product)), DirectSQLSHA256: sha(wanted.DirectSQL),
			ExpectedResultSHA256: wanted.ExpectedSHA256, OraclePrefix: adapter.prefixes[index],
			DecisionPreviousStep: wanted.Decision.PreviousStep, DecisionPreviousValue: wanted.Decision.PreviousValue,
			DecisionRule: wanted.Decision.Rule, DecisionThreshold: wanted.Decision.Threshold,
			RequestIDHash: saltedIdentityHash(operation, "request", requestID), RootTaskIDHash: evidence.RootTaskIDHash, Before: &before,
		}
		queryStarted := time.Now()
		var response queryResponse
		callErr := adapter.real.alice.call(ctx, "query_sql", map[string]any{
			"task_id": created.TaskID, "request_id": requestID, "sql": wanted.LogicalSQL(product),
		}, &response)
		step.ClientMS = durationMS(time.Since(queryStarted))
		if callErr != nil {
			var structured *mcpCallError
			if !errors.As(callErr, &structured) {
				return adapter.partialTaskgateSample(operation, evidence, started), callErr
			}
			after, snapshotErr := adapter.controlSnapshot(ctx, operation, created.TaskID, product, profile)
			if snapshotErr != nil {
				return adapter.partialTaskgateSample(operation, evidence, started), snapshotErr
			}
			rejected, projectionErr := (&attackAdapter{real: adapter.real}).attackRejectedQuery(ctx, operation, created.TaskID, requestID)
			if projectionErr != nil {
				return adapter.partialTaskgateSample(operation, evidence, started), projectionErr
			}
			step.After, step.RejectedQuery, step.Rejected = &after, &rejected, true
			step.QueryIDHash = rejected.QueryIDHash
			step.ObservedErrorCode = structured.Code
			step.ObservedErrorReason = safeRLSBudgetErrorReason(structured.Code, adapter.boundedStop)
			evidence.Steps = append(evidence.Steps, step)
			if operation.Mode != "bounded" || index+1 != adapter.boundedStop.Index || structured.Code != "EXPOSURE_BUDGET_EXHAUSTED" {
				return adapter.partialTaskgateSample(operation, evidence, started), &rlsInvariantError{reason: "TaskGate RLS rejection differs from the recomputed first exposure crossing"}
			}
			evidence.SuccessfulQueries, evidence.FirstRejectionIndex = adapter.boundedStop.SuccessfulQueries, adapter.boundedStop.Index
			evidence.StopReason = "EXPOSURE_BUDGET_EXHAUSTED"
			evidence.FinalRoot = cloneRoot(after.Root)
			elapsed := durationMS(time.Since(started))
			sample := adapter.finishRLSSample(operation, "taskgate", evidence, totalRows, elapsed)
			applyRLSRoot(&sample, after.Root)
			sample.Rejected, sample.RejectedNoResult, sample.RejectedNoArtifact, sample.RejectedNoSuccessfulAudit = true, true, true, true
			return sample, nil
		}
		rootAfterQuery, err := adapter.real.rootLedgerSnapshot(ctx, created.TaskID)
		if err != nil {
			return adapter.partialTaskgateSample(operation, evidence, started), err
		}
		released, err := adapter.real.completeTaskgateSample(ctx, operation, &pairState{taskID: created.TaskID}, before.Root,
			rootAfterQuery, queryStarted, durationMS(time.Since(queryStarted)), wanted.LogicalSQL(product), response)
		if err != nil {
			return adapter.partialTaskgateSample(operation, evidence, started), err
		}
		if released.ResultSHA256 != finalv5rls.VerifiedResultSHA256(wanted) || released.RowCount != int64(len(wanted.ExpectedRows)) || released.ColumnCount != 1 {
			return adapter.partialTaskgateSample(operation, evidence, started), &rlsInvariantError{reason: "released TaskGate RLS result differs from the frozen typed oracle"}
		}
		after, err := adapter.controlSnapshot(ctx, operation, created.TaskID, product, profile)
		if err != nil {
			return adapter.partialTaskgateSample(operation, evidence, started), err
		}
		if after.Root != rootAfterQuery {
			return adapter.partialTaskgateSample(operation, evidence, started), errors.New("RLS root changed while verifying the released artifact")
		}
		populateTaskgateRLSStep(&step, wanted, response, released, after)
		evidence.Steps = append(evidence.Steps, step)
		totalRows += released.RowCount
		if operation.Mode == "bounded" && index+1 >= adapter.boundedStop.Index {
			return adapter.partialTaskgateSample(operation, evidence, started), &rlsInvariantError{reason: "bounded RLS crossing candidate released a result instead of failing closed"}
		}
	}
	evidence.SuccessfulQueries, evidence.StopReason = len(adapter.steps), "TRACE_COMPLETED"
	last := evidence.Steps[len(evidence.Steps)-1]
	evidence.FinalRoot = cloneRoot(last.After.Root)
	elapsed := durationMS(time.Since(started))
	sample := adapter.finishRLSSample(operation, "taskgate", evidence, totalRows, elapsed)
	applyRLSRoot(&sample, last.After.Root)
	applyRLSLastArtifact(&sample, last)
	sample.ResultSHA256 = finalv5rls.TraceResultSHA256(adapter.steps, len(adapter.steps))
	return sample, nil
}

// The production exposure rejection deliberately exposes only a stable code
// and client-safe message. The exact crossed root dimension is proved by the
// independent candidate/budget oracle and projected as a stable evidence
// reason, just as the Attack adapter projects its root-outcome crossing.
func safeRLSBudgetErrorReason(code string, stop finalv5rls.BoundedStop) string {
	if code == "EXPOSURE_BUDGET_EXHAUSTED" {
		return stop.ErrorReason
	}
	return ""
}

func rlsTaskgateBinding(mode string) (product, profile string, maxOutcome int64) {
	if mode == "bounded" {
		return finalv5rls.BoundedProduct, finalv5rls.BoundedProfile, finalv5rls.BoundedMaxOutcomeFacts
	}
	return finalv5rls.UnlimitedProduct, finalv5rls.UnlimitedProfile, 1000
}

func modeBudget(mode string) struct{ release, dependency int64 } {
	if mode == "bounded" {
		return struct{ release, dependency int64 }{finalv5rls.BoundedMaxReleaseFacts, finalv5rls.BoundedMaxDependencyFacts}
	}
	return struct{ release, dependency int64 }{1000, 1000}
}

func populateTaskgateRLSStep(step *experiment.RLSStepEvidence, wanted finalv5rls.Step, response queryResponse,
	released experiment.Sample, after experiment.RLSControlSnapshot) {
	step.Accepted, step.After = true, &after
	step.ObservedResultSHA256, step.VerifiedResultSHA256 = wanted.ExpectedSHA256, released.ResultSHA256
	step.RowCount, step.ColumnCount, step.ScalarInt64 = released.RowCount, released.ColumnCount, cloneInt64(wanted.Scalar)
	step.QueryIDHash = released.BaselineVerification.VerifierManifest.QueryIDHash
	step.ResultIDHash = released.BaselineVerification.VerifierManifest.ResultIDHash
	step.PlanSHA256, step.ObservationSHA256 = response.PlanDigest, response.Exposure.ObservationSHA256
	step.ReleaseSetSHA256, step.DependencySetSHA256, step.OutcomeSetSHA256 = released.ReleaseSetSHA256, released.DependencySetSHA256, released.OutcomeSetSHA256
	step.ActualReleaseFacts, step.ChargedReleaseFacts = released.ActualReleaseFacts, released.ChargedReleaseFacts
	step.ActualDependencyFacts, step.ChargedDependencyFacts = released.ActualDependencyFacts, released.ChargedDependencyFacts
	step.ActualOutcomeFacts, step.ChargedOutcomeFacts = released.ActualOutcomeFacts, released.ChargedOutcomeFacts
	step.PredicateAtomCount, step.CompositeCount = released.PredicateAtomCount, released.CompositeCount
	step.SemanticReplay, step.IdempotentReplay = released.SemanticReplay, released.IdempotentReplay
	step.RootTaskIDHash = released.RootTaskIDHash
	step.ArtifactSHA256, step.ObjectSHA256 = released.ArtifactSHA256, released.ObjectSHA256
	step.ParquetBytes, step.EncryptedObjectBytes = released.ParquetBytes, released.EncryptedObjectBytes
	step.ReceiptVersion, step.ReceiptSHA256 = released.ReceiptVersion, released.ReceiptSHA256
	step.ArtifactIntentSHA256, step.AvailabilitySHA256 = released.ArtifactIntentSHA256, released.AvailabilityAuditSHA256
	step.Verification = released.BaselineVerification
}

func cloneRoot(root experiment.RootLedgerSnapshot) *experiment.RootLedgerSnapshot {
	value := root
	return &value
}

func (adapter *rlsAdapter) partialTaskgateSample(operation experiment.AdapterOperation,
	evidence *experiment.RLSVerificationEvidence, started time.Time) experiment.Sample {
	return adapter.finishRLSSample(operation, "taskgate", evidence, 0, durationMS(time.Since(started)))
}

func (adapter *rlsAdapter) finishRLSSample(operation experiment.AdapterOperation, system string,
	evidence *experiment.RLSVerificationEvidence, totalRows int64, elapsed float64) experiment.Sample {
	sample := baseSample(operation, system)
	sample.ClientAvailableMS, sample.ClientFullDrainMS = elapsed, elapsed
	sample.PipelineMS["execute_and_derive"], sample.PipelineMS["server_total"] = elapsed, elapsed
	sample.RowCount, sample.ColumnCount = totalRows, 1
	sample.PhysicalSQLSHA256 = sha(finalv5rls.TraceSHA256 + "\x00" + system)
	sample.LogicalSQLSHA256, sample.QueryPlanSHA256 = finalv5rls.TraceSHA256, sha(finalv5rls.CorpusID+"\x00"+operation.Mode)
	sample.RootTaskIDHash = evidence.RootTaskIDHash
	sample.Trace = buildRLSTrace(system, evidence.Product, adapter.steps, evidence.Steps)
	sample.RLSVerification, sample.Status = evidence, "pass"
	return sample
}

func buildRLSTrace(system, product string, corpus []finalv5rls.Step, evidence []experiment.RLSStepEvidence) []experiment.TraceStep {
	trace := make([]experiment.TraceStep, 0, len(evidence))
	state := sha("TASKGATE-FINAL-V5-RLS-TRACE-STATE-V1\x00" + finalv5rls.TraceSHA256)
	for index, step := range evidence {
		concrete := corpus[index].DirectSQL
		if system == "taskgate" {
			concrete = corpus[index].LogicalSQL(product)
		}
		next := "TASKGATE-FINAL-V5-RLS-END-V1"
		if index+1 < len(corpus) {
			next = corpus[index+1].DirectSQL
			if system == "taskgate" {
				next = corpus[index+1].LogicalSQL(product)
			}
		}
		transition := step.ObservedResultSHA256
		if step.Rejected {
			transition = step.ObservedErrorCode + "\x00" + step.ObservedErrorReason
		}
		trace = append(trace, experiment.TraceStep{Index: index + 1, ConcreteSQL: concrete, PriorStateSHA256: state,
			ResultSHA256: step.ObservedResultSHA256, NextSQLSHA256: sha(next), PlanSHA256: step.PlanSHA256,
			ObservationSHA256: step.ObservationSHA256, ReleaseSetSHA256: step.ReleaseSetSHA256,
			DependencySetSHA256: step.DependencySetSHA256, OutcomeSetSHA256: step.OutcomeSetSHA256,
			Rejected: step.Rejected, NoResult: step.Rejected, NoAvailableArtifact: step.Rejected, NoSuccessfulAudit: step.Rejected})
		state = sha(state + "\x00" + transition)
	}
	return trace
}

func applyRLSRoot(sample *experiment.Sample, root experiment.RootLedgerSnapshot) {
	sample.RootEpochAfter = root.Epoch
	sample.RootSetSHA256After = rootSetDigest(root)
	sample.ReleaseSetSHA256, sample.DependencySetSHA256, sample.OutcomeSetSHA256 = root.ReleaseSetSHA256, root.DependencySetSHA256, root.OutcomeSetSHA256
	sample.ActualReleaseFacts, sample.ActualDependencyFacts, sample.ActualOutcomeFacts = root.ReleaseCardinality, root.DependencyCardinality, root.OutcomeCardinality
}

func applyRLSLastArtifact(sample *experiment.Sample, step experiment.RLSStepEvidence) {
	sample.ArtifactSHA256, sample.ObjectSHA256 = step.ArtifactSHA256, step.ObjectSHA256
	sample.ParquetBytes, sample.EncryptedObjectBytes = step.ParquetBytes, step.EncryptedObjectBytes
	sample.ReceiptVersion, sample.ReceiptSHA256 = step.ReceiptVersion, step.ReceiptSHA256
	sample.ArtifactIntentSHA256, sample.AvailabilityAuditSHA256 = step.ArtifactIntentSHA256, step.AvailabilitySHA256
	sample.ReceiptVerified, sample.ArtifactAvailable = true, true
}

func (adapter *rlsAdapter) policyEvidence(ctx context.Context) (rlsPolicyEvidence, error) {
	tx, policy, err := adapter.beginRLS(ctx)
	if err != nil {
		return policy, err
	}
	if err := tx.Commit(ctx); err != nil {
		return policy, err
	}
	return policy, nil
}

func (adapter *rlsAdapter) controlSnapshot(ctx context.Context, operation experiment.AdapterOperation,
	taskID, product, profile string) (experiment.RLSControlSnapshot, error) {
	var snapshot experiment.RLSControlSnapshot
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
SELECT t.id,t.root_task_id,g.approved_products_json,
       b.max_queries,b.max_rows,b.used_queries,b.used_rows,b.reserved_queries,b.reserved_rows,
       g.max_release_facts,g.max_influence_facts,g.max_outcome_facts
FROM tasks t JOIN task_grants g ON g.task_id=t.id JOIN budget_ledger b ON b.task_id=t.id
WHERE t.id=$1`, taskID).Scan(&rawTaskID, &rawRootTaskID, &productsJSON,
		&snapshot.MaxQueries, &snapshot.MaxRows, &snapshot.UsedQueries, &snapshot.UsedRows, &snapshot.ReservedQueries, &snapshot.ReservedRows,
		&snapshot.MaxReleaseFacts, &snapshot.MaxDependencyFacts, &snapshot.MaxOutcomeFacts)
	if err != nil {
		return snapshot, err
	}
	var products []string
	if json.Unmarshal(productsJSON, &products) != nil || len(products) != 1 || products[0] != product {
		return snapshot, errors.New("RLS task grant does not bind exactly its frozen product")
	}
	snapshot.TaskIDHash, snapshot.RootTaskIDHash = saltedTaskHash(operation, rawTaskID), saltedTaskHash(operation, rawRootTaskID)
	snapshot.Product, snapshot.BudgetProfile = product, profile
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
FROM tasks t WHERE t.id=$1`, taskID).Scan(&snapshot.QueryRecords, &snapshot.Settlements, &snapshot.Observations,
		&snapshot.Receipts, &snapshot.Artifacts, &snapshot.AvailableArtifacts, &snapshot.SuccessfulAudits, &snapshot.FailureAudits)
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

func (adapter *rlsAdapter) runDirectPolicyFilter(ctx context.Context, operation experiment.AdapterOperation) (experiment.Sample, error) {
	started := time.Now()
	tx, policy, err := adapter.beginRLS(ctx)
	if err != nil {
		return experiment.Sample{}, err
	}
	defer tx.Rollback(context.Background())
	evidence, wanted, authorization, err := adapter.policyFilterEvidence(policy)
	if err != nil {
		return experiment.Sample{}, err
	}
	policyQueryStarted := time.Now()
	rows, err := queryRows(ctx, tx, wanted.DirectSQL)
	policyClientMS := durationMS(time.Since(policyQueryStarted))
	if err != nil {
		return experiment.Sample{}, err
	}
	verified, hashErr := experiment.CanonicalResultHash(rows)
	if hashErr != nil || len(rows) != 0 || verified != finalv5rls.VerifiedResultSHA256(wanted) {
		return experiment.Sample{}, &rlsInvariantError{reason: "direct FORCE RLS policy-control target was not filtered to an empty result"}
	}
	policyStep := directRLSStep(0, wanted, evidence.OraclePrefixes[0], rows)
	policyStep.ClientMS = policyClientMS
	deniedStarted := time.Now()
	deniedRows, deniedErr := tx.Query(ctx, authorization.DirectSQL)
	if deniedRows != nil {
		// pgx may defer a PostgreSQL execution error until Rows is advanced.
		// Observe that terminal error before Close so a real 42501 cannot be
		// mistaken for a successful authorization-control statement.
		_ = deniedRows.Next()
		if deniedErr == nil {
			deniedErr = deniedRows.Err()
		}
		deniedRows.Close()
	}
	deniedClientMS := durationMS(time.Since(deniedStarted))
	var pgErr *pgconn.PgError
	if !errors.As(deniedErr, &pgErr) || pgErr.Code != "42501" {
		return experiment.Sample{}, &rlsInvariantError{reason: "direct authorization control was not rejected with SQLSTATE 42501"}
	}
	deniedStep := experiment.RLSStepEvidence{
		Index: 2, StepID: authorization.ID, Family: authorization.Family, Variant: authorization.Variant,
		LogicalSQLSHA256: sha(authorization.LogicalSQL(finalv5rls.UnlimitedProduct)), DirectSQLSHA256: sha(authorization.DirectSQL),
		Rejected: true, ObservedErrorCode: "42501", ObservedErrorReason: "UNAPPROVED_COLUMN",
		ClientMS: deniedClientMS, OraclePrefix: evidence.OraclePrefixes[1],
	}
	evidence.Steps = []experiment.RLSStepEvidence{policyStep, deniedStep}
	evidence.NegativeControl.AuthorizationSQLSHA256 = sha(authorization.DirectSQL)
	evidence.NegativeControl.ExpectedAuthorizationErrorCode = "42501"
	evidence.NegativeControl.ObservedAuthorizationErrorCode = "42501"
	evidence.NegativeControl.ObservedAuthorizationErrorReason = "UNAPPROVED_COLUMN"
	evidence.NegativeControl.AuthorizationRejectedNoRows = true
	_ = tx.Rollback(ctx)
	elapsed := durationMS(time.Since(started))
	sample := adapter.finishDirectPolicyFilterSample(operation, evidence, wanted, authorization, elapsed)
	return sample, nil
}

func (adapter *rlsAdapter) runTaskgatePolicyFilter(ctx context.Context, operation experiment.AdapterOperation) (experiment.Sample, error) {
	product, profile, maxOutcome := rlsTaskgateBinding(operation.Mode)
	created, err := adapter.real.provisionCatalogTask(ctx, "Final V5 RLS policy filter "+operation.Mode+" "+operation.PairID,
		product, []string{"receipt_no", "amount"}, "")
	if err != nil {
		return experiment.Sample{}, err
	}
	budgets := modeBudget(operation.Mode)
	if created.RootTaskID != created.TaskID || created.BudgetProfile != profile || created.Budget.MaxQueries != 110 ||
		created.Budget.MaxReleaseFacts != budgets.release || created.Budget.MaxInfluenceFacts != budgets.dependency ||
		created.Budget.MaxOutcomeFacts != maxOutcome {
		return experiment.Sample{}, &rlsInvariantError{reason: "RLS negative-control task has the wrong frozen profile"}
	}
	policy, err := adapter.policyEvidence(ctx)
	if err != nil {
		return experiment.Sample{}, err
	}
	evidence, wanted, authorization, err := adapter.policyFilterEvidence(policy)
	if err != nil {
		return experiment.Sample{}, err
	}
	evidence.Product, evidence.BudgetProfile, evidence.RootTaskIDHash = product, profile, saltedTaskHash(operation, created.RootTaskID)
	sqlText := wanted.LogicalSQL(product)
	requestID := "final-v5-rls-policy-filter-" + sha(operation.SampleID)[:16]
	before, err := adapter.controlSnapshot(ctx, operation, created.TaskID, product, profile)
	if err != nil {
		return experiment.Sample{}, err
	}
	step := experiment.RLSStepEvidence{
		Index: 1, StepID: wanted.ID, Family: wanted.Family, Variant: wanted.Variant,
		LogicalSQLSHA256: sha(sqlText), DirectSQLSHA256: sha(wanted.DirectSQL),
		ExpectedResultSHA256: wanted.ExpectedSHA256, OraclePrefix: evidence.OraclePrefixes[0],
		RequestIDHash: saltedIdentityHash(operation, "request", requestID), Before: &before,
	}
	started := time.Now()
	var response queryResponse
	if err := adapter.real.alice.call(ctx, "query_sql", map[string]any{
		"task_id": created.TaskID, "request_id": requestID, "sql": sqlText,
	}, &response); err != nil {
		return experiment.Sample{}, err
	}
	step.ClientMS = durationMS(time.Since(started))
	rootAfterQuery, err := adapter.real.rootLedgerSnapshot(ctx, created.TaskID)
	if err != nil {
		return experiment.Sample{}, err
	}
	released, err := adapter.real.completeTaskgateSample(ctx, operation, &pairState{taskID: created.TaskID}, before.Root,
		rootAfterQuery, started, durationMS(time.Since(started)), sqlText, response)
	if err != nil {
		return experiment.Sample{}, err
	}
	if released.RowCount != 0 || released.ColumnCount != 1 || released.ResultSHA256 != finalv5rls.VerifiedResultSHA256(wanted) {
		return experiment.Sample{}, &rlsInvariantError{reason: "TaskGate mandatory scope did not filter the policy-control target to an authenticated empty result"}
	}
	after, err := adapter.controlSnapshot(ctx, operation, created.TaskID, product, profile)
	if err != nil {
		return experiment.Sample{}, err
	}
	if after.Root != rootAfterQuery {
		return experiment.Sample{}, errors.New("RLS policy-control root changed while verifying the empty artifact")
	}
	populateTaskgateRLSStep(&step, wanted, response, released, after)
	deniedSQL := authorization.LogicalSQL(product)
	deniedRequestID := "final-v5-rls-authorization-denied-" + sha(operation.SampleID)[:16]
	var deniedResponse queryResponse
	deniedStarted := time.Now()
	deniedErr := adapter.real.alice.call(ctx, "query_sql", map[string]any{
		"task_id": created.TaskID, "request_id": deniedRequestID, "sql": deniedSQL,
	}, &deniedResponse)
	deniedClientMS := durationMS(time.Since(deniedStarted))
	var structured *mcpCallError
	if !errors.As(deniedErr, &structured) || structured.Code != "COLUMN_NOT_APPROVED" {
		return experiment.Sample{}, &rlsInvariantError{reason: "TaskGate authorization control was not rejected as COLUMN_NOT_APPROVED"}
	}
	afterDenied, err := adapter.controlSnapshot(ctx, operation, created.TaskID, product, profile)
	if err != nil {
		return experiment.Sample{}, err
	}
	rejected, err := (&attackAdapter{real: adapter.real}).attackRejectedQuery(ctx, operation, created.TaskID, deniedRequestID)
	if err != nil {
		return experiment.Sample{}, err
	}
	deniedStep := experiment.RLSStepEvidence{
		Index: 2, StepID: authorization.ID, Family: authorization.Family, Variant: authorization.Variant,
		LogicalSQLSHA256: sha(deniedSQL), DirectSQLSHA256: sha(authorization.DirectSQL),
		Rejected: true, ObservedErrorCode: structured.Code, ObservedErrorReason: "UNAPPROVED_COLUMN", ClientMS: deniedClientMS,
		RequestIDHash: saltedIdentityHash(operation, "request", deniedRequestID), RootTaskIDHash: evidence.RootTaskIDHash,
		Before: &after, After: &afterDenied, OraclePrefix: evidence.OraclePrefixes[1], RejectedQuery: &rejected,
	}
	evidence.Steps = []experiment.RLSStepEvidence{step, deniedStep}
	evidence.NegativeControl.AuthorizationSQLSHA256 = sha(deniedSQL)
	evidence.NegativeControl.ExpectedAuthorizationErrorCode = "COLUMN_NOT_APPROVED"
	evidence.NegativeControl.ObservedAuthorizationErrorCode = structured.Code
	evidence.NegativeControl.ObservedAuthorizationErrorReason = "UNAPPROVED_COLUMN"
	evidence.NegativeControl.AuthorizationRejectedNoRows = true
	evidence.NegativeControl.Before, evidence.NegativeControl.After = &after, &afterDenied
	evidence.NegativeControl.RejectedQuery = &rejected
	evidence.FinalRoot = cloneRoot(afterDenied.Root)
	sample := released
	sample.RLSVerification = evidence
	sample.Trace = buildPolicyFilterTrace("taskgate", product, wanted, authorization, evidence.Steps)
	applyRLSRoot(&sample, afterDenied.Root)
	markPolicyControlRejected(&sample)
	sample.Status = "pass"
	return sample, nil
}

func (adapter *rlsAdapter) policyFilterEvidence(policy rlsPolicyEvidence) (*experiment.RLSVerificationEvidence,
	finalv5rls.Step, finalv5rls.AuthorizationControl, error) {
	wanted, err := adapter.manifest.PolicyInvisibleStep()
	if err != nil {
		return nil, finalv5rls.Step{}, finalv5rls.AuthorizationControl{}, err
	}
	authorization := finalv5rls.PolicyAuthorizationControl()
	target, err := adapter.manifest.PolicyInvisibleRow()
	if err != nil {
		return nil, finalv5rls.Step{}, finalv5rls.AuthorizationControl{}, err
	}
	evidence := adapter.baseEvidence(policy)
	evidence.OracleTrace = []finalv5oracle.Observation{wanted.Oracle, {}}
	evidence.OracleResult, err = finalv5oracle.Evaluate(evidence.OracleTrace)
	if err != nil {
		return nil, finalv5rls.Step{}, finalv5rls.AuthorizationControl{}, err
	}
	evidence.OraclePrefixes, err = finalv5oracle.EvaluatePrefixes(evidence.OracleTrace)
	if err != nil {
		return nil, finalv5rls.Step{}, finalv5rls.AuthorizationControl{}, err
	}
	evidence.SuccessfulQueries, evidence.FirstRejectionIndex = 1, 2
	evidence.UnrelatedAuthorizationDenials, evidence.StopReason = 1, "POLICY_FILTER_AND_AUTHORIZATION_REJECTION"
	evidence.NegativeControl = &experiment.RLSNegativeEvidence{
		TargetReceiptNo: finalv5rls.PolicyInvisibleReceipt, TargetDepartment: target.Department,
		PolicyDepartment: adapter.manifest.PolicyDepartment, TargetPresentOutsidePolicy: true, PolicyFiltered: true,
		ExpectedRowCount: 0, ObservedRowCount: 0, ExpectedResultSHA256: wanted.ExpectedSHA256,
		ObservedResultSHA256: wanted.ExpectedSHA256,
	}
	return evidence, wanted, authorization, nil
}

func (adapter *rlsAdapter) finishDirectPolicyFilterSample(operation experiment.AdapterOperation,
	evidence *experiment.RLSVerificationEvidence, wanted finalv5rls.Step, authorization finalv5rls.AuthorizationControl,
	elapsed float64) experiment.Sample {
	sample := baseSample(operation, "postgresql")
	sample.ClientAvailableMS, sample.ClientFullDrainMS = elapsed, elapsed
	sample.PipelineMS["execute_and_derive"], sample.PipelineMS["server_total"] = elapsed, elapsed
	sample.PhysicalSQLSHA256 = sha(authorization.DirectSQL)
	sample.LogicalSQLSHA256 = sha(authorization.LogicalSQL(finalv5rls.UnlimitedProduct))
	sample.QueryPlanSHA256 = sha("TASKGATE-FINAL-V5-RLS-POLICY-FILTER-AND-REJECTION-V1")
	sample.Trace = buildPolicyFilterTrace("postgresql", "", wanted, authorization, evidence.Steps)
	sample.RLSVerification, sample.Status = evidence, "pass"
	markPolicyControlRejected(&sample)
	return sample
}

func markPolicyControlRejected(sample *experiment.Sample) {
	sample.RowCount, sample.ColumnCount, sample.ResultSHA256 = 0, 0, ""
	sample.ArtifactSHA256, sample.ObjectSHA256 = "", ""
	sample.ParquetBytes, sample.EncryptedObjectBytes = 0, 0
	sample.ReceiptVersion, sample.ReceiptSHA256, sample.ArtifactIntentSHA256, sample.AvailabilityAuditSHA256 = "", "", "", ""
	sample.ReceiptVerified, sample.ArtifactAvailable, sample.BaselineVerification = false, false, nil
	sample.Rejected, sample.RejectedNoResult, sample.RejectedNoArtifact, sample.RejectedNoSuccessfulAudit = true, true, true, true
}

func buildPolicyFilterTrace(system, product string, wanted finalv5rls.Step, authorization finalv5rls.AuthorizationControl,
	steps []experiment.RLSStepEvidence) []experiment.TraceStep {
	policySQL, deniedSQL := wanted.DirectSQL, authorization.DirectSQL
	if system == "taskgate" {
		policySQL, deniedSQL = wanted.LogicalSQL(product), authorization.LogicalSQL(product)
	}
	state := sha("TASKGATE-FINAL-V5-RLS-POLICY-FILTER-V1")
	afterPolicy := sha(state + "\x00" + steps[0].ObservedResultSHA256)
	return []experiment.TraceStep{
		{Index: 1, ConcreteSQL: policySQL, PriorStateSHA256: state, ResultSHA256: steps[0].ObservedResultSHA256,
			NextSQLSHA256: sha(deniedSQL), PlanSHA256: steps[0].PlanSHA256, ObservationSHA256: steps[0].ObservationSHA256,
			ReleaseSetSHA256: steps[0].ReleaseSetSHA256, DependencySetSHA256: steps[0].DependencySetSHA256,
			OutcomeSetSHA256: steps[0].OutcomeSetSHA256},
		{Index: 2, ConcreteSQL: deniedSQL, PriorStateSHA256: afterPolicy,
			NextSQLSHA256: sha("TASKGATE-FINAL-V5-RLS-POLICY-FILTER-END-V1"), Rejected: true,
			NoResult: true, NoAvailableArtifact: true, NoSuccessfulAudit: true},
	}
}
