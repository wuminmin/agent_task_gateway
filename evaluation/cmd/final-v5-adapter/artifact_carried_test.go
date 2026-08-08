package main

import (
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/finalv5contracts"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/querybinding"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

// signedArtifactBinding is a paired-novel execution binding with two target
// records whose every comparable member differs, so a transcription that
// crossed the visible and the companion -- or dropped a member -- cannot pass by
// accident.
func signedArtifactBinding() *querybinding.QueryExecutionBindingV2 {
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

// The Adapter transcribes the Gateway's signed targets exactly, and carries the
// prepared target binding beside each rather than folded into it.
//
// The distinction matters: a StatementIdentity describes a statement, and the
// prepared target binding describes its place in a compiled operation. Folding
// them together would lose the check that stops one target's statement being
// presented as the other's.
func TestCarriedArtifactEvidenceTranscribesTheSignedTargets(t *testing.T) {
	signed := signedArtifactBinding()
	receipt := queryreceipt.QueryReceiptV1{ExecutionBindingV2: signed}
	registered := experiment.PreRegisteredObservationV3{
		Operation: experiment.OperationIdentity{
			OperationID: "artifact/result-heavy/100x4/novel", PathKind: experiment.PathPairedNovel,
			ContractIdentity: "release:index:artifact/result-heavy/100x4/novel",
		},
		ClassifierManifestSHA256: strings.Repeat("a", 64),
		ClassifierBindingSHA256:  strings.Repeat("b", 64),
	}
	window := experiment.ObserverWindowV2{
		Before: experiment.ObserverSnapshotV2{Phase: "before", Total: 3},
		After:  experiment.ObserverSnapshotV2{Phase: "after", Total: 41},
	}

	carried, err := carriedArtifactEvidence(registered, window, receipt)
	if err != nil {
		t.Fatalf("assemble the carried evidence: %v", err)
	}
	if carried.Arm != experiment.ArmTaskGate {
		t.Fatalf("the carried evidence claims arm %q", carried.Arm)
	}
	if carried.Operation != registered.Operation ||
		carried.ClassifierManifestSHA256 != registered.ClassifierManifestSHA256 ||
		carried.ClassifierBindingSHA256 != registered.ClassifierBindingSHA256 {
		t.Fatal("the carried evidence does not carry the pre-registered classification back unchanged")
	}
	if carried.Window.After.Total != window.After.Total {
		t.Fatal("the carried evidence does not carry the observed window")
	}
	for _, role := range []struct {
		name     string
		signed   querybinding.TargetRecordV1
		carried  *experimentStatement
		prepared string
	}{
		{"visible", signed.Visible, statementFor(carried.VisibleStatement),
			carried.VisiblePreparedTargetBindingSHA256},
		{"companion", *signed.Companion, statementFor(carried.CompanionStatement),
			carried.CompanionPreparedTargetBindingSHA256},
	} {
		if role.carried == nil {
			t.Fatalf("no %s execution identity was carried", role.name)
		}
		if role.carried.exact != role.signed.ExactSQLSHA256 ||
			role.carried.strict != role.signed.StrictASTSHA256 ||
			role.carried.rowLimit != role.signed.RowLimit ||
			role.carried.fingerprint != role.signed.PolicyFingerprint {
			t.Errorf("the carried %s statement is %+v, the Gateway signed %+v",
				role.name, role.carried, role.signed)
		}
		if role.prepared != role.signed.PreparedTargetBindingSHA256 {
			t.Errorf("the carried %s prepared target binding is %q, the Gateway signed %q",
				role.name, role.prepared, role.signed.PreparedTargetBindingSHA256)
		}
	}
}

// experimentStatement flattens the carried identity so a mismatch reports which
// member moved rather than two opaque structs.
type experimentStatement struct {
	exact, strict, fingerprint string
	rowLimit                   int64
}

func statementFor(identity *physicalquery.StatementIdentity) *experimentStatement {
	if identity == nil {
		return nil
	}
	return &experimentStatement{
		exact: identity.ExactSHA256, strict: identity.StrictASTSHA256,
		rowLimit: identity.RowLimit, fingerprint: identity.Fingerprint,
	}
}

// A receipt that describes no execution, or a governed artifact query whose
// receipt signs no provenance companion, produces no evidence at all.
//
// Failing here rather than submitting partial evidence is the point: a carried
// evidence value missing a target reaches the finalizer as "this path executed a
// statement none was signed for", which is a true statement about a different
// defect and a much harder one to read.
func TestCarriedArtifactEvidenceRefusesAnIncompleteReceipt(t *testing.T) {
	registered := experiment.PreRegisteredObservationV3{}
	window := experiment.ObserverWindowV2{}
	if _, err := carriedArtifactEvidence(registered, window,
		queryreceipt.QueryReceiptV1{}); err == nil {
		t.Fatal("evidence was assembled for a receipt that describes no execution")
	}
	noCompanion := signedArtifactBinding()
	noCompanion.Companion = nil
	if _, err := carriedArtifactEvidence(registered, window,
		queryreceipt.QueryReceiptV1{ExecutionBindingV2: noCompanion}); err == nil {
		t.Fatal("evidence was assembled for a paired artifact query signing no companion")
	}
}

// The selector names the cell exactly, in all four coordinates.
//
// Pre-registration requires the selector to admit exactly one frozen operation,
// because there is no receipt yet to identify one by. A selector that left a
// coordinate blank would admit the whole scale sweep and pre-register nothing.
func TestTheArtifactSelectorNamesEveryCoordinate(t *testing.T) {
	cell := finalv5contracts.CellIdentity{
		ExperimentID: finalv5contracts.ArtifactExperimentID,
		WorkloadID:   finalv5contracts.ArtifactWorkloadID, Scale: "100x4", Mode: "novel",
	}
	selector := artifactContractSelector(cell)
	if selector.ExperimentID != cell.ExperimentID || selector.WorkloadID != cell.WorkloadID ||
		selector.Scale != cell.Scale || selector.Mode != cell.Mode {
		t.Fatalf("the selector %s does not name the cell %s", selector, cell)
	}
}
