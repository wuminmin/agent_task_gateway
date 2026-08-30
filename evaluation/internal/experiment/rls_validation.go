package experiment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/evaluation/finalv5rls"
)

const (
	rlsEvidenceVersion   = "taskgate-final-v5-rls-evidence-v1"
	rlsEvidenceVersionV2 = "taskgate-final-v5-rls-evidence-v2"
)

// ValidateRLSEvidence is the adapter-side fail-closed gate. A real execution
// whose evidence violates the preregistered protocol is retained as fail.
func ValidateRLSEvidence(sample Sample) error {
	if sample.ExperimentID != "rls" || sample.Status != "pass" {
		return errors.New("RLS evidence validation requires a passing RLS sample")
	}
	expected := finalv5rls.TraceLength
	if sample.WorkloadID == "policy-denied-control" {
		expected = 2
	}
	if err := validateRLSVerificationStrict(sample, expected); err != nil {
		return err
	}
	failed := false
	validateTrace(sample, func(string) { failed = true })
	if failed {
		return errors.New("RLS trace transition evidence is invalid")
	}
	return nil
}

func validateRLSVerificationStrict(sample Sample, expectedSteps int) error {
	evidence := sample.RLSVerification
	manifest, err := finalv5rls.Load()
	if err != nil {
		return err
	}
	if evidence == nil || (evidence.Version != rlsEvidenceVersion && evidence.Version != rlsEvidenceVersionV2) || evidence.CorpusID != finalv5rls.CorpusID ||
		evidence.CorpusSHA256 != finalv5rls.CorpusSHA256 || evidence.TraceSHA256 != finalv5rls.TraceSHA256 ||
		evidence.DatasetID != finalv5rls.DatasetID || evidence.DatasetSHA256 != finalv5rls.DatasetSHA256(manifest) ||
		evidence.PolicySeed != manifest.Seed || !evidence.OracleComputedBefore {
		return errors.New("RLS corpus/dataset/seed/oracle binding is absent or changed")
	}
	for _, step := range evidence.Steps {
		if err := validateStepClientMS(evidence.Version == rlsEvidenceVersionV2, "RLS", step.ClientMS); err != nil {
			return err
		}
	}
	if err := validateRLSRolePolicy(evidence); err != nil {
		return err
	}
	if err := validateRLSProductBinding(sample, evidence); err != nil {
		return err
	}
	if sample.WorkloadID == "policy-denied-control" {
		if expectedSteps != 2 {
			return errors.New("RLS negative control step count differs from preregistration")
		}
		return validateRLSPolicyFiltered(sample, evidence, manifest)
	}
	if sample.WorkloadID != "adaptive-100-v1" || sample.Scale != "100-queries" || expectedSteps != finalv5rls.TraceLength {
		return errors.New("RLS sample is outside the frozen primary matrix")
	}
	steps, err := manifest.Trace()
	if err != nil {
		return err
	}
	oracleTrace := finalv5rls.OracleTrace(steps)
	oracleResult, err := finalv5oracle.Evaluate(oracleTrace)
	if err != nil {
		return err
	}
	prefixes, err := finalv5oracle.EvaluatePrefixes(oracleTrace)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(evidence.OracleTrace, oracleTrace) || evidence.OracleResult != oracleResult ||
		!reflect.DeepEqual(evidence.OraclePrefixes, prefixes) {
		return errors.New("RLS independent member trace/full union/prefixes differ from the source-controlled oracle")
	}
	return validateRLSPrimaryTrace(sample, evidence, steps, prefixes, oracleResult)
}

func validateRLSRolePolicy(evidence *RLSVerificationEvidence) error {
	if evidence.PolicySchema != finalv5rls.PolicySchema || evidence.PolicyTable != finalv5rls.PolicyTable ||
		evidence.PolicyName != finalv5rls.PolicyName || !evidence.RelRowSecurity || !evidence.RelForceRowSecurity ||
		evidence.BaselineRole != finalv5rls.PolicyRole || evidence.CurrentRole != finalv5rls.PolicyRole ||
		strings.TrimSpace(evidence.SessionUser) == "" || evidence.SessionUser == evidence.BaselineRole ||
		evidence.TableOwnerRole != "taskgate_snapshot_owner" || evidence.BaselineRoleIsOwner ||
		evidence.BaselineRoleCanLogin || evidence.BaselineRoleSuperuser || evidence.BaselineRoleInherit ||
		evidence.BaselineRoleCreateDB || evidence.BaselineRoleCreateRole || evidence.BaselineRoleReplication || evidence.BaselineRoleBypassRLS {
		return errors.New("PostgreSQL FORCE RLS effective-role/non-owner/role-attribute evidence is absent or unsafe")
	}
	wanted := []struct {
		name       string
		got, exact json.RawMessage
		digest     string
	}{
		{"pg_policies", evidence.PoliciesJSON, finalv5rls.ExpectedPoliciesJSON(), evidence.PoliciesSHA256},
		{"role memberships", evidence.MembershipsJSON, finalv5rls.ExpectedMembershipJSON(), evidence.MembershipsSHA256},
		{"role grants", evidence.GrantsJSON, finalv5rls.ExpectedGrantsJSON(), evidence.GrantsSHA256},
	}
	for _, item := range wanted {
		if !json.Valid(item.got) || !bytes.Equal(item.got, item.exact) || sha256Hex(item.got) != item.digest {
			return fmt.Errorf("canonical %s bytes/hash differ from the frozen RLS fixture", item.name)
		}
	}
	return nil
}

func validateRLSProductBinding(sample Sample, evidence *RLSVerificationEvidence) error {
	switch sample.Mode {
	case "rls":
		if sample.System != "postgresql" || evidence.Product != "" || evidence.BudgetProfile != "" || evidence.RootTaskIDHash != "" || evidence.FinalRoot != nil {
			return errors.New("PostgreSQL RLS arm carries TaskGate product/root evidence")
		}
	case "unlimited":
		if sample.System != "taskgate" || evidence.Product != finalv5rls.UnlimitedProduct || evidence.BudgetProfile != finalv5rls.UnlimitedProfile ||
			!validSHA256(evidence.RootTaskIDHash) || sample.RootTaskIDHash != evidence.RootTaskIDHash {
			return errors.New("unlimited TaskGate RLS control is not bound to its frozen product/profile/root")
		}
	case "bounded":
		if sample.System != "taskgate" || evidence.Product != finalv5rls.BoundedProduct || evidence.BudgetProfile != finalv5rls.BoundedProfile ||
			!validSHA256(evidence.RootTaskIDHash) || sample.RootTaskIDHash != evidence.RootTaskIDHash {
			return errors.New("bounded TaskGate RLS arm is not bound to its frozen product/profile/root")
		}
	default:
		return errors.New("unregistered RLS mode")
	}
	return nil
}

func validateRLSPrimaryTrace(sample Sample, evidence *RLSVerificationEvidence, expected []finalv5rls.Step,
	prefixes []finalv5oracle.TraceUnion, full finalv5oracle.TraceUnion) error {
	boundedStop, err := finalv5rls.ComputeBoundedStop(expected)
	if err != nil {
		return err
	}
	wantAccepted, wantSteps, wantRejection, wantStop := 100, 100, 0, "TRACE_COMPLETED"
	if sample.Mode == "bounded" {
		wantAccepted, wantSteps, wantRejection, wantStop = boundedStop.SuccessfulQueries, boundedStop.Index,
			boundedStop.Index, "EXPOSURE_BUDGET_EXHAUSTED"
	}
	if len(evidence.Steps) != wantSteps || len(sample.Trace) != wantSteps || evidence.SuccessfulQueries != wantAccepted ||
		evidence.FirstRejectionIndex != wantRejection || evidence.StopReason != wantStop ||
		evidence.UnrelatedAuthorizationDenials != 0 || evidence.ResultsAfterBudget != 0 {
		return errors.New("RLS completion/stop counters differ from the deterministic protocol")
	}
	if (sample.Mode == "bounded") != sample.Rejected || sample.Rejected != (wantRejection != 0) {
		return errors.New("RLS sample rejection marker differs from the deterministic stop")
	}
	accepted := 0
	for index := range evidence.Steps {
		step, wanted, trace := evidence.Steps[index], expected[index], sample.Trace[index]
		logicalSQL := wanted.LogicalSQL(evidence.Product)
		if sample.Mode == "rls" {
			logicalSQL = wanted.LogicalSQL(finalv5rls.UnlimitedProduct)
		}
		concreteSQL := wanted.DirectSQL
		if sample.System == "taskgate" {
			concreteSQL = wanted.LogicalSQL(evidence.Product)
		}
		if step.Index != index+1 || step.StepID != wanted.ID || step.Family != wanted.Family || step.Variant != wanted.Variant ||
			step.DirectSQLSHA256 != sha256Hex([]byte(wanted.DirectSQL)) || step.LogicalSQLSHA256 != sha256Hex([]byte(logicalSQL)) ||
			step.ExpectedResultSHA256 != wanted.ExpectedSHA256 || step.OraclePrefix != prefixes[index] ||
			step.DecisionPreviousStep != wanted.Decision.PreviousStep || step.DecisionPreviousValue != wanted.Decision.PreviousValue ||
			step.DecisionRule != wanted.Decision.Rule || step.DecisionThreshold != wanted.Decision.Threshold ||
			trace.Index != index+1 || trace.ConcreteSQL != concreteSQL {
			return fmt.Errorf("RLS step %d identity/SQL/decision/oracle binding differs from the frozen trace", index+1)
		}
		if sample.System == "taskgate" && index > 0 {
			previous := evidence.Steps[index-1]
			if previous.After == nil || step.Before == nil || *previous.After != *step.Before {
				return fmt.Errorf("RLS step %d does not continue from the exact prior Control/Business/root snapshot", index+1)
			}
		}
		if step.Rejected {
			if sample.Mode != "bounded" || index+1 != boundedStop.Index {
				return errors.New("unexpected RLS rejection")
			}
			if rejectionErr := validateRLSBudgetRejection(sample, evidence, step, wanted, full, boundedStop); rejectionErr != nil {
				return rejectionErr
			}
			continue
		}
		if err := validateRLSAcceptedStep(sample, evidence, step, wanted, prefixes[index], index); err != nil {
			return fmt.Errorf("RLS step %d: %w", index+1, err)
		}
		accepted++
	}
	if accepted != wantAccepted {
		return errors.New("RLS successful prefix count differs from preregistration")
	}
	wantTraceResult := finalv5rls.TraceResultSHA256(expected, wantAccepted)
	if !sample.Rejected && sample.ResultSHA256 != wantTraceResult {
		return errors.New("completed RLS trace-result digest differs from the exact corpus")
	}
	if sample.System == "taskgate" {
		last := evidence.Steps[wantAccepted-1]
		if last.After == nil || evidence.FinalRoot == nil || *evidence.FinalRoot != last.After.Root ||
			sample.ReleaseSetSHA256 != last.After.Root.ReleaseSetSHA256 || sample.DependencySetSHA256 != last.After.Root.DependencySetSHA256 ||
			sample.OutcomeSetSHA256 != last.After.Root.OutcomeSetSHA256 || sample.ActualReleaseFacts != last.After.Root.ReleaseCardinality ||
			sample.ActualDependencyFacts != last.After.Root.DependencyCardinality || sample.ActualOutcomeFacts != last.After.Root.OutcomeCardinality {
			return errors.New("RLS top-level TaskGate root evidence differs from the successful prefix")
		}
	}
	return nil
}

func validateRLSAcceptedStep(sample Sample, evidence *RLSVerificationEvidence, step RLSStepEvidence,
	wanted finalv5rls.Step, prefix finalv5oracle.TraceUnion, index int) error {
	if !step.Accepted || step.Rejected || step.ObservedErrorCode != "" || step.ObservedErrorReason != "" ||
		step.ObservedResultSHA256 != wanted.ExpectedSHA256 || step.RowCount != int64(len(wanted.ExpectedRows)) || step.ColumnCount != 1 {
		return errors.New("accepted result differs from the exact frozen result")
	}
	if (wanted.Scalar == nil) != (step.ScalarInt64 == nil) || (wanted.Scalar != nil && *wanted.Scalar != *step.ScalarInt64) {
		return errors.New("accepted scalar differs from the adaptive-policy input")
	}
	if sample.System == "postgresql" {
		if step.VerifiedResultSHA256 != "" || step.RequestIDHash != "" || step.QueryIDHash != "" || step.ResultIDHash != "" ||
			step.PlanSHA256 != "" || step.ObservationSHA256 != "" || step.ReleaseSetSHA256 != "" || step.DependencySetSHA256 != "" ||
			step.OutcomeSetSHA256 != "" || step.Before != nil || step.After != nil || step.Verification != nil || step.RejectedQuery != nil ||
			step.ActualReleaseFacts != 0 || step.ActualDependencyFacts != 0 || step.ActualOutcomeFacts != 0 ||
			step.ChargedReleaseFacts != 0 || step.ChargedDependencyFacts != 0 || step.ChargedOutcomeFacts != 0 ||
			step.ArtifactSHA256 != "" || step.ObjectSHA256 != "" || step.ParquetBytes != 0 || step.EncryptedObjectBytes != 0 {
			return errors.New("PostgreSQL RLS step carries TaskGate-only evidence")
		}
		return nil
	}
	for _, digest := range []string{step.VerifiedResultSHA256, step.RequestIDHash, step.QueryIDHash, step.ResultIDHash, step.PlanSHA256,
		step.ObservationSHA256, step.ReleaseSetSHA256, step.DependencySetSHA256, step.OutcomeSetSHA256, step.RootTaskIDHash,
		step.ArtifactSHA256, step.ObjectSHA256, step.ReceiptSHA256, step.ArtifactIntentSHA256, step.AvailabilitySHA256} {
		if !validSHA256(digest) {
			return errors.New("TaskGate RLS step lacks an authenticated digest")
		}
	}
	if step.VerifiedResultSHA256 != finalv5rls.VerifiedResultSHA256(wanted) {
		return errors.New("TaskGate verified typed result differs from the frozen SQL-type oracle")
	}
	if step.RootTaskIDHash != evidence.RootTaskIDHash || step.Before == nil || step.After == nil || step.Verification == nil || step.RejectedQuery != nil ||
		!currentReceiptVersion(step.ReceiptVersion) || step.ParquetBytes <= 0 || step.EncryptedObjectBytes <= 0 || step.IdempotentReplay {
		return errors.New("TaskGate RLS step lacks its root/current-receipt/artifact evidence")
	}
	if err := validateRLSControlSnapshot(*step.Before, evidence, fullBudgetForMode(sample.Mode)); err != nil {
		return err
	}
	if err := validateRLSControlSnapshot(*step.After, evidence, fullBudgetForMode(sample.Mode)); err != nil {
		return err
	}
	if step.After.UsedQueries != step.Before.UsedQueries+1 || step.After.ReservedQueries != 0 || step.Before.ReservedQueries != 0 {
		return errors.New("production TaskGate root transition differs from the independent prefix cardinality")
	}
	wantSemanticReplay := index > 0 && sameRLSExposureMembers(prefix, evidence.OraclePrefixes[index-1])
	if step.SemanticReplay != wantSemanticReplay {
		return errors.New("TaskGate semantic-replay marker differs from independent prefix novelty")
	}
	if err := validateRLSRootExposureTransition(step, prefix); err != nil {
		return err
	}
	visibleDelta, companionDelta := int64(1), int64(1)
	if step.SemanticReplay {
		visibleDelta, companionDelta = 0, 0
		if step.Before.Root != step.After.Root || step.ChargedReleaseFacts != 0 ||
			step.ChargedDependencyFacts != 0 || step.ChargedOutcomeFacts != 0 {
			return errors.New("RLS semantic replay changed the cumulative root or charged exposure")
		}
	} else if step.After.Root.Epoch != step.Before.Root.Epoch+1 ||
		step.After.Root.RootObservationCount != step.Before.Root.RootObservationCount+1 {
		return errors.New("novel RLS query did not append exactly one root observation")
	}
	if index == 0 {
		if err := validateFreshRootLedgerSnapshot(step.Before.Root); err != nil {
			return err
		}
	}
	verified := Sample{CampaignID: sample.CampaignID, DeploymentID: sample.DeploymentID, RootTaskIDHash: step.RootTaskIDHash,
		RowCount: step.RowCount, ColumnCount: step.ColumnCount, ResultSHA256: step.VerifiedResultSHA256,
		ReleaseSetSHA256: step.ReleaseSetSHA256, DependencySetSHA256: step.DependencySetSHA256, OutcomeSetSHA256: step.OutcomeSetSHA256,
		ActualReleaseFacts: step.ActualReleaseFacts, ChargedReleaseFacts: step.ChargedReleaseFacts,
		ActualDependencyFacts: step.ActualDependencyFacts, ChargedDependencyFacts: step.ChargedDependencyFacts,
		ActualOutcomeFacts: step.ActualOutcomeFacts, ChargedOutcomeFacts: step.ChargedOutcomeFacts,
		PredicateAtomCount: step.PredicateAtomCount, CompositeCount: step.CompositeCount,
		RootEpochAfter: step.After.Root.Epoch, ArtifactSHA256: step.ArtifactSHA256, ObjectSHA256: step.ObjectSHA256,
		ParquetBytes: step.ParquetBytes, EncryptedObjectBytes: step.EncryptedObjectBytes, ReceiptVersion: step.ReceiptVersion,
		ReceiptSHA256: step.ReceiptSHA256, ArtifactIntentSHA256: step.ArtifactIntentSHA256,
		AvailabilityAuditSHA256: step.AvailabilitySHA256, BaselineVerification: step.Verification}
	if err := validateBusinessSQLTransition(step.Before.Business, step.After.Business, visibleDelta, companionDelta); err != nil {
		return err
	}
	if err := validateRLSReleasedSnapshotDelta(*step.Before, *step.After, step, wantSemanticReplay); err != nil {
		return err
	}
	return validateBaselineVerification(verified)
}

func sameRLSExposureMembers(left, right finalv5oracle.TraceUnion) bool {
	return left.Release == right.Release && left.Dependency == right.Dependency && left.Outcome == right.Outcome
}

// validateRLSRootExposureTransition keeps the two authenticated set scopes
// separate. The step set digests bind the single observation in its signed
// receipt/verifier manifest, while Before/After bind the cumulative root
// union. Those digests coincide only for a fresh root's first observation.
func validateRLSRootExposureTransition(step RLSStepEvidence, prefix finalv5oracle.TraceUnion) error {
	if step.Before == nil || step.After == nil {
		return errors.New("TaskGate RLS root transition snapshots are absent")
	}
	if step.After.Root.ReleaseCardinality != prefix.Release.Cardinality ||
		step.After.Root.DependencyCardinality != prefix.Dependency.Cardinality ||
		step.After.Root.OutcomeCardinality != prefix.Outcome.Cardinality {
		return errors.New("production TaskGate root transition differs from the independent prefix cardinality")
	}
	dimensions := []struct {
		name                                string
		before, after, charged              int64
		beforeSet, afterSet, observationSet string
	}{
		{"release", step.Before.Root.ReleaseCardinality, step.After.Root.ReleaseCardinality, step.ChargedReleaseFacts,
			step.Before.Root.ReleaseSetSHA256, step.After.Root.ReleaseSetSHA256, step.ReleaseSetSHA256},
		{"dependency", step.Before.Root.DependencyCardinality, step.After.Root.DependencyCardinality, step.ChargedDependencyFacts,
			step.Before.Root.DependencySetSHA256, step.After.Root.DependencySetSHA256, step.DependencySetSHA256},
		{"outcome", step.Before.Root.OutcomeCardinality, step.After.Root.OutcomeCardinality, step.ChargedOutcomeFacts,
			step.Before.Root.OutcomeSetSHA256, step.After.Root.OutcomeSetSHA256, step.OutcomeSetSHA256},
	}
	for _, dimension := range dimensions {
		delta := dimension.after - dimension.before
		if delta < 0 || dimension.charged != delta {
			return fmt.Errorf("production TaskGate %s root digest transition differs from its charged union delta", dimension.name)
		}
		freshEmptyInitialization := step.Before.Root.Epoch == 0 && dimension.before == 0 && dimension.after == 0 && dimension.charged == 0
		if freshEmptyInitialization {
			if dimension.beforeSet != "" || !validSHA256(dimension.afterSet) || dimension.afterSet != dimension.observationSet {
				return fmt.Errorf("production TaskGate %s root did not initialize its canonical signed empty set", dimension.name)
			}
			continue
		}
		if (dimension.charged == 0) != (dimension.beforeSet == dimension.afterSet) {
			return fmt.Errorf("production TaskGate %s root digest transition differs from its charged union delta", dimension.name)
		}
	}
	return nil
}

func validateRLSReleasedSnapshotDelta(before, after RLSControlSnapshot, step RLSStepEvidence, replay bool) error {
	wantSuccessfulAudits := int64(4)
	if replay {
		wantSuccessfulAudits = 3
	}
	if after.UsedRows-before.UsedRows != step.RowCount ||
		after.QueryRecords-before.QueryRecords != 1 || after.Settlements-before.Settlements != 1 ||
		after.Observations-before.Observations != 1 || after.Receipts-before.Receipts != 1 ||
		after.Artifacts-before.Artifacts != 1 || after.AvailableArtifacts-before.AvailableArtifacts != 1 ||
		after.SuccessfulAudits-before.SuccessfulAudits != wantSuccessfulAudits ||
		after.FailureAudits != before.FailureAudits || after.CanonicalObjects-before.CanonicalObjects != 1 {
		return errors.New("released RLS Control/Business/object transition differs from its exact replay class")
	}
	return nil
}

type rlsBudgetVector struct{ release, dependency, outcome int64 }

func fullBudgetForMode(mode string) rlsBudgetVector {
	if mode == "bounded" {
		return rlsBudgetVector{finalv5rls.BoundedMaxReleaseFacts, finalv5rls.BoundedMaxDependencyFacts,
			finalv5rls.BoundedMaxOutcomeFacts}
	}
	return rlsBudgetVector{1000, 1000, 1000}
}

func validateRLSControlSnapshot(snapshot RLSControlSnapshot, evidence *RLSVerificationEvidence, budget rlsBudgetVector) error {
	if !validSHA256(snapshot.TaskIDHash) || snapshot.TaskIDHash != evidence.RootTaskIDHash || snapshot.RootTaskIDHash != evidence.RootTaskIDHash ||
		snapshot.Product != evidence.Product || snapshot.BudgetProfile != evidence.BudgetProfile || snapshot.MaxQueries != 110 || snapshot.MaxRows != 1000 ||
		snapshot.MaxReleaseFacts != budget.release || snapshot.MaxDependencyFacts != budget.dependency || snapshot.MaxOutcomeFacts != budget.outcome ||
		snapshot.UsedQueries < 0 || snapshot.UsedQueries > snapshot.MaxQueries || snapshot.UsedRows < 0 || snapshot.UsedRows > snapshot.MaxRows ||
		snapshot.ReservedQueries != 0 || snapshot.ReservedRows != 0 || snapshot.QueryRecords < 0 || snapshot.Settlements < 0 ||
		snapshot.Observations < 0 || snapshot.Receipts < 0 || snapshot.Artifacts < 0 || snapshot.AvailableArtifacts < 0 ||
		snapshot.SuccessfulAudits < 0 || snapshot.FailureAudits < 0 || snapshot.CanonicalObjects < 0 {
		return errors.New("RLS Control/resource/profile snapshot is incomplete or outside its frozen budget")
	}
	if err := validateBusinessSQLSnapshot(snapshot.Business); err != nil {
		return err
	}
	if snapshot.Root.Epoch == 0 {
		return validateFreshRootLedgerSnapshot(snapshot.Root)
	}
	return validateRootLedgerSnapshot(snapshot.Root)
}

func validateRLSBudgetRejection(sample Sample, evidence *RLSVerificationEvidence, step RLSStepEvidence,
	wanted finalv5rls.Step, full finalv5oracle.TraceUnion, stop finalv5rls.BoundedStop) error {
	if step.Accepted || !step.Rejected || step.ObservedErrorCode != "EXPOSURE_BUDGET_EXHAUSTED" ||
		step.ObservedErrorReason != stop.ErrorReason || step.Before == nil || step.After == nil || step.RejectedQuery == nil ||
		step.ObservedResultSHA256 != "" || step.VerifiedResultSHA256 != "" || step.RowCount != 0 || step.ColumnCount != 0 || step.ScalarInt64 != nil ||
		!validSHA256(step.RequestIDHash) || !validSHA256(step.QueryIDHash) || step.QueryIDHash != step.RejectedQuery.QueryIDHash ||
		step.RootTaskIDHash != evidence.RootTaskIDHash || step.ResultIDHash != "" || step.PlanSHA256 != "" || step.ObservationSHA256 != "" ||
		step.ReleaseSetSHA256 != "" || step.DependencySetSHA256 != "" || step.OutcomeSetSHA256 != "" || step.Verification != nil ||
		step.ArtifactSHA256 != "" || step.ObjectSHA256 != "" || step.ParquetBytes != 0 || step.EncryptedObjectBytes != 0 {
		return errors.New("bounded RLS terminal step is not an exposure-only fail-closed rejection")
	}
	if err := validateRLSControlSnapshot(*step.Before, evidence, fullBudgetForMode(sample.Mode)); err != nil {
		return err
	}
	if err := validateRLSControlSnapshot(*step.After, evidence, fullBudgetForMode(sample.Mode)); err != nil {
		return err
	}
	releaseCrossed := stop.Candidate.Release.Cardinality > full.Release.Budget
	dependencyCrossed := stop.Candidate.Dependency.Cardinality > full.Dependency.Budget
	outcomeCrossed := stop.Candidate.Outcome.Cardinality > full.Outcome.Budget
	if full != stop.Full || step.OraclePrefix != stop.Candidate || stop.Dimension != "dependency" ||
		releaseCrossed || !dependencyCrossed || outcomeCrossed ||
		stop.Before.Release.Cardinality > full.Release.Budget || stop.Before.Dependency.Cardinality > full.Dependency.Budget ||
		stop.Before.Outcome.Cardinality > full.Outcome.Budget ||
		step.Before.Root != step.After.Root || step.Before.CanonicalObjects != step.After.CanonicalObjects ||
		step.Before.Artifacts != step.After.Artifacts || step.Before.AvailableArtifacts != step.After.AvailableArtifacts ||
		step.Before.SuccessfulAudits != step.After.SuccessfulAudits ||
		step.Before.Root.ReleaseCardinality != stop.Before.Release.Cardinality ||
		step.Before.Root.DependencyCardinality != stop.Before.Dependency.Cardinality ||
		step.Before.Root.OutcomeCardinality != stop.Before.Outcome.Cardinality ||
		full.Dependency.Budget != step.Before.MaxDependencyFacts {
		return errors.New("bounded RLS rejection is not exactly the recomputed first dependency crossing")
	}
	projection := step.RejectedQuery
	if !projection.Found || !validSHA256(projection.QueryIDHash) || projection.Status != "FAILED" ||
		projection.ErrorCode != "EXPOSURE_BUDGET_EXHAUSTED" || projection.ResultSHA256 != "" || projection.ReservationStatus != "RELEASED" ||
		projection.EncryptedResults != 0 || projection.EncryptedChunks != 0 || projection.Materializations != 0 || projection.QueryObservations != 0 ||
		projection.RootObservations != 0 || projection.Artifacts != 0 || projection.AvailableArtifacts != 0 || projection.AvailabilityAudits != 0 ||
		projection.SuccessfulAudits != 0 || projection.FailureAudits != 2 || projection.Receipts != 1 {
		return errors.New("bounded RLS rejection lacks the exact failure-only Control projection")
	}
	candidateRows := int64(len(wanted.ExpectedRows))
	if wanted.Index != stop.Index || candidateRows <= 0 ||
		step.Before.UsedQueries != int64(stop.SuccessfulQueries) || step.After.UsedQueries != int64(stop.Index) ||
		step.After.UsedQueries != step.Before.UsedQueries+1 || step.Before.UsedQueries >= step.Before.MaxQueries ||
		step.After.UsedQueries > step.After.MaxQueries || step.After.UsedRows != step.Before.UsedRows+candidateRows ||
		step.After.UsedRows > step.After.MaxRows || step.Before.UsedRows+candidateRows > step.Before.MaxRows ||
		step.After.QueryRecords-step.Before.QueryRecords != 1 || step.After.Receipts-step.Before.Receipts != 1 ||
		step.After.FailureAudits-step.Before.FailureAudits != 2 || step.After.Settlements != step.Before.Settlements ||
		step.After.Observations != step.Before.Observations || step.After.Artifacts != step.Before.Artifacts {
		return errors.New("bounded RLS rejection was not independent of ordinary query/row budgets")
	}
	return validateBusinessSQLTransition(step.Before.Business, step.After.Business, 1, 1)
}

func validateRLSPolicyFiltered(sample Sample, evidence *RLSVerificationEvidence, manifest finalv5rls.Manifest) error {
	wanted, err := manifest.PolicyInvisibleStep()
	if err != nil {
		return err
	}
	target, err := manifest.PolicyInvisibleRow()
	if err != nil {
		return err
	}
	authorization := finalv5rls.PolicyAuthorizationControl()
	oracleTrace := []finalv5oracle.Observation{wanted.Oracle, {}}
	oracleResult, err := finalv5oracle.Evaluate(oracleTrace)
	if err != nil {
		return err
	}
	oraclePrefixes, err := finalv5oracle.EvaluatePrefixes(oracleTrace)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(evidence.OracleTrace, oracleTrace) || evidence.OracleResult != oracleResult ||
		!reflect.DeepEqual(evidence.OraclePrefixes, oraclePrefixes) || len(evidence.Steps) != 2 || len(sample.Trace) != 2 ||
		evidence.SuccessfulQueries != 1 || evidence.FirstRejectionIndex != 2 || evidence.UnrelatedAuthorizationDenials != 1 ||
		evidence.ResultsAfterBudget != 0 || evidence.StopReason != "POLICY_FILTER_AND_AUTHORIZATION_REJECTION" || evidence.NegativeControl == nil ||
		!sample.Rejected || !sample.RejectedNoResult || !sample.RejectedNoArtifact || !sample.RejectedNoSuccessfulAudit ||
		sample.RowCount != 0 || sample.ColumnCount != 0 || sample.ResultSHA256 != "" || sample.ArtifactSHA256 != "" ||
		sample.ObjectSHA256 != "" || sample.AvailabilityAuditSHA256 != "" || sample.BaselineVerification != nil {
		return errors.New("RLS policy-filter/rejection control is incomplete")
	}

	logicalSQL, concreteSQL := wanted.LogicalSQL(finalv5rls.UnlimitedProduct), wanted.DirectSQL
	authorizationLogicalSQL, authorizationSQL := authorization.LogicalSQL(finalv5rls.UnlimitedProduct), authorization.DirectSQL
	expectedAuthorizationCode := "42501"
	if sample.System == "taskgate" {
		logicalSQL = wanted.LogicalSQL(evidence.Product)
		concreteSQL = logicalSQL
		authorizationLogicalSQL = authorization.LogicalSQL(evidence.Product)
		authorizationSQL = authorizationLogicalSQL
		expectedAuthorizationCode = "COLUMN_NOT_APPROVED"
	}
	negative, step, trace := evidence.NegativeControl, evidence.Steps[0], sample.Trace[0]
	if negative.TargetReceiptNo != finalv5rls.PolicyInvisibleReceipt || negative.TargetDepartment != target.Department ||
		negative.PolicyDepartment != manifest.PolicyDepartment || !negative.TargetPresentOutsidePolicy || !negative.PolicyFiltered ||
		negative.ExpectedRowCount != 0 || negative.ObservedRowCount != 0 ||
		negative.ExpectedResultSHA256 != wanted.ExpectedSHA256 || negative.ObservedResultSHA256 != wanted.ExpectedSHA256 ||
		negative.AuthorizationSQLSHA256 != sha256Hex([]byte(authorizationSQL)) ||
		negative.ExpectedAuthorizationErrorCode != expectedAuthorizationCode || negative.ObservedAuthorizationErrorCode != expectedAuthorizationCode ||
		negative.ObservedAuthorizationErrorReason != "UNAPPROVED_COLUMN" || !negative.AuthorizationRejectedNoRows ||
		step.Index != 1 || step.StepID != wanted.ID || step.Family != wanted.Family || step.Variant != wanted.Variant ||
		step.DirectSQLSHA256 != sha256Hex([]byte(wanted.DirectSQL)) || step.LogicalSQLSHA256 != sha256Hex([]byte(logicalSQL)) ||
		step.ExpectedResultSHA256 != wanted.ExpectedSHA256 || step.OraclePrefix != oraclePrefixes[0] ||
		trace.ConcreteSQL != concreteSQL || trace.Rejected || trace.NoResult || trace.NoAvailableArtifact || trace.NoSuccessfulAudit ||
		trace.ResultSHA256 != wanted.ExpectedSHA256 || trace.PriorStateSHA256 != sha256Hex([]byte("TASKGATE-FINAL-V5-RLS-POLICY-FILTER-V1")) ||
		trace.NextSQLSHA256 != sha256Hex([]byte(authorizationSQL)) {
		return errors.New("RLS FORCE-policy row-invisibility proof differs from preregistration")
	}
	if err := validateRLSAcceptedStep(sample, evidence, step, wanted, oraclePrefixes[0], 0); err != nil {
		return fmt.Errorf("RLS policy-filtered step: %w", err)
	}
	denied, deniedTrace := evidence.Steps[1], sample.Trace[1]
	wantDeniedPrior := sha256Hex([]byte(trace.PriorStateSHA256 + "\x00" + step.ObservedResultSHA256))
	if denied.Index != 2 || denied.StepID != authorization.ID || denied.Family != authorization.Family || denied.Variant != authorization.Variant ||
		denied.DirectSQLSHA256 != sha256Hex([]byte(authorization.DirectSQL)) || denied.LogicalSQLSHA256 != sha256Hex([]byte(authorizationLogicalSQL)) ||
		denied.Accepted || !denied.Rejected || denied.ObservedErrorCode != expectedAuthorizationCode ||
		denied.ObservedErrorReason != "UNAPPROVED_COLUMN" || denied.ObservedResultSHA256 != "" || denied.VerifiedResultSHA256 != "" ||
		denied.ExpectedResultSHA256 != "" || denied.RowCount != 0 || denied.ColumnCount != 0 || denied.ScalarInt64 != nil ||
		denied.OraclePrefix != oraclePrefixes[1] || deniedTrace.Index != 2 || deniedTrace.ConcreteSQL != authorizationSQL ||
		!deniedTrace.Rejected || !deniedTrace.NoResult || !deniedTrace.NoAvailableArtifact || !deniedTrace.NoSuccessfulAudit ||
		deniedTrace.ResultSHA256 != "" || deniedTrace.PriorStateSHA256 != wantDeniedPrior ||
		deniedTrace.NextSQLSHA256 != sha256Hex([]byte("TASKGATE-FINAL-V5-RLS-POLICY-FILTER-END-V1")) {
		return errors.New("RLS authorization-rejection proof differs from preregistration")
	}
	if sample.System == "postgresql" {
		if denied.RequestIDHash != "" || denied.QueryIDHash != "" || denied.ResultIDHash != "" || denied.PlanSHA256 != "" ||
			denied.ObservationSHA256 != "" || denied.ReleaseSetSHA256 != "" || denied.DependencySetSHA256 != "" || denied.OutcomeSetSHA256 != "" ||
			denied.RootTaskIDHash != "" || denied.Before != nil || denied.After != nil || denied.Verification != nil || denied.RejectedQuery != nil ||
			denied.ArtifactSHA256 != "" || denied.ObjectSHA256 != "" || denied.ParquetBytes != 0 || denied.EncryptedObjectBytes != 0 ||
			negative.Before != nil || negative.After != nil || negative.RejectedQuery != nil {
			return errors.New("direct RLS authorization rejection carries TaskGate-only evidence")
		}
		return nil
	}
	if !validSHA256(denied.RequestIDHash) || denied.RootTaskIDHash != evidence.RootTaskIDHash || denied.QueryIDHash != "" || denied.ResultIDHash != "" || denied.PlanSHA256 != "" ||
		denied.ObservationSHA256 != "" || denied.ReleaseSetSHA256 != "" || denied.DependencySetSHA256 != "" || denied.OutcomeSetSHA256 != "" ||
		denied.Verification != nil || denied.ArtifactSHA256 != "" || denied.ObjectSHA256 != "" || denied.ParquetBytes != 0 || denied.EncryptedObjectBytes != 0 ||
		denied.Before == nil || denied.After == nil || denied.RejectedQuery == nil || *denied.RejectedQuery != (AttackRejectedQueryEvidence{}) ||
		negative.Before == nil || negative.After == nil || negative.RejectedQuery == nil ||
		*negative.Before != *denied.Before || *negative.After != *denied.After || *negative.RejectedQuery != *denied.RejectedQuery ||
		*denied.Before != *denied.After {
		return errors.New("TaskGate authorization rejection changed or omitted Control/Business/root/artifact state")
	}
	if err := validateRLSControlSnapshot(*denied.Before, evidence, fullBudgetForMode(sample.Mode)); err != nil {
		return err
	}
	if step.After == nil || evidence.FinalRoot == nil || *evidence.FinalRoot != denied.After.Root || step.After.Root != denied.Before.Root ||
		sample.ReleaseSetSHA256 != denied.After.Root.ReleaseSetSHA256 || sample.DependencySetSHA256 != denied.After.Root.DependencySetSHA256 ||
		sample.OutcomeSetSHA256 != denied.After.Root.OutcomeSetSHA256 || sample.ActualReleaseFacts != denied.After.Root.ReleaseCardinality ||
		sample.ActualDependencyFacts != denied.After.Root.DependencyCardinality || sample.ActualOutcomeFacts != denied.After.Root.OutcomeCardinality {
		return errors.New("TaskGate policy-filter control top-level root differs from its authenticated empty result")
	}
	return nil
}

// validateStepClientMS is the evidence-v2 timing rule: every step of a v2
// sample records the positive client-observed milliseconds of its query call,
// and a v1 sample (sealed before the field existed) must not carry one.
func validateStepClientMS(v2 bool, experimentName string, clientMS float64) error {
	if v2 {
		if !(clientMS > 0) {
			return fmt.Errorf("evidence-v2 %s step lacks a positive client_ms", experimentName)
		}
		return nil
	}
	if clientMS != 0 {
		return fmt.Errorf("evidence-v1 %s step carries client_ms", experimentName)
	}
	return nil
}
