package main

import (
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/internal/querybinding"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

func provSQLRegistrationForTest() experiment.PreRegisteredObservationV3 {
	return experiment.PreRegisteredObservationV3{
		Operation: experiment.OperationIdentity{
			OperationID:      "provsql/nonce-join-group/10k/taskgate",
			PathKind:         experiment.PathPairedNovel,
			ContractIdentity: "release:index:binding-file:binding-section:binding-key:binding-query:provsql/nonce-join-group/10k/taskgate",
		},
		Plan:                     experiment.GatewayControlPlanV3{PathKind: experiment.PathPairedNovel},
		ClassifierManifestSHA256: strings.Repeat("a", 64),
		ClassifierBindingSHA256:  strings.Repeat("b", 64),
	}
}

func signedProvSQLBinding() *querybinding.QueryExecutionBindingV2 {
	visible := querybinding.TargetRecordV1{
		Role: querybinding.RoleVisible, Authorized: true, Executed: true,
		ExactSQLSHA256: strings.Repeat("1", 64), StrictASTSHA256: strings.Repeat("2", 64),
		RowLimit: 100, PolicyFingerprint: strings.Repeat("3", 64),
		PreparedTargetBindingSHA256: strings.Repeat("4", 64),
	}
	companion := querybinding.TargetRecordV1{
		Role: querybinding.RoleCompanion, Authorized: true, Executed: true,
		ExactSQLSHA256: strings.Repeat("5", 64), StrictASTSHA256: strings.Repeat("6", 64),
		RowLimit: 400, PolicyFingerprint: strings.Repeat("7", 64),
		PreparedTargetBindingSHA256: strings.Repeat("8", 64),
	}
	return &querybinding.QueryExecutionBindingV2{
		PathKind: querybinding.PathPairedNovel, Visible: visible, Companion: &companion,
	}
}

func TestProvSQLSelectorNamesPublicCellAndExactPrivateVariant(t *testing.T) {
	operation := experiment.AdapterOperation{
		ExperimentID: "provsql", WorkloadID: "nonce-join-group", Scale: "10k", Mode: "taskgate",
	}
	bindingKey := provSQLBindingKey(operation.Scale, 1000000042)
	selector := provSQLContractSelector(operation, bindingKey)
	if selector.ExperimentID != operation.ExperimentID || selector.WorkloadID != operation.WorkloadID ||
		selector.Scale != operation.Scale || selector.Mode != operation.Mode || selector.BindingKey != bindingKey {
		t.Fatalf("selector does not name the exact public/private ProvSQL operation: %+v", selector)
	}
}

func TestCarriedProvSQLEvidenceTranscribesBothExecutedTargets(t *testing.T) {
	registered := provSQLRegistrationForTest()
	window := experiment.ObserverWindowV2{
		Before: experiment.ObserverSnapshotV2{Phase: "before", Total: 7},
		After:  experiment.ObserverSnapshotV2{Phase: "after", Total: 31},
	}
	signed := signedProvSQLBinding()
	carried, err := carriedProvSQLEvidence(registered, window,
		queryreceipt.QueryReceiptV1{ExecutionBindingV2: signed})
	if err != nil {
		t.Fatalf("assemble ProvSQL carried evidence: %v", err)
	}
	if carried.Arm != experiment.ArmTaskGate || carried.Operation != registered.Operation ||
		carried.Plan.PathKind != experiment.PathPairedNovel || carried.Window.After.Total != window.After.Total ||
		carried.ClassifierManifestSHA256 != registered.ClassifierManifestSHA256 ||
		carried.ClassifierBindingSHA256 != registered.ClassifierBindingSHA256 {
		t.Fatal("ProvSQL evidence did not carry its registration and observer window unchanged")
	}
	if carried.VisibleStatement == nil || carried.CompanionStatement == nil ||
		carried.VisibleStatement.ExactSHA256 != signed.Visible.ExactSQLSHA256 ||
		carried.VisibleStatement.StrictASTSHA256 != signed.Visible.StrictASTSHA256 ||
		carried.VisibleStatement.RowLimit != signed.Visible.RowLimit ||
		carried.VisibleStatement.Fingerprint != signed.Visible.PolicyFingerprint ||
		carried.VisiblePreparedTargetBindingSHA256 != signed.Visible.PreparedTargetBindingSHA256 ||
		carried.CompanionStatement.ExactSHA256 != signed.Companion.ExactSQLSHA256 ||
		carried.CompanionStatement.StrictASTSHA256 != signed.Companion.StrictASTSHA256 ||
		carried.CompanionStatement.RowLimit != signed.Companion.RowLimit ||
		carried.CompanionStatement.Fingerprint != signed.Companion.PolicyFingerprint ||
		carried.CompanionPreparedTargetBindingSHA256 != signed.Companion.PreparedTargetBindingSHA256 {
		t.Fatal("ProvSQL evidence did not transcribe both signed targets exactly")
	}
}

func TestCarriedProvSQLEvidenceRefusesMissingOrNonexecutedPair(t *testing.T) {
	registered := provSQLRegistrationForTest()
	window := experiment.ObserverWindowV2{}
	if _, err := carriedProvSQLEvidence(registered, window, queryreceipt.QueryReceiptV1{}); err == nil {
		t.Fatal("ProvSQL evidence was assembled without a signed execution binding")
	}

	wrongRegistration := registered
	wrongRegistration.Operation.PathKind = experiment.PathSingleQuery
	if _, err := carriedProvSQLEvidence(wrongRegistration, window,
		queryreceipt.QueryReceiptV1{ExecutionBindingV2: signedProvSQLBinding()}); err == nil {
		t.Fatal("ProvSQL evidence accepted a non-paired operation registration")
	}
	wrongRegistration = registered
	wrongRegistration.Plan.PathKind = experiment.PathSingleQuery
	if _, err := carriedProvSQLEvidence(wrongRegistration, window,
		queryreceipt.QueryReceiptV1{ExecutionBindingV2: signedProvSQLBinding()}); err == nil {
		t.Fatal("ProvSQL evidence accepted a non-paired control plan")
	}

	wrongPath := signedProvSQLBinding()
	wrongPath.PathKind = querybinding.PathSingleQuery
	if _, err := carriedProvSQLEvidence(registered, window,
		queryreceipt.QueryReceiptV1{ExecutionBindingV2: wrongPath}); err == nil {
		t.Fatal("ProvSQL evidence accepted a receipt for another execution path")
	}

	missingCompanion := signedProvSQLBinding()
	missingCompanion.Companion = nil
	if _, err := carriedProvSQLEvidence(registered, window,
		queryreceipt.QueryReceiptV1{ExecutionBindingV2: missingCompanion}); err == nil {
		t.Fatal("ProvSQL evidence was assembled without a signed companion")
	}

	visibleNotExecuted := signedProvSQLBinding()
	visibleNotExecuted.Visible.Executed = false
	if _, err := carriedProvSQLEvidence(registered, window,
		queryreceipt.QueryReceiptV1{ExecutionBindingV2: visibleNotExecuted}); err == nil {
		t.Fatal("ProvSQL evidence presented an unexecuted visible target")
	}
	companionNotExecuted := signedProvSQLBinding()
	companionNotExecuted.Companion.Executed = false
	if _, err := carriedProvSQLEvidence(registered, window,
		queryreceipt.QueryReceiptV1{ExecutionBindingV2: companionNotExecuted}); err == nil {
		t.Fatal("ProvSQL evidence presented an unexecuted companion target")
	}
}
