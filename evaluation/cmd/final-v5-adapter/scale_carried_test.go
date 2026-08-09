package main

import (
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/internal/querybinding"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

func signedScaleBinding(path querybinding.PathKind, executed bool) *querybinding.QueryExecutionBindingV2 {
	visible := querybinding.TargetRecordV1{
		Role: querybinding.RoleVisible, Authorized: true, Executed: executed,
		ExactSQLSHA256: strings.Repeat("1", 64), StrictASTSHA256: strings.Repeat("2", 64),
		RowLimit: 100, PolicyFingerprint: strings.Repeat("3", 64),
		PreparedTargetBindingSHA256: strings.Repeat("4", 64),
	}
	companion := querybinding.TargetRecordV1{
		Role: querybinding.RoleCompanion, Authorized: true, Executed: executed,
		ExactSQLSHA256: strings.Repeat("5", 64), StrictASTSHA256: strings.Repeat("6", 64),
		RowLimit: 400, PolicyFingerprint: strings.Repeat("7", 64),
		PreparedTargetBindingSHA256: strings.Repeat("8", 64),
	}
	return &querybinding.QueryExecutionBindingV2{PathKind: path, Visible: visible, Companion: &companion}
}

func scaleRegistrationForTest(path experiment.GatewayPathKind) experiment.PreRegisteredObservationV3 {
	return experiment.PreRegisteredObservationV3{
		Operation: experiment.OperationIdentity{
			OperationID: "scale/dependency-e2e/10k-overlap-0/" + string(path),
			PathKind:    path, ContractIdentity: "release:index:scale/dependency-e2e/10k-overlap-0/" + string(path),
		},
		ClassifierManifestSHA256: strings.Repeat("a", 64),
		ClassifierBindingSHA256:  strings.Repeat("b", 64),
	}
}

func TestScaleSelectorNamesEveryFrozenCoordinate(t *testing.T) {
	operation := experiment.AdapterOperation{
		ExperimentID: "scale", WorkloadID: "dependency-e2e",
		Scale: "10k-overlap-50", Mode: "semantic_replay",
	}
	selector := scaleContractSelector(operation)
	if selector.ExperimentID != operation.ExperimentID || selector.WorkloadID != operation.WorkloadID ||
		selector.Scale != operation.Scale || selector.Mode != operation.Mode {
		t.Fatalf("the selector %s does not name the exact Scale cell", selector)
	}
}

func TestCarriedScaleNovelEvidenceTranscribesBothExecutedTargets(t *testing.T) {
	registered := scaleRegistrationForTest(experiment.PathPairedNovel)
	window := experiment.ObserverWindowV2{
		Before: experiment.ObserverSnapshotV2{Phase: "before", Total: 7},
		After:  experiment.ObserverSnapshotV2{Phase: "after", Total: 31},
	}
	signed := signedScaleBinding(querybinding.PathPairedNovel, true)
	carried, err := carriedScaleEvidence("novel", registered, window,
		queryreceipt.QueryReceiptV1{ExecutionBindingV2: signed})
	if err != nil {
		t.Fatalf("assemble novel evidence: %v", err)
	}
	if carried.Arm != experiment.ArmTaskGate || carried.Operation != registered.Operation ||
		carried.ClassifierManifestSHA256 != registered.ClassifierManifestSHA256 ||
		carried.ClassifierBindingSHA256 != registered.ClassifierBindingSHA256 ||
		carried.Window.After.Total != window.After.Total {
		t.Fatal("novel evidence did not carry the pre-registration and retained window unchanged")
	}
	if carried.VisibleStatement == nil || carried.CompanionStatement == nil {
		t.Fatal("novel evidence omitted an executed target")
	}
	if carried.VisibleStatement.ExactSHA256 != signed.Visible.ExactSQLSHA256 ||
		carried.VisibleStatement.StrictASTSHA256 != signed.Visible.StrictASTSHA256 ||
		carried.VisibleStatement.RowLimit != signed.Visible.RowLimit ||
		carried.VisibleStatement.Fingerprint != signed.Visible.PolicyFingerprint ||
		carried.VisiblePreparedTargetBindingSHA256 != signed.Visible.PreparedTargetBindingSHA256 {
		t.Fatal("novel evidence did not transcribe the signed visible target exactly")
	}
	if carried.CompanionStatement.ExactSHA256 != signed.Companion.ExactSQLSHA256 ||
		carried.CompanionStatement.StrictASTSHA256 != signed.Companion.StrictASTSHA256 ||
		carried.CompanionStatement.RowLimit != signed.Companion.RowLimit ||
		carried.CompanionStatement.Fingerprint != signed.Companion.PolicyFingerprint ||
		carried.CompanionPreparedTargetBindingSHA256 != signed.Companion.PreparedTargetBindingSHA256 {
		t.Fatal("novel evidence did not transcribe the signed companion target exactly")
	}
}

func TestCarriedScaleSemanticReplayCarriesNoExecutionTargets(t *testing.T) {
	registered := scaleRegistrationForTest(experiment.PathSemanticReplay)
	signed := signedScaleBinding(querybinding.PathSemanticReplay, false)
	carried, err := carriedScaleEvidence("semantic_replay", registered, experiment.ObserverWindowV2{},
		queryreceipt.QueryReceiptV1{ExecutionBindingV2: signed})
	if err != nil {
		t.Fatalf("assemble semantic-replay evidence: %v", err)
	}
	if carried.VisibleStatement != nil || carried.CompanionStatement != nil ||
		carried.VisiblePreparedTargetBindingSHA256 != "" ||
		carried.CompanionPreparedTargetBindingSHA256 != "" {
		t.Fatalf("semantic replay presented authorized-only targets as current execution evidence: %+v", carried)
	}
	if carried.Operation != registered.Operation || carried.Arm != experiment.ArmTaskGate {
		t.Fatal("semantic replay omitted its pre-registered operation or TaskGate arm")
	}
}

func TestCarriedScaleEvidenceRefusesMissingOrUnsupportedExecution(t *testing.T) {
	registered := scaleRegistrationForTest(experiment.PathPairedNovel)
	if _, err := carriedScaleEvidence("novel", registered, experiment.ObserverWindowV2{},
		queryreceipt.QueryReceiptV1{}); err == nil {
		t.Fatal("evidence was assembled without a signed execution binding")
	}
	missingCompanion := signedScaleBinding(querybinding.PathPairedNovel, true)
	missingCompanion.Companion = nil
	if _, err := carriedScaleEvidence("novel", registered, experiment.ObserverWindowV2{},
		queryreceipt.QueryReceiptV1{ExecutionBindingV2: missingCompanion}); err == nil {
		t.Fatal("novel evidence was assembled without a signed companion")
	}
	notExecuted := signedScaleBinding(querybinding.PathPairedNovel, false)
	if _, err := carriedScaleEvidence("novel", registered, experiment.ObserverWindowV2{},
		queryreceipt.QueryReceiptV1{ExecutionBindingV2: notExecuted}); err == nil {
		t.Fatal("novel evidence carried targets the receipt did not mark executed")
	}
	semanticExecuted := signedScaleBinding(querybinding.PathSemanticReplay, true)
	if _, err := carriedScaleEvidence("semantic_replay", registered, experiment.ObserverWindowV2{},
		queryreceipt.QueryReceiptV1{ExecutionBindingV2: semanticExecuted}); err == nil {
		t.Fatal("semantic-replay evidence accepted a target marked executed")
	}
	semanticMissingCompanion := signedScaleBinding(querybinding.PathSemanticReplay, false)
	semanticMissingCompanion.Companion = nil
	if _, err := carriedScaleEvidence("semantic_replay", registered, experiment.ObserverWindowV2{},
		queryreceipt.QueryReceiptV1{ExecutionBindingV2: semanticMissingCompanion}); err == nil {
		t.Fatal("semantic-replay evidence was assembled without both authorized targets")
	}
	if _, err := carriedScaleEvidence("idempotent_replay", registered, experiment.ObserverWindowV2{},
		queryreceipt.QueryReceiptV1{ExecutionBindingV2: signedScaleBinding(querybinding.PathPairedNovel, true)}); err == nil {
		t.Fatal("an unsupported Scale mode assembled carried evidence")
	}
}
