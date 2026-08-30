package experiment

import (
	"encoding/json"
	"errors"
	"fmt"

	"taskbound.local/agent-data-gateway/evaluation/finalv5attack"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

const (
	attackVerificationVersion   = "taskgate-final-v5-attack-evidence-v1"
	attackVerificationVersionV2 = "taskgate-final-v5-attack-evidence-v2"
)

// validateAttackVerificationStrict deliberately starts from the immutable
// corpus and reconstructs every assertion. Adapter booleans are observations,
// never an oracle. TaskGate releases are additionally reverified against the
// signed V8 receipt, audit inclusion proofs, Control snapshots, and canonical
// object evidence retained for that exact step.
func validateAttackVerificationStrict(sample Sample) error {
	evidence := sample.AttackVerification
	if evidence == nil || (evidence.Version != attackVerificationVersion && evidence.Version != attackVerificationVersionV2) ||
		evidence.CorpusID != finalv5attack.CorpusID || evidence.CorpusSHA256 != finalv5attack.CorpusSHA256 ||
		evidence.DatasetID != finalv5attack.DatasetID {
		return errors.New("raw attack verification evidence is absent or not bound to the frozen corpus")
	}
	for _, step := range evidence.Steps {
		if err := validateStepClientMS(evidence.Version == attackVerificationVersionV2, "attack", step.ClientMS); err != nil {
			return err
		}
	}
	if err := validateAttackModeAndSystem(sample); err != nil {
		return err
	}
	manifest, err := finalv5attack.Load()
	if err != nil {
		return err
	}
	attackCase, found := manifest.Lookup(sample.WorkloadID, sample.Scale)
	if !found || len(evidence.Steps) != len(attackCase.Steps) || len(sample.Trace) != len(attackCase.Steps) {
		return errors.New("unknown or incomplete preregistered attack workload")
	}
	if err := validateAttackCatalogBinding(sample, attackCase); err != nil {
		return err
	}

	primary := make([]string, 0, len(attackCase.Steps))
	var primaryRows int64
	primaryColumns := 0
	traceState := sha256Hex([]byte("TASKGATE-FINAL-V5-ATTACK-TRACE-V1\x00" + finalv5attack.CorpusSHA256))
	for index, expected := range attackCase.Steps {
		step := &evidence.Steps[index]
		if err := validateAttackStepIdentity(sample, attackCase, index, expected, step, traceState); err != nil {
			return fmt.Errorf("attack step %d: %w", index+1, err)
		}
		if step.Accepted {
			if err := validateAttackAcceptedRows(*step); err != nil {
				return fmt.Errorf("attack step %d: %w", index+1, err)
			}
			if sample.System == "taskgate" {
				if err := validateAttackReleasedStep(sample, *step); err != nil {
					return fmt.Errorf("attack step %d: %w", index+1, err)
				}
			} else if err := validateDirectAttackStep(*step); err != nil {
				return fmt.Errorf("attack step %d: %w", index+1, err)
			}
			if expected.Primary {
				primary = append(primary, step.ResultSHA256)
				primaryRows += step.RowCount
				if primaryColumns == 0 {
					primaryColumns = step.ColumnCount
				} else if primaryColumns != step.ColumnCount {
					return errors.New("primary attack result sequence changes column cardinality")
				}
			}
		} else if err := validateAttackRejectedStep(sample, expected, *step); err != nil {
			return fmt.Errorf("attack step %d: %w", index+1, err)
		}
		transition := step.ResultSHA256
		if step.Rejected {
			transition = step.ObservedErrorCode + "\x00" + step.ObservedErrorReason
		}
		traceState = sha256Hex([]byte(traceState + "\x00" + transition))
	}

	primaryDigest, err := finalv5attack.PrimaryResultSHA256(primary)
	if err != nil || evidence.PrimaryResultSHA256 != primaryDigest || sample.ResultSHA256 != primaryDigest ||
		sample.RowCount != primaryRows || sample.ColumnCount != primaryColumns {
		return errors.New("attack primary result sequence does not recompute")
	}
	if sample.PhysicalSQLSHA256 != attackSQLSequenceDigest(attackCase, sample.System == "postgresql") ||
		sample.LogicalSQLSHA256 != attackSQLSequenceDigest(attackCase, false) ||
		sample.QueryPlanSHA256 != sha256Hex([]byte(finalv5attack.CorpusSHA256+"\x00"+sample.WorkloadID+"\x00"+sample.Scale)) {
		return errors.New("attack top-level corpus/SQL sequence binding differs from the frozen source")
	}
	if err := validateAttackCaseSemantics(sample, attackCase); err != nil {
		return err
	}
	if sample.System == "taskgate" {
		return validateAttackTaskgateSummary(sample)
	}
	return validateAttackDirectSummary(sample)
}

type expectedAttackBinding struct {
	product, profile       string
	maxQueries, maxOutcome int64
}

func expectedAttackBindingFor(workloadID string) expectedAttackBinding {
	if workloadID == "E-threshold" {
		return expectedAttackBinding{product: "expense_detail", profile: "detail-manual-v5", maxQueries: 5, maxOutcome: 5}
	}
	return expectedAttackBinding{product: "final_v5_attack_expense_detail", profile: "final-v5-attack-medium-v1", maxQueries: 10, maxOutcome: 10}
}

func validateAttackCatalogBinding(sample Sample, attackCase finalv5attack.Case) error {
	evidence := sample.AttackVerification
	want := expectedAttackBindingFor(sample.WorkloadID)
	if evidence.Product != want.product {
		return errors.New("attack product differs from the frozen workload binding")
	}
	if sample.System == "postgresql" {
		if evidence.BudgetProfile != "" || evidence.RootTaskIDHash != "" {
			return errors.New("direct attack control fabricates a TaskGate profile/root binding")
		}
		return nil
	}
	if evidence.BudgetProfile != want.profile || !validSHA256(evidence.RootTaskIDHash) ||
		sample.RootTaskIDHash != evidence.RootTaskIDHash {
		return errors.New("TaskGate attack lacks its exact product/profile/root binding")
	}
	rootTaskHash := evidence.RootTaskIDHash
	childTaskHash := ""
	for index := range evidence.Steps {
		step := evidence.Steps[index]
		if step.Before == nil || step.After == nil {
			return fmt.Errorf("attack step %d lacks bound Control snapshots", index+1)
		}
		for _, snapshot := range []*AttackControlSnapshot{step.Before, step.After} {
			if snapshot.Product != want.product || snapshot.BudgetProfile != want.profile ||
				snapshot.MaxQueries != want.maxQueries || snapshot.MaxOutcomeFacts != want.maxOutcome ||
				snapshot.RootTaskIDHash != evidence.RootTaskIDHash {
				return fmt.Errorf("attack step %d product/profile/root budget binding differs", index+1)
			}
		}
		if step.Before.TaskIDHash != step.After.TaskIDHash {
			return fmt.Errorf("attack step %d changes task identity", index+1)
		}
		eChildStep := expectedAttackTaskRoute(attackCase.Steps[index]) == finalv5attack.TaskRouteDelegatedChild
		if eChildStep {
			if step.Before.TaskIDHash == rootTaskHash {
				return fmt.Errorf("E child step %d used the root task", index+1)
			}
			if childTaskHash == "" {
				childTaskHash = step.Before.TaskIDHash
			} else if step.Before.TaskIDHash != childTaskHash {
				return errors.New("E prefix and terminal probe did not use the same child task")
			}
		} else if step.Before.TaskIDHash != rootTaskHash {
			return fmt.Errorf("attack step %d unexpectedly left the root task", index+1)
		}
		if step.Accepted && step.RootTaskIDHash != evidence.RootTaskIDHash {
			return fmt.Errorf("attack step %d signed receipt root differs", index+1)
		}
	}
	return validateAttackExactQueryBudget(sample)
}

func expectedAttackTaskRoute(step finalv5attack.Step) string {
	if step.TaskRoute == "" {
		return finalv5attack.TaskRouteRoot
	}
	return step.TaskRoute
}

func validateAttackExactQueryBudget(sample Sample) error {
	steps := sample.AttackVerification.Steps
	for index := range steps {
		step := steps[index]
		before, after := step.Before, step.After
		wantDelta := int64(1)
		if step.IdempotentReplay || step.ObservedErrorCode == "SQL_NOT_LOWERABLE" {
			wantDelta = 0
		}
		if before.ReservedQueries != 0 || after.ReservedQueries != 0 ||
			after.UsedQueries-before.UsedQueries != wantDelta {
			return fmt.Errorf("attack step %d resource query budget transition differs", index+1)
		}
		if index > 0 && steps[index-1].After.TaskIDHash == before.TaskIDHash &&
			steps[index-1].After.UsedQueries != before.UsedQueries {
			return fmt.Errorf("attack step %d query budget snapshots are discontinuous", index+1)
		}
	}
	first, last := steps[0].Before, steps[len(steps)-1].After
	switch sample.WorkloadID {
	case "A-pagination":
		if first.UsedQueries != 0 || last.UsedQueries != 6 {
			return errors.New("pagination attack did not consume exactly six query units")
		}
	case "B-equivalent-sql":
		if first.UsedQueries != 0 || last.UsedQueries != 4 {
			return errors.New("equivalent-SQL attack did not consume exactly four query units")
		}
	case "C-request-id":
		wantBefore, wantAfter := int64(0), int64(1)
		if sample.Mode == "semantic_replay" {
			wantBefore, wantAfter = 1, 2
		} else if sample.Mode == "idempotent_replay" {
			wantBefore, wantAfter = 2, 2
		}
		if first.UsedQueries != wantBefore || last.UsedQueries != wantAfter {
			return errors.New("request-ID attack query usage differs from its mode")
		}
	case "D-split-union":
		if first.UsedQueries != 0 || last.UsedQueries != 3 {
			return errors.New("split/union attack did not consume exactly three query units")
		}
	case "E-threshold":
		if first.UsedQueries != 0 || steps[0].After.UsedQueries != 1 ||
			steps[1].Before.UsedQueries != 0 || steps[len(steps)-2].After.UsedQueries != 4 ||
			steps[len(steps)-1].Before.UsedQueries != 1 || last.UsedQueries != 2 ||
			steps[len(steps)-2].After.UsedQueries >= steps[len(steps)-2].After.MaxQueries ||
			last.UsedQueries >= last.MaxQueries ||
			steps[len(steps)-1].Before.Root.OutcomeCardinality != 5 || last.Root.OutcomeCardinality != 5 {
			return errors.New("E did not isolate B+1 from both child and root resource-query ceilings")
		}
	}
	return nil
}

func validateAttackModeAndSystem(sample Sample) error {
	if sample.ExperimentID != "attack" {
		return errors.New("attack evidence attached to a different experiment")
	}
	if sample.WorkloadID == "C-request-id" {
		if sample.System != "taskgate" || (sample.Mode != "novel" && sample.Mode != "semantic_replay" && sample.Mode != "idempotent_replay") {
			return errors.New("request-ID control has an unregistered system/mode")
		}
		return nil
	}
	if sample.Mode == "direct" && sample.System == "postgresql" {
		return nil
	}
	if sample.Mode == "novel" && sample.System == "taskgate" {
		return nil
	}
	return errors.New("attack cell has an unregistered system/mode")
}

func validateAttackStepIdentity(sample Sample, attackCase finalv5attack.Case, index int, expected finalv5attack.Step,
	step *AttackStepEvidence, priorState string) error {
	if step.Index != index+1 || step.VariantID != expected.ID || step.Classification != expected.Classification ||
		step.Role != expected.Role || step.LogicalSQLSHA256 != sha256Hex([]byte(expected.LogicalSQL)) ||
		step.DirectSQLSHA256 != sha256Hex([]byte(expected.DirectSQL)) || step.Accepted == step.Rejected {
		return errors.New("identity/classification differs from the frozen corpus")
	}
	expectRejected := sample.System == "taskgate" && expected.Classification == "expected_rejection"
	if step.Rejected != expectRejected {
		return errors.New("accepted/rejected outcome differs from the registered system arm")
	}
	trace := sample.Trace[index]
	sqlText := expected.LogicalSQL
	if sample.System == "postgresql" {
		sqlText = expected.DirectSQL
	}
	nextSQL := "TASKGATE-FINAL-V5-ATTACK-END-V1"
	if index+1 < len(attackCase.Steps) {
		nextSQL = attackCase.Steps[index+1].LogicalSQL
		if sample.System == "postgresql" {
			nextSQL = attackCase.Steps[index+1].DirectSQL
		}
	}
	if trace.Index != index+1 || trace.ConcreteSQL != sqlText || trace.PriorStateSHA256 != priorState ||
		trace.ResultSHA256 != step.ResultSHA256 || trace.NextSQLSHA256 != sha256Hex([]byte(nextSQL)) ||
		trace.PlanSHA256 != step.PlanSHA256 || trace.ObservationSHA256 != step.ObservationSHA256 ||
		trace.ReleaseSetSHA256 != step.ReleaseSetSHA256 || trace.DependencySetSHA256 != step.DependencySetSHA256 ||
		trace.OutcomeSetSHA256 != step.OutcomeSetSHA256 || trace.Rejected != step.Rejected ||
		trace.NoResult != step.Rejected || trace.NoAvailableArtifact != step.Rejected || trace.NoSuccessfulAudit != step.Rejected {
		return errors.New("adaptive transition does not reconstruct exactly")
	}
	return nil
}

func validateAttackAcceptedRows(step AttackStepEvidence) error {
	if !validSHA256(step.ResultSHA256) || !validSHA256(step.RowSetSHA256) || step.RowCount <= 0 ||
		step.ColumnCount <= 0 || int64(len(step.RowSHA256)) != step.RowCount || step.ObservedErrorCode != "" ||
		step.ObservedErrorReason != "" || step.TraceIDHash != "" || step.RejectedQuery != nil {
		return errors.New("accepted attack step lacks exact result shape or carries rejection evidence")
	}
	rowSet, err := finalv5attack.RowSetSHA256(step.RowSHA256)
	if err != nil || rowSet != step.RowSetSHA256 {
		return errors.New("accepted attack row set does not recompute")
	}
	return nil
}

func validateDirectAttackStep(step AttackStepEvidence) error {
	if step.RequestIDHash != "" || step.QueryIDHash != "" || step.ResultIDHash != "" || step.PlanSHA256 != "" ||
		len(step.ResultMetadataJSON) != 0 || step.ObservationSHA256 != "" || step.ReleaseSetSHA256 != "" ||
		step.DependencySetSHA256 != "" || step.OutcomeSetSHA256 != "" || step.ActualReleaseFacts != 0 ||
		step.ChargedReleaseFacts != 0 || step.ActualDependencyFacts != 0 || step.ChargedDependencyFacts != 0 ||
		step.ActualOutcomeFacts != 0 || step.ChargedOutcomeFacts != 0 || step.PredicateAtomCount != 0 ||
		step.CompositeCount != 0 || step.SemanticReplay || step.IdempotentReplay || step.RootTaskIDHash != "" ||
		step.RootEpochAfter != 0 || step.ArtifactSHA256 != "" || step.ObjectSHA256 != "" || step.ParquetBytes != 0 ||
		step.EncryptedObjectBytes != 0 || step.ReceiptVersion != "" || step.ReceiptSHA256 != "" ||
		step.ArtifactIntentSHA256 != "" || step.AvailabilitySHA256 != "" || step.Before != nil || step.After != nil ||
		step.Verification != nil {
		return errors.New("direct PostgreSQL step contains fabricated TaskGate evidence")
	}
	return nil
}

func validateAttackReleasedStep(parent Sample, step AttackStepEvidence) error {
	if step.Verification == nil || step.Before == nil || step.After == nil || !currentReceiptVersion(step.ReceiptVersion) ||
		!validSHA256(step.RequestIDHash) || !validSHA256(step.QueryIDHash) || !validSHA256(step.ResultIDHash) ||
		!validSHA256(step.PlanSHA256) || !validSHA256(step.ObservationSHA256) || !validSHA256(step.ReleaseSetSHA256) ||
		!validSHA256(step.DependencySetSHA256) || !validSHA256(step.OutcomeSetSHA256) ||
		!validSHA256(step.RootTaskIDHash) || step.RootEpochAfter <= 0 || !validSHA256(step.ArtifactSHA256) ||
		!validSHA256(step.ObjectSHA256) || step.ParquetBytes <= 0 || step.EncryptedObjectBytes <= 0 ||
		!validSHA256(step.ReceiptSHA256) || !validSHA256(step.ArtifactIntentSHA256) ||
		!validSHA256(step.AvailabilitySHA256) {
		return errors.New("released TaskGate step lacks current Receipt/Control/MinIO evidence")
	}
	temporary := attackReleasedSample(parent, step)
	if err := validateBaselineVerification(temporary); err != nil {
		return fmt.Errorf("V8/audit verification: %w", err)
	}
	if err := validateRedactedVerifierManifest(temporary); err != nil {
		return fmt.Errorf("composite verifier manifest: %w", err)
	}
	receipt := step.Verification.Receipt
	intent := receipt.ArtifactIntent
	if intent == nil || step.RequestIDHash != redactedIdentitySHA256(parent, "request", receipt.RequestID) ||
		step.QueryIDHash != redactedIdentitySHA256(parent, "query", receipt.QueryID) ||
		step.ResultIDHash != redactedIdentitySHA256(parent, "result", intent.ResultID) {
		return errors.New("redacted request/query/result identities differ from the signed receipt")
	}
	if !json.Valid(step.ResultMetadataJSON) || sha256Hex(step.ResultMetadataJSON) != intent.ResultMetadataSHA256 {
		return errors.New("result metadata bytes differ from the signed artifact intent")
	}
	var metadata struct {
		PlanDigest string `json:"plan_digest"`
	}
	if err := json.Unmarshal(step.ResultMetadataJSON, &metadata); err != nil || metadata.PlanDigest != step.PlanSHA256 {
		return errors.New("response plan is not bound by signed result metadata")
	}
	if err := validateAttackControlSnapshot(*step.Before); err != nil {
		return fmt.Errorf("before snapshot: %w", err)
	}
	if err := validateAttackControlSnapshot(*step.After); err != nil {
		return fmt.Errorf("after snapshot: %w", err)
	}
	if err := validateRootLedgerSnapshot(step.After.Root); err != nil || step.After.Root.Epoch != step.RootEpochAfter {
		return errors.New("released step root epoch/snapshot is invalid")
	}
	if step.SemanticReplay && step.IdempotentReplay {
		return errors.New("released step claims two replay modes")
	}
	if step.IdempotentReplay {
		if *step.Before != *step.After {
			return errors.New("request-ID replay changed Business, Control, root, audit, or canonical-object state")
		}
		return nil
	}
	if step.Before.TaskIDHash != step.After.TaskIDHash || step.Before.RootTaskIDHash != step.After.RootTaskIDHash ||
		step.Before.Product != step.After.Product || step.Before.BudgetProfile != step.After.BudgetProfile ||
		step.Before.MaxQueries != step.After.MaxQueries || step.Before.MaxOutcomeFacts != step.After.MaxOutcomeFacts ||
		step.Before.ReservedQueries != 0 || step.After.ReservedQueries != 0 ||
		step.After.UsedQueries != step.Before.UsedQueries+1 {
		return errors.New("released step resource budget/task binding transition is invalid")
	}
	visibleDelta, companionDelta := int64(1), int64(1)
	if step.SemanticReplay {
		visibleDelta, companionDelta = 0, 0
		if step.Before.Root != step.After.Root || step.ChargedReleaseFacts != 0 ||
			step.ChargedDependencyFacts != 0 || step.ChargedOutcomeFacts != 0 {
			return errors.New("semantic replay changed the root or charged exposure facts")
		}
	} else if step.After.Root.Epoch != step.Before.Root.Epoch+1 ||
		step.After.Root.ReleaseCardinality < step.Before.Root.ReleaseCardinality ||
		step.After.Root.DependencyCardinality < step.Before.Root.DependencyCardinality ||
		step.After.Root.OutcomeCardinality < step.Before.Root.OutcomeCardinality ||
		step.After.Root.RootObservationCount != step.Before.Root.RootObservationCount+1 {
		return errors.New("novel attack release did not append exactly one monotone root observation")
	}
	if err := validateBusinessSQLTransition(step.Before.Business, step.After.Business, visibleDelta, companionDelta); err != nil {
		return err
	}
	if err := validateAttackSnapshotDelta(*step.Before, *step.After, expectedAttackReleasedSnapshotDelta(step)); err != nil {
		return err
	}
	return nil
}

func expectedAttackReleasedSnapshotDelta(step AttackStepEvidence) attackSnapshotDelta {
	successfulAudits := int64(4)
	if step.SemanticReplay {
		// A semantic replay creates a new query/result/artifact chain but reuses
		// the committed root observation, so it has no novel root-settlement
		// audit. The remaining query, registration, and availability audits are
		// still required exactly.
		successfulAudits = 3
	}
	return attackSnapshotDelta{
		QueryRecords: 1, Settlements: 1, Observations: 1, Receipts: 1, Artifacts: 1,
		AvailableArtifacts: 1, SuccessfulAudits: successfulAudits, FailureAudits: 0, CanonicalObjects: 1,
	}
}

func attackReleasedSample(parent Sample, step AttackStepEvidence) Sample {
	return Sample{
		CampaignID: parent.CampaignID, DeploymentID: parent.DeploymentID,
		ResultSHA256: step.ResultSHA256, RowCount: step.RowCount, ColumnCount: step.ColumnCount,
		ReleaseSetSHA256: step.ReleaseSetSHA256, DependencySetSHA256: step.DependencySetSHA256,
		OutcomeSetSHA256: step.OutcomeSetSHA256, ActualReleaseFacts: step.ActualReleaseFacts,
		ChargedReleaseFacts: step.ChargedReleaseFacts, ActualDependencyFacts: step.ActualDependencyFacts,
		ChargedDependencyFacts: step.ChargedDependencyFacts, ActualOutcomeFacts: step.ActualOutcomeFacts,
		ChargedOutcomeFacts: step.ChargedOutcomeFacts, PredicateAtomCount: step.PredicateAtomCount,
		CompositeCount: step.CompositeCount, RootEpochAfter: step.RootEpochAfter, RootTaskIDHash: step.RootTaskIDHash,
		ArtifactSHA256: step.ArtifactSHA256, ObjectSHA256: step.ObjectSHA256, ParquetBytes: step.ParquetBytes,
		EncryptedObjectBytes: step.EncryptedObjectBytes, ReceiptVersion: step.ReceiptVersion,
		ReceiptSHA256: step.ReceiptSHA256, ArtifactIntentSHA256: step.ArtifactIntentSHA256,
		AvailabilityAuditSHA256: step.AvailabilitySHA256, BaselineVerification: step.Verification,
	}
}

func validateAttackControlSnapshot(snapshot AttackControlSnapshot) error {
	if !validSHA256(snapshot.TaskIDHash) || !validSHA256(snapshot.RootTaskIDHash) ||
		snapshot.Product == "" || snapshot.BudgetProfile == "" || snapshot.MaxQueries <= 0 ||
		snapshot.MaxOutcomeFacts <= 0 || snapshot.UsedQueries < 0 || snapshot.ReservedQueries < 0 ||
		snapshot.UsedQueries+snapshot.ReservedQueries > snapshot.MaxQueries {
		return errors.New("Control snapshot task/product/profile resource budget binding is invalid")
	}
	if err := validateBusinessSQLSnapshot(snapshot.Business); err != nil {
		return err
	}
	if snapshot.Root.Epoch == 0 {
		if err := validateFreshRootLedgerSnapshot(snapshot.Root); err != nil {
			return err
		}
	} else if err := validateRootLedgerSnapshot(snapshot.Root); err != nil {
		return err
	}
	for name, value := range map[string]int64{
		"query records": snapshot.QueryRecords, "settlements": snapshot.Settlements,
		"observations": snapshot.Observations, "receipts": snapshot.Receipts, "artifacts": snapshot.Artifacts,
		"available artifacts": snapshot.AvailableArtifacts, "successful audits": snapshot.SuccessfulAudits,
		"failure audits": snapshot.FailureAudits, "canonical objects": snapshot.CanonicalObjects,
	} {
		if value < 0 {
			return fmt.Errorf("%s counter is negative", name)
		}
	}
	if snapshot.AvailableArtifacts > snapshot.Artifacts {
		return errors.New("available artifact count exceeds registered artifacts")
	}
	return nil
}

type attackSnapshotDelta struct {
	QueryRecords, Settlements, Observations, Receipts, Artifacts int64
	AvailableArtifacts, SuccessfulAudits, FailureAudits          int64
	CanonicalObjects                                             int64
}

func validateAttackSnapshotDelta(before, after AttackControlSnapshot, want attackSnapshotDelta) error {
	actual := attackSnapshotDelta{
		QueryRecords:       after.QueryRecords - before.QueryRecords,
		Settlements:        after.Settlements - before.Settlements,
		Observations:       after.Observations - before.Observations,
		Receipts:           after.Receipts - before.Receipts,
		Artifacts:          after.Artifacts - before.Artifacts,
		AvailableArtifacts: after.AvailableArtifacts - before.AvailableArtifacts,
		SuccessfulAudits:   after.SuccessfulAudits - before.SuccessfulAudits,
		FailureAudits:      after.FailureAudits - before.FailureAudits,
		CanonicalObjects:   after.CanonicalObjects - before.CanonicalObjects,
	}
	if actual != want {
		return fmt.Errorf("Control/MinIO transition = %+v, want %+v", actual, want)
	}
	return nil
}

func validateAttackRejectedStep(sample Sample, expected finalv5attack.Step, step AttackStepEvidence) error {
	if sample.System != "taskgate" || expected.Classification != "expected_rejection" || step.Accepted || !step.Rejected ||
		step.ObservedErrorCode != expected.ExpectedErrorCode || step.ObservedErrorReason != expectedAttackErrorReason(expected) ||
		!validSHA256(step.TraceIDHash) || step.Before == nil || step.After == nil || step.RejectedQuery == nil {
		return errors.New("structured rejection differs from the frozen corpus")
	}
	if step.RowCount != 0 || step.ColumnCount != 0 || step.ResultSHA256 != "" || step.ScalarInt64 != nil ||
		len(step.RowSHA256) != 0 || step.RowSetSHA256 != "" || step.PlanSHA256 != "" || len(step.ResultMetadataJSON) != 0 ||
		step.ObservationSHA256 != "" || step.ReleaseSetSHA256 != "" || step.DependencySetSHA256 != "" ||
		step.OutcomeSetSHA256 != "" || step.ActualReleaseFacts != 0 || step.ChargedReleaseFacts != 0 ||
		step.ActualDependencyFacts != 0 || step.ChargedDependencyFacts != 0 || step.ActualOutcomeFacts != 0 ||
		step.ChargedOutcomeFacts != 0 || step.PredicateAtomCount != 0 || step.CompositeCount != 0 ||
		step.SemanticReplay || step.IdempotentReplay || step.RootTaskIDHash != "" || step.RootEpochAfter != 0 ||
		step.ArtifactSHA256 != "" || step.ObjectSHA256 != "" || step.ParquetBytes != 0 || step.EncryptedObjectBytes != 0 ||
		step.ReceiptVersion != "" || step.ReceiptSHA256 != "" || step.ArtifactIntentSHA256 != "" ||
		step.AvailabilitySHA256 != "" || step.Verification != nil || !validSHA256(step.RequestIDHash) ||
		step.QueryIDHash != "" || step.ResultIDHash != "" {
		return errors.New("rejected attack step carries released result/artifact/exposure evidence")
	}
	if err := validateAttackControlSnapshot(*step.Before); err != nil {
		return err
	}
	if err := validateAttackControlSnapshot(*step.After); err != nil {
		return err
	}
	if step.Before.Root != step.After.Root {
		return errors.New("fail-closed rejection changed signed root state")
	}
	if step.Before.Artifacts != step.After.Artifacts || step.Before.AvailableArtifacts != step.After.AvailableArtifacts {
		return fmt.Errorf("fail-closed rejection changed artifact state (%d/%d -> %d/%d)",
			step.Before.Artifacts, step.Before.AvailableArtifacts, step.After.Artifacts, step.After.AvailableArtifacts)
	}
	if step.Before.SuccessfulAudits != step.After.SuccessfulAudits {
		return fmt.Errorf("fail-closed rejection changed successful-audit state (%d -> %d)",
			step.Before.SuccessfulAudits, step.After.SuccessfulAudits)
	}
	if step.Before.CanonicalObjects != step.After.CanonicalObjects {
		return fmt.Errorf("fail-closed rejection changed canonical-object state (%d -> %d)",
			step.Before.CanonicalObjects, step.After.CanonicalObjects)
	}
	if expected.ExpectedErrorCode == "SQL_NOT_LOWERABLE" {
		if *step.RejectedQuery != (AttackRejectedQueryEvidence{}) {
			return errors.New("SQL lowering rejection created a query-specific Control projection")
		}
		if err := validateBusinessSQLTransition(step.Before.Business, step.After.Business, 0, 0); err != nil {
			return err
		}
		if step.Before.TaskIDHash != step.After.TaskIDHash || step.Before.RootTaskIDHash != step.After.RootTaskIDHash ||
			step.Before.Product != step.After.Product || step.Before.BudgetProfile != step.After.BudgetProfile ||
			step.Before.MaxQueries != step.After.MaxQueries || step.Before.MaxOutcomeFacts != step.After.MaxOutcomeFacts ||
			step.Before.UsedQueries != step.After.UsedQueries || step.Before.ReservedQueries != 0 || step.After.ReservedQueries != 0 {
			return errors.New("SQL lowering rejection changed task/profile/resource budget state")
		}
		if err := validateAttackSnapshotDelta(*step.Before, *step.After, attackSnapshotDelta{}); err != nil {
			return err
		}
		return nil
	}
	if expected.ExpectedErrorCode != "EXPOSURE_BUDGET_EXHAUSTED" {
		return errors.New("unregistered attack rejection code")
	}
	projection := step.RejectedQuery
	if !projection.Found || !validSHA256(projection.QueryIDHash) || projection.Status != "FAILED" ||
		projection.ErrorCode != expected.ExpectedErrorCode || projection.ResultSHA256 != "" ||
		projection.ReservationStatus != "RELEASED" || projection.EncryptedResults != 0 || projection.EncryptedChunks != 0 ||
		projection.Materializations != 0 || projection.QueryObservations != 0 || projection.RootObservations != 0 ||
		projection.Artifacts != 0 || projection.AvailableArtifacts != 0 || projection.AvailabilityAudits != 0 ||
		projection.SuccessfulAudits != 0 || projection.FailureAudits != 2 || projection.Receipts != 1 {
		return errors.New("exposure rejection did not retain the exact failure-only Control projection")
	}
	if err := validateBusinessSQLTransition(step.Before.Business, step.After.Business, 1, 1); err != nil {
		return err
	}
	if step.Before.TaskIDHash != step.After.TaskIDHash || step.Before.RootTaskIDHash != step.After.RootTaskIDHash ||
		step.Before.Product != step.After.Product || step.Before.BudgetProfile != step.After.BudgetProfile ||
		step.Before.MaxQueries != step.After.MaxQueries || step.Before.MaxOutcomeFacts != step.After.MaxOutcomeFacts ||
		step.Before.UsedQueries >= step.Before.MaxQueries || step.Before.ReservedQueries != 0 || step.After.ReservedQueries != 0 ||
		step.After.UsedQueries != step.Before.UsedQueries+1 {
		return errors.New("exposure rejection was not proven independent of resource query exhaustion")
	}
	return validateAttackSnapshotDelta(*step.Before, *step.After, attackSnapshotDelta{
		QueryRecords: 1, Receipts: 1, FailureAudits: 2,
	})
}

func expectedAttackErrorReason(expected finalv5attack.Step) string {
	if expected.ExpectedErrorReason != "" {
		return expected.ExpectedErrorReason
	}
	if expected.ExpectedErrorCode == "EXPOSURE_BUDGET_EXHAUSTED" {
		return "ROOT_OUTCOME_CEILING_EXCEEDED"
	}
	return "UNREGISTERED_STRUCTURED_REJECTION"
}

func validateAttackCaseSemantics(sample Sample, attackCase finalv5attack.Case) error {
	steps := sample.AttackVerification.Steps
	if sample.WorkloadID != "E-threshold" {
		if len(sample.AttackVerification.ExpectedThresholds) != 0 || len(sample.AttackVerification.ObservedThresholdResults) != 0 ||
			sample.AttackVerification.OutcomeCeiling != 0 || sample.AttackVerification.ObservedOutcome != 0 ||
			sample.AttackVerification.ThresholdRejectionIndex != 0 {
			return errors.New("non-threshold attack cell carries threshold evidence")
		}
	}
	if sample.System == "taskgate" {
		for index, expected := range attackCase.Steps {
			wantSemantic := expected.Classification == "semantic_replay" ||
				(expected.Classification == "accepted_equivalent" && index > 0)
			wantIdempotent := false
			if sample.WorkloadID == "C-request-id" {
				wantSemantic = sample.Mode == "semantic_replay"
				wantIdempotent = sample.Mode == "idempotent_replay"
			}
			if steps[index].Accepted && (steps[index].SemanticReplay != wantSemantic || steps[index].IdempotentReplay != wantIdempotent) {
				return fmt.Errorf("step %d replay classification differs from the preregistered mode", index+1)
			}
		}
	}
	switch sample.WorkloadID {
	case "A-pagination", "D-split-union":
		return validateAttackDecomposition(sample)
	case "B-equivalent-sql":
		return validateAttackEquivalentSQL(sample)
	case "C-request-id":
		return validateAttackRequestIdentity(sample)
	case "E-threshold":
		return validateAttackThreshold(sample, attackCase)
	default:
		return errors.New("unregistered attack case")
	}
}

func validateAttackDecomposition(sample Sample) error {
	complete, partitions := []string{}, []string{}
	var replaySource, replayStep *AttackStepEvidence
	for index := range sample.AttackVerification.Steps {
		step := &sample.AttackVerification.Steps[index]
		if step.Accepted && step.Role == "complete" {
			complete = append(complete, step.RowSHA256...)
		}
		if step.Accepted && step.Role == "partition" {
			partitions = append(partitions, step.RowSHA256...)
		}
		if step.VariantID == "page-1" {
			replaySource = step
		}
		if step.Role == "replay" {
			replayStep = step
		}
	}
	completeDigest, completeErr := finalv5attack.RowSetSHA256(complete)
	partitionDigest, partitionErr := finalv5attack.RowSetSHA256(partitions)
	if completeErr != nil || partitionErr != nil || completeDigest != partitionDigest ||
		sample.AttackVerification.CompleteRowSetSHA256 != completeDigest ||
		sample.AttackVerification.DecomposedRowSetSHA256 != partitionDigest {
		return errors.New("complete result and exact decomposed union differ")
	}
	if sample.WorkloadID == "A-pagination" {
		if replaySource == nil || replayStep == nil || replaySource.ResultSHA256 != replayStep.ResultSHA256 ||
			replaySource.RowSetSHA256 != replayStep.RowSetSHA256 ||
			(sample.System == "taskgate" && replaySource.PlanSHA256 != replayStep.PlanSHA256) {
			return errors.New("pagination replay differs from its source page")
		}
	}
	if len(sample.AttackVerification.NormalFormSHA256) != 0 {
		return errors.New("decomposition attack unexpectedly carries equivalent-SQL normal forms")
	}
	return nil
}

func validateAttackEquivalentSQL(sample Sample) error {
	steps := sample.AttackVerification.Steps
	if len(steps) != 5 {
		return errors.New("equivalent-SQL attack step count changed")
	}
	baseline := steps[0]
	for index := 1; index < 4; index++ {
		if !steps[index].Accepted || steps[index].ResultSHA256 != baseline.ResultSHA256 ||
			steps[index].RowSetSHA256 != baseline.RowSetSHA256 ||
			(sample.System == "taskgate" && steps[index].PlanSHA256 != baseline.PlanSHA256) {
			return errors.New("equivalent SQL did not retain exact result/row-set/normal form")
		}
	}
	if sample.System == "postgresql" {
		if len(sample.AttackVerification.NormalFormSHA256) != 0 || !steps[4].Accepted ||
			steps[4].RowSetSHA256 != baseline.RowSetSHA256 {
			return errors.New("direct equivalent-SQL/UNION control differs from the expected relation")
		}
		return nil
	}
	if len(sample.AttackVerification.NormalFormSHA256) != 4 {
		return errors.New("TaskGate equivalent-SQL normal-form sequence is incomplete")
	}
	for index, digest := range sample.AttackVerification.NormalFormSHA256 {
		if digest != steps[index].PlanSHA256 || digest != baseline.PlanSHA256 {
			return errors.New("TaskGate equivalent-SQL normal forms differ")
		}
	}
	return nil
}

func validateAttackRequestIdentity(sample Sample) error {
	evidence := sample.AttackVerification
	if len(evidence.Steps) != 1 || !evidence.Steps[0].Accepted || !validSHA256(evidence.AnchorRequestIDHash) ||
		!validSHA256(evidence.AnchorQueryIDHash) || !validSHA256(evidence.AnchorResultIDHash) ||
		len(evidence.NormalFormSHA256) != 0 {
		return errors.New("request-ID control lacks a complete novel anchor")
	}
	step := evidence.Steps[0]
	switch sample.Mode {
	case "novel":
		if step.SemanticReplay || step.IdempotentReplay || step.RequestIDHash != evidence.AnchorRequestIDHash ||
			step.QueryIDHash != evidence.AnchorQueryIDHash || step.ResultIDHash != evidence.AnchorResultIDHash {
			return errors.New("novel request-ID result does not establish its own anchor")
		}
	case "semantic_replay":
		if !step.SemanticReplay || step.IdempotentReplay || step.RequestIDHash == evidence.AnchorRequestIDHash ||
			step.QueryIDHash == evidence.AnchorQueryIDHash || step.ResultIDHash == evidence.AnchorResultIDHash {
			return errors.New("semantic replay reused novel request/query/result identity")
		}
	case "idempotent_replay":
		if step.SemanticReplay || !step.IdempotentReplay || step.RequestIDHash != evidence.AnchorRequestIDHash ||
			step.QueryIDHash != evidence.AnchorQueryIDHash || step.ResultIDHash != evidence.AnchorResultIDHash ||
			*step.Before != *step.After {
			return errors.New("idempotent request replay changed identity or observable state")
		}
	default:
		return errors.New("unregistered request-ID mode")
	}
	return nil
}

func validateAttackThreshold(sample Sample, attackCase finalv5attack.Case) error {
	evidence := sample.AttackVerification
	if !attackEqualInt64s(evidence.ExpectedThresholds, attackCase.Thresholds) || evidence.OutcomeCeiling != attackCase.OutcomeCeiling ||
		attackCase.OutcomeCeiling != 5 || len(attackCase.ThresholdResults) != len(attackCase.Thresholds) {
		return errors.New("threshold/ceiling preregistration differs from the frozen corpus")
	}
	observed := make([]int64, 0, len(attackCase.Thresholds))
	thresholdIndex := map[int64]int{}
	for index, threshold := range attackCase.Thresholds {
		thresholdIndex[threshold] = index
	}
	var sourcePrimer *AttackStepEvidence
	for index := range evidence.Steps {
		step := &evidence.Steps[index]
		expected := attackCase.Steps[index]
		if expected.Threshold > 0 && step.Accepted {
			position, found := thresholdIndex[expected.Threshold]
			if !found || step.ScalarInt64 == nil || *step.ScalarInt64 != attackCase.ThresholdResults[position] {
				return fmt.Errorf("threshold %d result differs from the frozen dataset oracle", expected.Threshold)
			}
			observed = append(observed, *step.ScalarInt64)
		}
		if expected.ID == "outcome-primer-320-detail" {
			sourcePrimer = step
			if !step.Accepted || step.RowCount != attackCase.ThresholdResults[1] || step.ColumnCount != 2 || step.ScalarInt64 != nil {
				return errors.New("outcome-five primer differs from the frozen six-row detail relation")
			}
		}
		if expected.Classification == "semantic_replay" {
			if sourcePrimer == nil || !step.Accepted || step.ResultSHA256 != sourcePrimer.ResultSHA256 ||
				step.RowSetSHA256 != sourcePrimer.RowSetSHA256 || step.RowCount != sourcePrimer.RowCount ||
				step.ColumnCount != sourcePrimer.ColumnCount || step.ScalarInt64 != nil ||
				(sample.System == "taskgate" && step.PlanSHA256 != sourcePrimer.PlanSHA256) {
				return errors.New("outcome-five replay/rewrite differs from its detail primer")
			}
		}
	}
	if !attackEqualInt64s(evidence.ObservedThresholdResults, observed) {
		return errors.New("observed threshold result sequence does not reconstruct")
	}
	if sample.System == "postgresql" {
		if !attackEqualInt64s(observed, attackCase.ThresholdResults) || evidence.ThresholdRejectionIndex != 0 ||
			evidence.ObservedOutcome != 0 || evidence.FinalRoot != nil {
			return errors.New("direct threshold control did not execute all frozen thresholds")
		}
		return nil
	}
	if len(observed) != len(attackCase.ThresholdResults)-1 ||
		!attackEqualInt64s(observed, attackCase.ThresholdResults[:len(attackCase.ThresholdResults)-1]) ||
		evidence.ThresholdRejectionIndex != len(attackCase.Thresholds) || evidence.ObservedOutcome != evidence.OutcomeCeiling ||
		evidence.FinalRoot == nil || evidence.FinalRoot.OutcomeCardinality != evidence.OutcomeCeiling ||
		!evidence.Steps[len(evidence.Steps)-1].Rejected ||
		evidence.Steps[len(evidence.Steps)-1].ObservedErrorCode != "EXPOSURE_BUDGET_EXHAUSTED" {
		return errors.New("TaskGate threshold attack did not stop exactly at ceiling plus one")
	}
	return validateAttackEOutcomeSequence(evidence.Steps)
}

func validateAttackEOutcomeSequence(steps []AttackStepEvidence) error {
	if len(steps) != 6 {
		return errors.New("E outcome sequence is incomplete")
	}
	exposureAt := func(index int) (*queryreceipt.ExposureEvidenceV1, error) {
		step := steps[index]
		if step.Verification == nil || step.Verification.Receipt.Exposure == nil {
			return nil, fmt.Errorf("E step %d lacks signed V8 exposure evidence", index+1)
		}
		return step.Verification.Receipt.Exposure, nil
	}
	first, err := exposureAt(0)
	if err != nil {
		return err
	}
	second, err := exposureAt(1)
	if err != nil {
		return err
	}
	primer, err := exposureAt(2)
	if err != nil {
		return err
	}
	if steps[0].Before.Root.OutcomeCardinality != 0 || steps[0].After.Root.OutcomeCardinality != 2 ||
		steps[1].Before.Root.OutcomeCardinality != 2 || steps[1].After.Root.OutcomeCardinality != 4 ||
		steps[0].ChargedOutcomeFacts != 2 || steps[1].ChargedOutcomeFacts != 2 ||
		first.ChargedPredicateAtomCount != 1 || first.ChargedCompositeCount != 1 ||
		second.ChargedPredicateAtomCount != 1 || second.ChargedCompositeCount != 1 {
		return errors.New("E threshold prefix did not create the exact two-plus-two outcome facts")
	}
	if steps[2].Before.Root.OutcomeCardinality != 4 || steps[2].After.Root.OutcomeCardinality != 5 ||
		steps[2].ChargedOutcomeFacts != 1 || steps[2].PredicateAtomCount != 1 || steps[2].CompositeCount != 1 ||
		primer.ActualPredicateAtomCount != 1 || primer.ChargedPredicateAtomCount != 0 ||
		primer.ActualCompositeCount != 1 || primer.ChargedCompositeCount != 1 ||
		primer.PredicateContextSHA256 != second.PredicateContextSHA256 ||
		primer.PredicateSetSHA256 != second.PredicateSetSHA256 ||
		primer.CompositeOutcomeSHA256 == second.CompositeOutcomeSHA256 || steps[2].PlanSHA256 == steps[1].PlanSHA256 {
		return errors.New("E primer did not reuse its predicate atom and add exactly one new composite")
	}
	for _, index := range []int{3, 4} {
		replay, replayErr := exposureAt(index)
		if replayErr != nil {
			return replayErr
		}
		if steps[index].Before.Root.OutcomeCardinality != 5 || steps[index].After.Root.OutcomeCardinality != 5 ||
			steps[index].Before.Root != steps[index].After.Root || steps[index].ChargedOutcomeFacts != 0 ||
			!steps[index].SemanticReplay || steps[index].PlanSHA256 != steps[2].PlanSHA256 ||
			steps[index].ResultSHA256 != steps[2].ResultSHA256 || replay.PredicateSetSHA256 != primer.PredicateSetSHA256 ||
			replay.CompositeOutcomeSHA256 != primer.CompositeOutcomeSHA256 || replay.ChargedPredicateAtomCount != 0 ||
			replay.ChargedCompositeCount != 0 {
			return fmt.Errorf("E replay/rewrite step %d changed the outcome-five root", index+1)
		}
	}
	if steps[5].Before == nil || steps[5].After == nil ||
		steps[5].Before.Root.OutcomeCardinality != 5 || steps[5].After.Root.OutcomeCardinality != 5 {
		return errors.New("E B+1 rejection was not attempted against exact root outcome five")
	}
	return nil
}

func validateAttackTaskgateSummary(sample Sample) error {
	evidence := sample.AttackVerification
	if evidence.FinalRoot == nil {
		return errors.New("TaskGate attack lacks final root evidence")
	}
	wantObservedOutcome := int64(0)
	if sample.WorkloadID == "E-threshold" {
		wantObservedOutcome = evidence.FinalRoot.OutcomeCardinality
	}
	var firstBefore *AttackControlSnapshot
	var lastAfter *AttackControlSnapshot
	var lastReleased *AttackStepEvidence
	for index := range evidence.Steps {
		step := &evidence.Steps[index]
		if firstBefore == nil && step.Before != nil {
			firstBefore = step.Before
		}
		if step.After != nil {
			lastAfter = step.After
		}
		if step.Accepted {
			lastReleased = step
		}
	}
	if firstBefore == nil || lastAfter == nil || lastReleased == nil || *evidence.FinalRoot != lastAfter.Root ||
		sample.RootEpochBefore != firstBefore.Root.Epoch || sample.RootEpochAfter != lastAfter.Root.Epoch ||
		sample.RootSetSHA256Before != rootLedgerSetSHA256(firstBefore.Root) ||
		sample.RootSetSHA256After != rootLedgerSetSHA256(lastAfter.Root) ||
		sample.ReleaseSetSHA256 != lastAfter.Root.ReleaseSetSHA256 ||
		sample.DependencySetSHA256 != lastAfter.Root.DependencySetSHA256 ||
		sample.OutcomeSetSHA256 != lastAfter.Root.OutcomeSetSHA256 ||
		sample.ActualReleaseFacts != lastAfter.Root.ReleaseCardinality ||
		sample.ActualDependencyFacts != lastAfter.Root.DependencyCardinality ||
		sample.ActualOutcomeFacts != lastAfter.Root.OutcomeCardinality ||
		sample.ChargedReleaseFacts != lastAfter.Root.ReleaseCardinality-firstBefore.Root.ReleaseCardinality ||
		sample.ChargedDependencyFacts != lastAfter.Root.DependencyCardinality-firstBefore.Root.DependencyCardinality ||
		sample.ChargedOutcomeFacts != lastAfter.Root.OutcomeCardinality-firstBefore.Root.OutcomeCardinality ||
		evidence.ObservedOutcome != wantObservedOutcome {
		return errors.New("TaskGate top-level root summary differs from step snapshots")
	}
	if sample.RootTaskIDHash != lastReleased.RootTaskIDHash || sample.ArtifactSHA256 != lastReleased.ArtifactSHA256 ||
		sample.ObjectSHA256 != lastReleased.ObjectSHA256 || sample.ParquetBytes != lastReleased.ParquetBytes ||
		sample.EncryptedObjectBytes != lastReleased.EncryptedObjectBytes || sample.ReceiptVersion != lastReleased.ReceiptVersion ||
		sample.ReceiptSHA256 != lastReleased.ReceiptSHA256 || sample.ArtifactIntentSHA256 != lastReleased.ArtifactIntentSHA256 ||
		sample.AvailabilityAuditSHA256 != lastReleased.AvailabilitySHA256 || !sample.ReceiptVerified || !sample.ArtifactAvailable {
		return errors.New("TaskGate top-level artifact summary differs from the last released step")
	}
	wantSemantic := sample.Mode == "semantic_replay"
	wantIdempotent := sample.Mode == "idempotent_replay"
	if sample.SemanticReplay != wantSemantic || sample.IdempotentReplay != wantIdempotent {
		return errors.New("TaskGate top-level replay markers differ from operation mode")
	}
	businessDelta := lastAfter.Business.VisibleCalls - firstBefore.Business.VisibleCalls +
		lastAfter.Business.CompanionCalls - firstBefore.Business.CompanionCalls
	if sample.BusinessSQLDelta != businessDelta {
		return errors.New("TaskGate top-level Business SQL delta differs from independent snapshots")
	}
	return nil
}

func validateAttackDirectSummary(sample Sample) error {
	evidence := sample.AttackVerification
	if evidence.FinalRoot != nil || evidence.ObservedOutcome != 0 || evidence.AnchorRequestIDHash != "" ||
		evidence.AnchorQueryIDHash != "" || evidence.AnchorResultIDHash != "" || sample.ReleaseSetSHA256 != "" ||
		sample.DependencySetSHA256 != "" || sample.OutcomeSetSHA256 != "" || sample.ArtifactSHA256 != "" ||
		sample.ObjectSHA256 != "" || sample.ActualReleaseFacts != 0 || sample.ChargedReleaseFacts != 0 ||
		sample.ActualDependencyFacts != 0 || sample.ChargedDependencyFacts != 0 || sample.ActualOutcomeFacts != 0 ||
		sample.ChargedOutcomeFacts != 0 || sample.SemanticReplay || sample.IdempotentReplay || sample.BusinessSQLDelta != 0 ||
		sample.RootEpochBefore != 0 || sample.RootEpochAfter != 0 || sample.RootTaskIDHash != "" ||
		sample.RootSetSHA256Before != "" || sample.RootSetSHA256After != "" || sample.ParquetBytes != 0 ||
		sample.EncryptedObjectBytes != 0 || sample.ReceiptVersion != "" || sample.ReceiptSHA256 != "" ||
		sample.ArtifactIntentSHA256 != "" || sample.AvailabilityAuditSHA256 != "" || sample.ReceiptVerified ||
		sample.ArtifactAvailable {
		return errors.New("direct PostgreSQL summary contains fabricated TaskGate evidence")
	}
	return nil
}

func attackSQLSequenceDigest(attackCase finalv5attack.Case, direct bool) string {
	values := make([]string, 0, len(attackCase.Steps))
	for _, step := range attackCase.Steps {
		value := step.LogicalSQL
		if direct {
			value = step.DirectSQL
		}
		values = append(values, value)
	}
	encoded := ""
	for index, value := range values {
		if index > 0 {
			encoded += "\x00"
		}
		encoded += value
	}
	return sha256Hex([]byte(encoded))
}

func attackEqualInt64s(left, right []int64) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
