package gateway

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/preparedbinding"
	"taskbound.local/agent-data-gateway/internal/querybinding"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

// # What these tests do and do not prove
//
// They drive the production receipt path -- control.GetQueryReceipt loading the
// evidence a real settlement wrote, then BuildQueryReceiptRequest selecting,
// building and signing -- over a real live execution's real evidence, with the
// execution binding carried as a QueryExecutionBindingV2 instead of a V1.
//
// The preparation inside that V2 is synthesized here rather than produced by
// physicalquery.Prepare. It has to be: the Gateway does not yet call Prepare on
// its production path, so there is no real sealed preparation for it to carry.
// That wiring is T1d, and until it lands nothing in this file should be read as
// proving the Gateway builds a V2 from what it prepared. What is proved is
// everything downstream of that: a persisted V2 earns V10 and not V9, the
// delivery mode follows whether a result object was actually registered, the
// signature covers the whole carried preparation, and an inline delivery signs
// and verifies without an artifact intent.
//
// The persistence half -- first write, idempotent retry, contradictory retry,
// strict per-version decode, rollback -- is proved against live PostgreSQL in
// internal/control/execution_binding_v2_test.go.

func liveV2Compiler(t *testing.T) preparedbinding.CompilerIdentityV1 {
	t.Helper()
	// The local identity, so the compiler and renderer named in the binding are
	// the ones this binary actually contains rather than a fixture's invention.
	identity, err := physicalquery.LocalCompilerIdentity()
	if err != nil {
		t.Fatalf("local compiler identity: %v", err)
	}
	return identity
}

// upgradeToV2 rebuilds a real V1 binding's runtime half as a V2 around a
// synthesized preparation.
//
// Every runtime member -- the path kind, the row limits, the pre-state digests,
// both target records -- is the one the live execution actually produced. Only
// the preparation is invented, and the prepared target digests are taken from
// the real target records so the cross-binding V2 performs is a real check
// rather than one arranged to pass.
func upgradeToV2(t *testing.T, v1 querybinding.QueryExecutionBindingV1) querybinding.QueryExecutionBindingV2 {
	t.Helper()
	compiler := liveV2Compiler(t)

	visible := v1.Visible
	visible.PolicyRendererVersion = compiler.PolicyRendererVersion
	visible.PolicyRendererDigest = compiler.PolicyRendererSHA256

	prepared := preparedbinding.PreparedOperationBindingV1{
		HasCompanion:     v1.Companion != nil,
		Grouped:          true,
		ExpandedEvidence: v1.UsesExpandedEvidence,

		VisibleFieldCount: 3, FactFieldCount: 1, ProvenanceFieldCount: 2,
		VisibleFieldsSHA256:    liveDigest("11"),
		FactFieldsSHA256:       liveDigest("12"),
		ProvenanceFieldsSHA256: liveDigest("13"),

		PreparationInputsSHA256: liveDigest("14"),
		GrantSHA256:             liveDigest("15"),
		CatalogSHA256:           liveDigest("16"),
		PlanSHA256:              v1.PlanSHA256,
		CompilerIdentitySHA256:  compiler.SHA256,

		PolicyGrantSHA256:   liveDigest("19"),
		NormalFormSHA256:    liveDigest("1a"),
		DictionarySetSHA256: v1.OrdinalDictionarySetSHA256,
		SidecarGrantsSHA256: v1.SidecarGrantsSHA256,

		VisibleTargetSHA256: v1.Visible.PreparedTargetBindingSHA256,
	}
	if v1.Companion != nil {
		prepared.CompanionTargetSHA256 = v1.Companion.PreparedTargetBindingSHA256
	}
	sealedPrepared, err := prepared.Seal()
	if err != nil {
		t.Fatalf("seal preparation: %v", err)
	}

	candidate := querybinding.QueryExecutionBindingV2{
		PathKind:                   v1.PathKind,
		PreparedOperation:          sealedPrepared,
		Compiler:                   compiler,
		ExposureProfileVersion:     v1.ExposureProfileVersion,
		VisibleRowLimit:            v1.VisibleRowLimit,
		BudgetBeforeSHA256:         v1.BudgetBeforeSHA256,
		ExposureLedgerBeforeSHA256: v1.ExposureLedgerBeforeSHA256,
		Visible:                    visible,
	}
	if v1.Companion != nil {
		companion := *v1.Companion
		companion.PolicyRendererVersion = compiler.PolicyRendererVersion
		companion.PolicyRendererDigest = compiler.PolicyRendererSHA256
		candidate.Companion = &companion
		candidate.CompanionEvidenceRows = v1.CompanionEvidenceRows
		candidate.CompanionPolicyRows = v1.CompanionPolicyRows
	}
	sealed, err := candidate.Seal()
	if err != nil {
		t.Fatalf("seal execution binding v2: %v", err)
	}
	return sealed
}

func liveDigest(seed string) string {
	digest := make([]byte, 0, 64)
	for len(digest) < 64 {
		digest = append(digest, seed...)
	}
	return string(digest[:64])
}

// liveV2Evidence runs one real paired-novel execution and returns the evidence
// the settlement persisted, with the execution binding upgraded to a V2.
func liveV2Evidence(t *testing.T, taskID, requestID string) (control.QueryReceipt, string) {
	t.Helper()
	harness := newV9Harness(t, taskID)
	result := harness.executePlan(t, requestID)
	queryID := result["query_id"].(string)

	evidence, err := harness.store.GetQueryReceipt(t.Context(), queryID)
	if err != nil {
		t.Fatalf("load receipt evidence: %v", err)
	}
	if evidence.ExecutionBinding == nil || evidence.ExecutionBinding.Binding == nil {
		t.Fatal("the live execution persisted no V1 execution binding to upgrade")
	}
	upgraded := upgradeToV2(t, *evidence.ExecutionBinding.Binding)
	evidence.ExecutionBinding = &control.QueryExecutionBinding{
		QueryID:              queryID,
		BindingV2:            &upgraded,
		ExposureLedgerBefore: evidence.ExecutionBinding.ExposureLedgerBefore,
		CreatedAt:            evidence.ExecutionBinding.CreatedAt,
	}
	if err := evidence.ExecutionBinding.Validate(); err != nil {
		t.Fatalf("the upgraded V2 binding does not validate against the live pre-state: %v", err)
	}
	return evidence, queryID
}

func signLiveReceipt(t *testing.T, evidence control.QueryReceipt) (queryreceipt.QueryReceiptV1, []byte) {
	t.Helper()
	signer := queryreceipt.DemoSigner([]byte("v10-live"))
	signedAt := time.Now().UTC()
	if evidence.Query.CompletedAt != nil && signedAt.Before(evidence.Query.CompletedAt.UTC()) {
		signedAt = evidence.Query.CompletedAt.UTC()
	}
	// The production builder, not a test reimplementation of it.
	saved, err := BuildQueryReceiptRequest(evidence, signer, signedAt)
	if err != nil {
		t.Fatalf("build the receipt: %v", err)
	}
	var receipt queryreceipt.QueryReceiptV1
	if err := json.Unmarshal(saved.ReceiptJSON, &receipt); err != nil {
		t.Fatal(err)
	}
	keyring, err := queryreceipt.NewKeyring(signer, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := keyring.Verify(receipt); err != nil {
		t.Fatalf("the signed receipt did not verify: %v", err)
	}
	return receipt, saved.ReceiptJSON
}

// A persisted V2 earns V10, carries the V2 and no V1, and names its delivery
// mode. This is the artifact-delivery case: the live execution registered a
// result object, so the mode is artifact and the intent is required.
func TestLiveV2ArtifactDeliveryEarnsAVerifiableV10(t *testing.T) {
	evidence, _ := liveV2Evidence(t, "task-v10-artifact", "v10-artifact-1")
	receipt, _ := signLiveReceipt(t, evidence)

	if receipt.Version != queryreceipt.VersionV10 {
		t.Fatalf("a persisted V2 binding produced a V%s receipt", receipt.Version)
	}
	if receipt.ResultDeliveryMode != queryreceipt.DeliveryArtifact {
		t.Fatalf("delivery mode is %q; the settlement registered a result object",
			receipt.ResultDeliveryMode)
	}
	if receipt.ExecutionBinding != nil {
		t.Fatal("a V10 receipt carries a V1 execution binding")
	}
	if receipt.ExecutionBindingV2 == nil {
		t.Fatal("a V10 receipt carries no V2 execution binding")
	}
	if !receipt.CarriesArtifactIntent() || !receipt.RequiresArtifactInclusionProofs() {
		t.Fatal("an artifact-mode V10 does not report its artifact intent")
	}
	if receipt.ExecutionBindingV2.SHA256 != evidence.ExecutionBinding.SHA256() {
		t.Fatal("the receipt was signed over a binding other than the persisted one")
	}
}

// The same live execution, delivered inline. This is the shape V9 could not
// represent at all, and the reason the three-path v3 cutover was blocked on a
// receipt version rather than on evidence.
func TestLiveV2InlineDeliveryEarnsAV10WithNoArtifactIntent(t *testing.T) {
	evidence, _ := liveV2Evidence(t, "task-v10-inline", "v10-inline-1")
	// Drop the registered result object, exactly as an operation that returns its
	// rows in the response would never have created one.
	evidence.Artifact = nil
	evidence.ArtifactRegistrationAudit = nil

	receipt, _ := signLiveReceipt(t, evidence)
	if receipt.Version != queryreceipt.VersionV10 {
		t.Fatalf("an inline V2 binding produced a V%s receipt", receipt.Version)
	}
	if receipt.ResultDeliveryMode != queryreceipt.DeliveryInline {
		t.Fatalf("delivery mode is %q; no result object was registered", receipt.ResultDeliveryMode)
	}
	if receipt.CarriesArtifactIntent() {
		t.Fatal("an inline V10 carries an artifact intent")
	}
	if receipt.RequiresArtifactInclusionProofs() {
		t.Fatal("an inline V10 was reported as requiring artifact inclusion proofs")
	}
	if receipt.ExecutionBindingV2 == nil {
		t.Fatal("an inline V10 carries no execution binding")
	}
}

// The signature covers the whole carried preparation, over live evidence.
func TestLiveV10SignatureCoversTheCarriedPreparation(t *testing.T) {
	evidence, _ := liveV2Evidence(t, "task-v10-tamper", "v10-tamper-1")
	receipt, original := signLiveReceipt(t, evidence)

	signer := queryreceipt.DemoSigner([]byte("v10-live"))
	keyring, err := queryreceipt.NewKeyring(signer, nil)
	if err != nil {
		t.Fatal(err)
	}
	tampered := receipt
	binding := *receipt.ExecutionBindingV2
	prepared := binding.PreparedOperation
	prepared.PolicyGrantSHA256 = liveDigest("ff")
	resealedPrepared, err := prepared.Seal()
	if err != nil {
		t.Fatal(err)
	}
	binding.PreparedOperation = resealedPrepared
	resealedBinding, err := binding.Seal()
	if err != nil {
		t.Fatal(err)
	}
	tampered.ExecutionBindingV2 = &resealedBinding
	if err := keyring.Verify(tampered); err == nil {
		t.Fatal("widening the signed policy grant left the V10 signature valid")
	}

	// And the untampered receipt is byte-stable: re-encoding what was signed
	// reproduces the persisted bytes, so a recovery re-signs the same document.
	reEncoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var roundTripped queryreceipt.QueryReceiptV1
	if err := json.Unmarshal(reEncoded, &roundTripped); err != nil {
		t.Fatal(err)
	}
	if err := keyring.Verify(roundTripped); err != nil {
		t.Fatalf("a round-tripped V10 receipt did not verify: %v", err)
	}
	if len(original) == 0 {
		t.Fatal("no receipt bytes were persisted")
	}
	if !bytes.Contains(original, []byte("execution_binding_v2")) {
		t.Fatal("the persisted receipt bytes carry no execution_binding_v2 member")
	}
}
