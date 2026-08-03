package experiment

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/evaluation/finalv5rls"
)

func TestRLSNestedVerificationSchemaMatchesExactWireStructs(t *testing.T) {
	value, err := os.ReadFile(filepath.Join("..", "..", "final-v5-wsl2", "schema", "sample-v1.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(value, &schema); err != nil {
		t.Fatal(err)
	}
	definitions := objectMap(t, schema["$defs"], "sample definitions")
	for name, wireType := range map[string]reflect.Type{
		"rlsOracleObservation": reflect.TypeOf(finalv5oracle.Observation{}),
		"rlsOracleDimension":   reflect.TypeOf(finalv5oracle.Dimension{}),
		"rlsOracleTraceUnion":  reflect.TypeOf(finalv5oracle.TraceUnion{}),
		"rlsControlSnapshot":   reflect.TypeOf(RLSControlSnapshot{}),
		"rlsNegativeControl":   reflect.TypeOf(RLSNegativeEvidence{}),
		"rlsStep":              reflect.TypeOf(RLSStepEvidence{}),
	} {
		t.Run(name, func(t *testing.T) {
			definition := objectMap(t, definitions[name], name)
			if definition["type"] != "object" || definition["additionalProperties"] != false {
				t.Fatal("RLS nested evidence must be a strict object")
			}
			wantProperties, wantRequired := jsonFields(wireType)
			gotProperties := sortedKeys(objectMap(t, definition["properties"], name+" properties"))
			gotRequired := stringArray(t, definition["required"], name+" required")
			sort.Strings(gotRequired)
			if !reflect.DeepEqual(gotProperties, wantProperties) || !reflect.DeepEqual(gotRequired, wantRequired) {
				t.Fatalf("schema properties/required = %v/%v, Go fields = %v/%v", gotProperties, gotRequired, wantProperties, wantRequired)
			}
		})
	}
}

func TestRLSDirectEvidenceBindsExactHundredStepCorpusAndMutations(t *testing.T) {
	sample := validDirectRLSSample(t)
	if err := ValidateRLSEvidence(sample); err != nil {
		t.Fatalf("valid direct RLS evidence: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*Sample)
	}{
		{"role can login", func(value *Sample) { value.RLSVerification.BaselineRoleCanLogin = true }},
		{"force RLS absent", func(value *Sample) { value.RLSVerification.RelForceRowSecurity = false }},
		{"policy bytes", func(value *Sample) { value.RLSVerification.PoliciesJSON = []byte(`[]`) }},
		{"oracle member", func(value *Sample) { value.RLSVerification.OracleTrace[0].Release[0] = digestForRLSTest("mutated") }},
		{"prefix cardinality", func(value *Sample) { value.RLSVerification.OraclePrefixes[0].Release.Cardinality++ }},
		{"concrete query", func(value *Sample) { value.Trace[9].ConcreteSQL += " " }},
		{"adaptive input", func(value *Sample) { value.RLSVerification.Steps[96].DecisionPreviousValue++ }},
		{"result", func(value *Sample) { value.RLSVerification.Steps[20].ObservedResultSHA256 = digestForRLSTest("wrong") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := cloneRLSSample(t, sample)
			test.mutate(&mutated)
			if err := ValidateRLSEvidence(mutated); err == nil {
				t.Fatal("mutated RLS evidence was accepted")
			}
		})
	}
}

func TestRLSPolicyControlRequiresBothForceRLSFilteringAndAuthorizationRejection(t *testing.T) {
	sample := validDirectRLSPolicyDeniedSample(t)
	if err := ValidateRLSEvidence(sample); err != nil {
		t.Fatalf("valid direct policy control: %v", err)
	}
	mutated := cloneRLSSample(t, sample)
	mutated.RLSVerification.NegativeControl.PolicyFiltered = false
	if err := ValidateRLSEvidence(mutated); err == nil {
		t.Fatal("authorization rejection without FORCE RLS filtering was accepted")
	}
	mutated = cloneRLSSample(t, sample)
	mutated.RLSVerification.NegativeControl.ObservedAuthorizationErrorCode = ""
	if err := ValidateRLSEvidence(mutated); err == nil {
		t.Fatal("FORCE RLS filtering without the explicit rejection was accepted")
	}
	mutated = cloneRLSSample(t, sample)
	mutated.Trace[1].NoResult = false
	if err := ValidateRLSEvidence(mutated); err == nil {
		t.Fatal("authorization rejection with a result was accepted")
	}
}

func TestRLSBoundedStopRequiresExactStep37FailureOnlyProjection(t *testing.T) {
	manifest, err := finalv5rls.Load()
	if err != nil {
		t.Fatal(err)
	}
	steps, err := manifest.Trace()
	if err != nil {
		t.Fatal(err)
	}
	stop, err := finalv5rls.ComputeBoundedStop(steps)
	if err != nil {
		t.Fatal(err)
	}
	candidate := steps[stop.Index-1]
	rootHash := digestForRLSTest("bounded-root")
	root := validRootLedgerSnapshot()
	root.Epoch, root.RootObservationCount = int64(stop.SuccessfulQueries), int64(stop.SuccessfulQueries)
	root.ReleaseCardinality, root.DependencyCardinality, root.OutcomeCardinality =
		stop.Before.Release.Cardinality, stop.Before.Dependency.Cardinality, stop.Before.Outcome.Cardinality
	before := RLSControlSnapshot{
		TaskIDHash: rootHash, RootTaskIDHash: rootHash, Product: finalv5rls.BoundedProduct,
		BudgetProfile: finalv5rls.BoundedProfile, MaxQueries: 110, MaxRows: 1000,
		UsedQueries: int64(stop.SuccessfulQueries), UsedRows: 100,
		MaxReleaseFacts: finalv5rls.BoundedMaxReleaseFacts, MaxDependencyFacts: finalv5rls.BoundedMaxDependencyFacts,
		MaxOutcomeFacts: finalv5rls.BoundedMaxOutcomeFacts,
		Business:        BusinessSQLSnapshot{StatsResetUnixMicro: 100, Dealloc: 2, VisibleCalls: 36, CompanionCalls: 36},
		Root:            root, QueryRecords: 36, Settlements: 36, Observations: 36, Receipts: 36,
		Artifacts: 36, AvailableArtifacts: 36, SuccessfulAudits: 144, CanonicalObjects: 36,
	}
	after := before
	after.UsedQueries++
	after.UsedRows += int64(len(candidate.ExpectedRows))
	after.Business.VisibleCalls++
	after.Business.CompanionCalls++
	after.QueryRecords++
	after.Receipts++
	after.FailureAudits += 2
	queryHash := digestForRLSTest("bounded-rejected-query")
	projection := AttackRejectedQueryEvidence{
		Found: true, QueryIDHash: queryHash, Status: "FAILED", ErrorCode: "EXPOSURE_BUDGET_EXHAUSTED",
		ReservationStatus: "RELEASED", FailureAudits: 2, Receipts: 1,
	}
	step := RLSStepEvidence{
		Rejected: true, ObservedErrorCode: "EXPOSURE_BUDGET_EXHAUSTED", ObservedErrorReason: stop.ErrorReason,
		RequestIDHash: digestForRLSTest("bounded-request"), QueryIDHash: queryHash, RootTaskIDHash: rootHash,
		Before: &before, After: &after, OraclePrefix: stop.Candidate, RejectedQuery: &projection,
	}
	sample := Sample{Mode: "bounded"}
	evidence := &RLSVerificationEvidence{Product: finalv5rls.BoundedProduct, BudgetProfile: finalv5rls.BoundedProfile, RootTaskIDHash: rootHash}
	if err := validateRLSBudgetRejection(sample, evidence, step, candidate, stop.Full, stop); err != nil {
		t.Fatalf("valid bounded rejection: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*RLSStepEvidence)
	}{
		{name: "early query usage", mutate: func(value *RLSStepEvidence) { value.Before.UsedQueries-- }},
		{name: "successful audit", mutate: func(value *RLSStepEvidence) { value.After.SuccessfulAudits++ }},
		{name: "missing release audit", mutate: func(value *RLSStepEvidence) {
			value.After.FailureAudits--
			value.RejectedQuery.FailureAudits--
		}},
		{name: "candidate prefix", mutate: func(value *RLSStepEvidence) { value.OraclePrefix.Dependency.Cardinality-- }},
		{name: "missing executed rows", mutate: func(value *RLSStepEvidence) { value.After.UsedRows-- }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := step
			beforeCopy, afterCopy, projectionCopy := before, after, projection
			mutated.Before, mutated.After, mutated.RejectedQuery = &beforeCopy, &afterCopy, &projectionCopy
			test.mutate(&mutated)
			if err := validateRLSBudgetRejection(sample, evidence, mutated, candidate, stop.Full, stop); err == nil {
				t.Fatal("mutated bounded rejection was accepted")
			}
		})
	}
}

func TestRLSRootTransitionAllowsStep2ObservationSetsToDifferFromCumulativeRoot(t *testing.T) {
	before := RLSControlSnapshot{Root: RootLedgerSnapshot{
		ReleaseCardinality: 1, DependencyCardinality: 2, OutcomeCardinality: 2,
		ReleaseSetSHA256: digestForRLSTest("step1-root-release"), DependencySetSHA256: digestForRLSTest("step1-root-dependency"),
		OutcomeSetSHA256: digestForRLSTest("step1-root-outcome"),
	}}
	after := RLSControlSnapshot{Root: RootLedgerSnapshot{
		ReleaseCardinality: 2, DependencyCardinality: 4, OutcomeCardinality: 4,
		ReleaseSetSHA256: digestForRLSTest("step2-root-release"), DependencySetSHA256: digestForRLSTest("step2-root-dependency"),
		OutcomeSetSHA256: digestForRLSTest("step2-root-outcome"),
	}}
	step := RLSStepEvidence{
		Before: &before, After: &after,
		ChargedReleaseFacts: 1, ChargedDependencyFacts: 2, ChargedOutcomeFacts: 2,
		// These are the signed step-2 observation sets, not the cumulative root
		// union. Their composite-verifier validation occurs separately.
		ReleaseSetSHA256:    digestForRLSTest("step2-observation-release"),
		DependencySetSHA256: digestForRLSTest("step2-observation-dependency"),
		OutcomeSetSHA256:    digestForRLSTest("step2-observation-outcome"),
	}
	prefix := finalv5oracle.TraceUnion{
		Release: finalv5oracle.Dimension{Cardinality: 2}, Dependency: finalv5oracle.Dimension{Cardinality: 4},
		Outcome: finalv5oracle.Dimension{Cardinality: 4},
	}
	if step.ReleaseSetSHA256 == after.Root.ReleaseSetSHA256 || step.DependencySetSHA256 == after.Root.DependencySetSHA256 ||
		step.OutcomeSetSHA256 == after.Root.OutcomeSetSHA256 {
		t.Fatal("test fixture did not separate observation and cumulative-root set scopes")
	}
	if err := validateRLSRootExposureTransition(step, prefix); err != nil {
		t.Fatalf("valid step-2 root union transition: %v", err)
	}

	mutated := step
	mutatedAfter := after
	mutatedAfter.Root.DependencySetSHA256 = before.Root.DependencySetSHA256
	mutated.After = &mutatedAfter
	if err := validateRLSRootExposureTransition(mutated, prefix); err == nil {
		t.Fatal("positive dependency charge with an unchanged root digest was accepted")
	}

	mutated = step
	mutated.ChargedOutcomeFacts = 1
	if err := validateRLSRootExposureTransition(mutated, prefix); err == nil {
		t.Fatal("charged outcome count differing from the root cardinality delta was accepted")
	}
}

func TestRLSFreshRootInitializesOnlyCanonicalSignedEmptyDimensionSets(t *testing.T) {
	emptyRelease := digestForRLSTest("canonical-empty-release")
	emptyDependency := digestForRLSTest("canonical-empty-dependency")
	outcome := digestForRLSTest("policy-empty-result-outcome")
	before := RLSControlSnapshot{Root: RootLedgerSnapshot{Epoch: 0}}
	after := RLSControlSnapshot{Root: RootLedgerSnapshot{
		Epoch: 1, RootObservationCount: 1,
		ReleaseSetSHA256: emptyRelease, DependencySetSHA256: emptyDependency, OutcomeSetSHA256: outcome,
		OutcomeCardinality: 2,
	}}
	step := RLSStepEvidence{
		Before: &before, After: &after, ChargedOutcomeFacts: 2,
		ReleaseSetSHA256: emptyRelease, DependencySetSHA256: emptyDependency, OutcomeSetSHA256: outcome,
	}
	prefix := finalv5oracle.TraceUnion{Outcome: finalv5oracle.Dimension{Cardinality: 2}}
	if err := validateRLSRootExposureTransition(step, prefix); err != nil {
		t.Fatalf("valid fresh canonical empty-set initialization: %v", err)
	}

	mutated := step
	mutated.ReleaseSetSHA256 = digestForRLSTest("different-observation-empty-release")
	if err := validateRLSRootExposureTransition(mutated, prefix); err == nil {
		t.Fatal("fresh root accepted an empty release digest not bound to the signed observation")
	}

	nonFreshBefore, nonFreshAfter := before, after
	nonFreshBefore.Root.Epoch, nonFreshBefore.Root.ReleaseSetSHA256 = 1, emptyRelease
	nonFreshAfter.Root.Epoch = 2
	nonFreshAfter.Root.ReleaseSetSHA256 = digestForRLSTest("later-empty-release-churn")
	mutated = step
	mutated.Before, mutated.After = &nonFreshBefore, &nonFreshAfter
	if err := validateRLSRootExposureTransition(mutated, prefix); err == nil {
		t.Fatal("non-fresh zero-charge release digest churn was accepted")
	}
}

func TestRLSPrefixNoveltyUsesMemberSetsRatherThanQueryCount(t *testing.T) {
	dimension := finalv5oracle.Dimension{Cardinality: 6, SetSHA256: digestForRLSTest("prefix-members"), Budget: 4}
	before := finalv5oracle.TraceUnion{Oracle: finalv5oracle.OracleID, Queries: 9,
		Release: dimension, Dependency: dimension, Outcome: dimension}
	after := before
	after.Queries = 10
	if !sameRLSExposureMembers(before, after) {
		t.Fatal("a replay-only query count increment was mistaken for novel membership")
	}
	after.Outcome.SetSHA256 = digestForRLSTest("novel-outcome-member")
	if sameRLSExposureMembers(before, after) {
		t.Fatal("a changed outcome member set was mistaken for semantic replay")
	}
}

func validDirectRLSSample(t *testing.T) Sample {
	t.Helper()
	manifest, err := finalv5rls.Load()
	if err != nil {
		t.Fatal(err)
	}
	steps, err := manifest.Trace()
	if err != nil {
		t.Fatal(err)
	}
	oracleTrace := finalv5rls.OracleTrace(steps)
	oracleResult, _ := finalv5oracle.Evaluate(oracleTrace)
	prefixes, _ := finalv5oracle.EvaluatePrefixes(oracleTrace)
	evidence := validRLSPolicyEvidence(manifest)
	evidence.OracleTrace, evidence.OracleResult, evidence.OraclePrefixes = oracleTrace, oracleResult, prefixes
	evidence.StopReason, evidence.SuccessfulQueries = "TRACE_COMPLETED", 100
	sample := validTestSample()
	sample.ExperimentID, sample.System, sample.Mode = "rls", "postgresql", "rls"
	sample.WorkloadID, sample.Scale, sample.ResultSHA256 = "adaptive-100-v1", "100-queries", finalv5rls.TraceResultSHA256(steps, len(steps))
	sample.RLSVerification = evidence
	for index, wanted := range steps {
		step := RLSStepEvidence{Index: index + 1, StepID: wanted.ID, Family: wanted.Family, Variant: wanted.Variant,
			LogicalSQLSHA256: sha256Hex([]byte(wanted.LogicalSQL(finalv5rls.UnlimitedProduct))), DirectSQLSHA256: sha256Hex([]byte(wanted.DirectSQL)),
			ExpectedResultSHA256: wanted.ExpectedSHA256, ObservedResultSHA256: wanted.ExpectedSHA256,
			RowCount: int64(len(wanted.ExpectedRows)), ColumnCount: 1, ScalarInt64: cloneRLSTestInt(wanted.Scalar), Accepted: true,
			DecisionPreviousStep: wanted.Decision.PreviousStep, DecisionPreviousValue: wanted.Decision.PreviousValue,
			DecisionRule: wanted.Decision.Rule, DecisionThreshold: wanted.Decision.Threshold, OraclePrefix: prefixes[index]}
		evidence.Steps = append(evidence.Steps, step)
		sample.Trace = append(sample.Trace, TraceStep{Index: index + 1, ConcreteSQL: wanted.DirectSQL,
			PriorStateSHA256: digestForRLSTestIndex("state", index), ResultSHA256: wanted.ExpectedSHA256,
			NextSQLSHA256: digestForRLSTestIndex("next", index)})
	}
	return sample
}

func validDirectRLSPolicyDeniedSample(t *testing.T) Sample {
	t.Helper()
	manifest, err := finalv5rls.Load()
	if err != nil {
		t.Fatal(err)
	}
	evidence := validRLSPolicyEvidence(manifest)
	policy, err := manifest.PolicyInvisibleStep()
	if err != nil {
		t.Fatal(err)
	}
	target, err := manifest.PolicyInvisibleRow()
	if err != nil {
		t.Fatal(err)
	}
	authorization := finalv5rls.PolicyAuthorizationControl()
	evidence.OracleTrace = []finalv5oracle.Observation{policy.Oracle, {}}
	evidence.OracleResult, _ = finalv5oracle.Evaluate(evidence.OracleTrace)
	evidence.OraclePrefixes, _ = finalv5oracle.EvaluatePrefixes(evidence.OracleTrace)
	evidence.SuccessfulQueries, evidence.FirstRejectionIndex = 1, 2
	evidence.UnrelatedAuthorizationDenials, evidence.StopReason = 1, "POLICY_FILTER_AND_AUTHORIZATION_REJECTION"
	evidence.Steps = []RLSStepEvidence{
		{Index: 1, StepID: policy.ID, Family: policy.Family, Variant: policy.Variant,
			LogicalSQLSHA256: sha256Hex([]byte(policy.LogicalSQL(finalv5rls.UnlimitedProduct))), DirectSQLSHA256: sha256Hex([]byte(policy.DirectSQL)),
			ExpectedResultSHA256: policy.ExpectedSHA256, ObservedResultSHA256: policy.ExpectedSHA256,
			RowCount: 0, ColumnCount: 1, Accepted: true, OraclePrefix: evidence.OraclePrefixes[0]},
		{Index: 2, StepID: authorization.ID, Family: authorization.Family, Variant: authorization.Variant,
			LogicalSQLSHA256: sha256Hex([]byte(authorization.LogicalSQL(finalv5rls.UnlimitedProduct))),
			DirectSQLSHA256:  sha256Hex([]byte(authorization.DirectSQL)), Rejected: true,
			ObservedErrorCode: "42501", ObservedErrorReason: "UNAPPROVED_COLUMN", OraclePrefix: evidence.OraclePrefixes[1]},
	}
	evidence.NegativeControl = &RLSNegativeEvidence{
		TargetReceiptNo: finalv5rls.PolicyInvisibleReceipt, TargetDepartment: target.Department,
		PolicyDepartment: manifest.PolicyDepartment, TargetPresentOutsidePolicy: true, PolicyFiltered: true,
		ExpectedRowCount: 0, ObservedRowCount: 0, ExpectedResultSHA256: policy.ExpectedSHA256,
		ObservedResultSHA256: policy.ExpectedSHA256, AuthorizationSQLSHA256: sha256Hex([]byte(authorization.DirectSQL)),
		ExpectedAuthorizationErrorCode: "42501", ObservedAuthorizationErrorCode: "42501",
		ObservedAuthorizationErrorReason: "UNAPPROVED_COLUMN", AuthorizationRejectedNoRows: true,
	}
	sample := validTestSample()
	sample.ExperimentID, sample.System, sample.Mode, sample.WorkloadID, sample.Scale = "rls", "postgresql", "rls", "policy-denied-control", "single"
	sample.RowCount, sample.ColumnCount, sample.ResultSHA256 = 0, 0, ""
	sample.Rejected, sample.RejectedNoResult, sample.RejectedNoArtifact, sample.RejectedNoSuccessfulAudit = true, true, true, true
	sample.RLSVerification = evidence
	state := sha256Hex([]byte("TASKGATE-FINAL-V5-RLS-POLICY-FILTER-V1"))
	sample.Trace = []TraceStep{
		{Index: 1, ConcreteSQL: policy.DirectSQL, PriorStateSHA256: state, ResultSHA256: policy.ExpectedSHA256,
			NextSQLSHA256: sha256Hex([]byte(authorization.DirectSQL))},
		{Index: 2, ConcreteSQL: authorization.DirectSQL,
			PriorStateSHA256: sha256Hex([]byte(state + "\x00" + policy.ExpectedSHA256)),
			NextSQLSHA256:    sha256Hex([]byte("TASKGATE-FINAL-V5-RLS-POLICY-FILTER-END-V1")),
			Rejected:         true, NoResult: true, NoAvailableArtifact: true, NoSuccessfulAudit: true},
	}
	return sample
}

func validRLSPolicyEvidence(manifest finalv5rls.Manifest) *RLSVerificationEvidence {
	policies, memberships, grants := finalv5rls.ExpectedPoliciesJSON(), finalv5rls.ExpectedMembershipJSON(), finalv5rls.ExpectedGrantsJSON()
	return &RLSVerificationEvidence{Version: rlsEvidenceVersion, CorpusID: finalv5rls.CorpusID, CorpusSHA256: finalv5rls.CorpusSHA256,
		TraceSHA256: finalv5rls.TraceSHA256, DatasetID: finalv5rls.DatasetID, DatasetSHA256: finalv5rls.DatasetSHA256(manifest), PolicySeed: manifest.Seed,
		PolicySchema: finalv5rls.PolicySchema, PolicyTable: finalv5rls.PolicyTable, PolicyName: finalv5rls.PolicyName,
		RelRowSecurity: true, RelForceRowSecurity: true, SessionUser: "postgres", CurrentRole: finalv5rls.PolicyRole,
		BaselineRole: finalv5rls.PolicyRole, TableOwnerRole: "taskgate_snapshot_owner",
		PoliciesJSON: policies, PoliciesSHA256: sha256Hex(policies), MembershipsJSON: memberships, MembershipsSHA256: sha256Hex(memberships),
		GrantsJSON: grants, GrantsSHA256: sha256Hex(grants), OracleComputedBefore: true}
}

func cloneRLSSample(t *testing.T, sample Sample) Sample {
	t.Helper()
	encoded, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	var clone Sample
	if err := json.Unmarshal(encoded, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func cloneRLSTestInt(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
func digestForRLSTest(value string) string { return sha256Hex([]byte("RLS-TEST\x00" + value)) }
func digestForRLSTestIndex(domain string, index int) string {
	return digestForRLSTest(fmt.Sprintf("%s-%d", domain, index))
}
