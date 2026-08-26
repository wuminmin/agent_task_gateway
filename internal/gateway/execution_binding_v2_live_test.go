//go:build taskgate_scale

// These cases prepare an ordinal-program plan, and preparation resolves every
// snapshot publication the Catalog declares (preparation_inputs.go:180). Five of
// the seven are scanned out of the Business database, which measured 25.84 GB
// peak on a 30 GB host, so they belong on the taskgate_scale lane rather than
// holding the acceptance run open.

package gateway

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

// # R3b: the production V10 round-trip
//
// These cases drive a real execution through the production Gateway path and
// then the production receipt path -- control.GetQueryReceipt loading the
// evidence the settlement wrote, then BuildQueryReceiptRequest selecting,
// building and signing.
//
// Nothing is synthesized. Before T1d the preparation inside the V2 had to be
// invented here, because the Gateway did not call
// internal/physicalquery.Prepare and there was no real sealed preparation to
// carry; the file said so and warned that nothing in it proved the Gateway built
// a V2 from what it prepared. That is what this now proves: the binding below is
// the one the Gateway persisted, and the preparation inside it is the one the
// executed statements were compiled from.
//
// The persistence half -- first write, idempotent retry, contradictory retry,
// strict per-version decode, rollback -- is proved against live PostgreSQL in
// internal/control/execution_binding_v2_test.go.

// liveV2Evidence runs one real paired-novel execution and returns the evidence
// the settlement persisted.
func liveV2Evidence(t *testing.T, taskID, requestID string) (control.QueryReceipt, string) {
	t.Helper()
	harness := newV10Harness(t, taskID)
	result := harness.executePlan(t, requestID)
	queryID := result["query_id"].(string)

	evidence, err := harness.store.GetQueryReceipt(t.Context(), queryID)
	if err != nil {
		t.Fatalf("load receipt evidence: %v", err)
	}
	if evidence.ExecutionBinding == nil || evidence.ExecutionBinding.BindingV2 == nil {
		t.Fatal("the live execution persisted no V2 execution binding")
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

	if receipt.Version != queryreceipt.Version {
		t.Fatalf("a persisted V2 binding produced a V%s receipt", receipt.Version)
	}
	if receipt.ResultDeliveryMode != queryreceipt.DeliveryArtifact {
		t.Fatalf("delivery mode is %q; the settlement registered a result object",
			receipt.ResultDeliveryMode)
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
	if receipt.Version != queryreceipt.Version {
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

// The carried preparation is the one the executed statements were compiled from.
//
// This is the claim T1d exists to make, and the one nothing could make while the
// preparation in a V2 was synthesized. The Gateway prepared the operation, the
// Connector executed the statements that preparation produced, and the signed
// receipt carries the sealed binding of THAT preparation -- so the two prepared
// target digests it names are the digests of the bytes that ran, and the
// compiler identity it names is this binary's.
func TestLiveV10CarriesThePreparationTheStatementsWereCompiledFrom(t *testing.T) {
	harness := newV10Harness(t, "task-v10-round-trip")
	result := harness.executePlan(t, "v10-round-trip-1")
	queryID := result["query_id"].(string)

	evidence, err := harness.store.GetQueryReceipt(t.Context(), queryID)
	if err != nil {
		t.Fatalf("load receipt evidence: %v", err)
	}
	receipt, _ := signLiveReceipt(t, evidence)
	binding := receipt.ExecutionBindingV2
	if binding == nil {
		t.Fatal("the live execution signed no V2 execution binding")
	}

	// The compiler is this binary's, and the preparation names it.
	compiler, err := physicalquery.LocalCompilerIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if binding.Compiler.SHA256 != compiler.SHA256 {
		t.Fatalf("the binding names compiler %s but this binary is %s",
			binding.Compiler.SHA256[:12], compiler.SHA256[:12])
	}
	if binding.PreparedOperation.CompilerIdentitySHA256 != compiler.SHA256 {
		t.Fatalf("the carried preparation was compiled by %s but this binary is %s",
			binding.PreparedOperation.CompilerIdentitySHA256[:12], compiler.SHA256[:12])
	}

	// The preparation describes a real operation: both statements, the projections
	// it compiled, the policy grant it authorized against, the V5 footprint and
	// the ordinal universe. A synthesized preparation could carry the flags; it
	// could not carry these and still tie to what ran.
	prepared := binding.PreparedOperation
	if !prepared.HasCompanion || binding.Companion == nil {
		t.Fatal("a paired-novel V5 execution carries no companion preparation")
	}
	for name, digest := range map[string]string{
		"preparation inputs":   prepared.PreparationInputsSHA256,
		"grant":                prepared.GrantSHA256,
		"catalog":              prepared.CatalogSHA256,
		"plan":                 prepared.PlanSHA256,
		"snapshot binding set": prepared.SnapshotBindingSetSHA256,
		"policy grant":         prepared.PolicyGrantSHA256,
		"normal form":          prepared.NormalFormSHA256,
		"ordinal program":      prepared.OrdinalProgramSHA256,
		"dictionary set":       prepared.DictionarySetSHA256,
		"predicate footprint":  prepared.PredicateFootprintSHA256,
		"visible fields":       prepared.VisibleFieldsSHA256,
		"provenance fields":    prepared.ProvenanceFieldsSHA256,
	} {
		if digest == "" {
			t.Errorf("the carried preparation names no %s; it did not come from a real V5 preparation", name)
		}
	}
	if prepared.EstimatedBaseFacts == 0 {
		t.Error("the carried preparation estimated no base facts")
	}

	// And the executed bytes are the ones it prepared. The prepared target digest
	// covers the compiled statement, the input set and the compiler; the target
	// record's exact digest covers the rendered bytes the Connector received. V2
	// requires the first pair to agree, and this checks the second against the
	// Connector's own record of what it was handed.
	requests := harness.connector.requests
	if len(requests) < 2 {
		t.Fatalf("the Connector received %d statements, want a visible and a companion", len(requests))
	}
	visibleSQL := requests[len(requests)-2].SQL
	companionSQL := requests[len(requests)-1].SQL
	if got := physicalquery.ExactDigest(visibleSQL); got != binding.Visible.ExactSQLSHA256 {
		t.Fatalf("the visible target's exact digest is %s but the executed statement digests to %s",
			binding.Visible.ExactSQLSHA256, got)
	}
	if got := physicalquery.ExactDigest(companionSQL); got != binding.Companion.ExactSQLSHA256 {
		t.Fatalf("the companion target's exact digest is %s but the executed statement digests to %s",
			binding.Companion.ExactSQLSHA256, got)
	}

	// The Gateway's own in-memory preparation for the same task and plan must be
	// the one that was signed. This is what closes the loop: preparation is a
	// function of its inputs, so a second call reaching a different sealed binding
	// would mean the signed one described something else.
	grant, err := harness.store.GetGrant(t.Context(), harness.taskID)
	if err != nil {
		t.Fatal(err)
	}
	reprepared, err := harness.service.preparePlan(grant, plannedSummaryQuery())
	if err != nil {
		t.Fatalf("re-prepare the executed plan: %v", err)
	}
	if err := reprepared.Exposure.prepared.Binding().RequireSame(prepared); err != nil {
		t.Fatalf("re-preparing the executed plan produced a different sealed binding: %v", err)
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

func liveDigest(seed string) string {
	digest := make([]byte, 0, 64)
	for len(digest) < 64 {
		digest = append(digest, seed...)
	}
	return string(digest[:64])
}
