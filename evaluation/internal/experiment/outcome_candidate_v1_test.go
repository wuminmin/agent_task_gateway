package experiment

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/catalogschema"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/preparedbinding"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

func outcomeCandidateTestDigest(index int) string {
	return fmt.Sprintf("%064x", index)
}

func outcomeCandidateTestExpectation(t *testing.T, members ...string) OutcomeCandidateExpectationV1 {
	t.Helper()
	expectation, err := summarizeOutcomeCandidateMembers(members)
	if err != nil {
		t.Fatalf("summarize test Outcome candidate: %v", err)
	}
	if err := expectation.Validate(); err != nil {
		t.Fatalf("test Outcome candidate does not validate: %v", err)
	}
	return expectation
}

func outcomeCandidateTestExposure(atomCount int64, composite string) *queryreceipt.ExposureEvidenceV1 {
	return &queryreceipt.ExposureEvidenceV1{
		ActualPredicateAtomCount: atomCount,
		ActualOutcomeFacts:       atomCount + 1,
		ActualCompositeCount:     1,
		CompositeOutcomeSHA256:   composite,
		// These are coherent production-side identities, but none is an
		// ordinary semantic-set digest and none may be used as the oracle.
		PredicateContextSHA256: outcomeCandidateTestDigest(900),
		PredicateSetSHA256:     outcomeCandidateTestDigest(901),
		OutcomeSetSHA256:       outcomeCandidateTestDigest(902),
	}
}

func outcomeCandidateTestReproduction(atoms []string) ReproducedExecutionV3 {
	return ReproducedExecutionV3{
		PreparedPredicateAtomSHA256:    append([]string(nil), atoms...),
		PreparedPredicateContextSHA256: outcomeCandidateTestDigest(900),
		PreparedPredicateSetSHA256:     outcomeCandidateTestDigest(901),
	}
}

func outcomeCandidateScaleMaterial(t *testing.T) FrozenOperationMaterialV3 {
	t.Helper()
	reviewDir, err := filepath.Abs(filepath.Join("..", "..", "final-v5-wsl2",
		"publication-review", "exposure-scale-v1"))
	if err != nil {
		t.Fatalf("resolve exposure-scale review directory: %v", err)
	}
	artifactRoot := t.TempDir()
	publicationDir := filepath.Join(artifactRoot, "final-v5-exposure-scale-v1")
	if err := os.Mkdir(publicationDir, 0o700); err != nil {
		t.Fatalf("make test publication directory: %v", err)
	}
	bundleName := "final-v5-exposure-scale-v1.bundle.json"
	if err := os.Link(filepath.Join(reviewDir, bundleName), filepath.Join(publicationDir, bundleName)); err != nil {
		t.Fatalf("link retained review bundle into registry layout: %v", err)
	}
	return FrozenOperationMaterialV3{
		CatalogPath:         filepath.Join(reviewDir, "catalog.yaml"),
		SnapshotArtifactDir: artifactRoot,
		Plan: queryplan.QueryPlan{
			Product: "final_v5_exposure_scale",
			Aggregates: []queryplan.Aggregate{{
				Function: "count", Column: "*", Alias: "member_count",
			}},
			Filters: []queryplan.Filter{
				{Column: "partition_key", Op: "=", Value: json.Number("1")},
				{Column: "family_id", Op: "=", Value: json.Number("1")},
				{Column: "member_rank", Op: "<=", Value: json.Number("2000")},
				{Column: "metric", Op: "<=", Value: json.Number("1001.00")},
			},
		},
		Grant: physicalquery.Grant{
			ApprovedProducts: []string{"final_v5_exposure_scale"},
			ApprovedColumns: map[string][]string{
				"final_v5_exposure_scale": {"member_rank", "metric", "family_id", "partition_key"},
			},
			MandatoryScope:  []byte(`{"partition_key":["1"]}`),
			ExposureProfile: "taskgate-exposure-v5",
			PredicateLimits: queryplan.PredicateLimits{
				MaxRawLiteralsPerQuery: 64, MaxUniqueAtomsPerQuery: 16,
				MaxAtomPayloadBytes: 4096, MaxTotalAtomPayloadBytes: 65536,
			},
		},
	}
}

func outcomeCandidateScaleFootprint(t *testing.T, catalogPath string) AttestationFootprintV2 {
	t.Helper()
	logical, err := catalog.Load(catalogPath)
	if err != nil {
		t.Fatalf("load exposure-scale Catalog: %v", err)
	}
	built, err := catalogschema.Build(logical)
	if err != nil {
		t.Fatalf("build exposure-scale ExpectedSchema: %v", err)
	}
	footprint, err := NewAttestationFootprintV2(built.Digest, built.Count,
		RequiredMeasurementEnvironment(), testRuntimeIdentity(), "outcome-candidate-integration-test",
		map[AttestationScope][]AttestationInternalEntry{
			AttestationScopeConstructorOrColdPool:  {{StrictASTSHA256: testInternalKeyA, CallsPerAttestation: 1}},
			AttestationScopeExplicitPreflightPool:  {{StrictASTSHA256: testInternalKeyA, CallsPerAttestation: 1}},
			AttestationScopeSingleQueryTransaction: {{StrictASTSHA256: testInternalKeyA, CallsPerAttestation: 1}},
			AttestationScopePairedQueryTransaction: {{StrictASTSHA256: testInternalKeyA, CallsPerAttestation: 1}},
		})
	if err != nil {
		t.Fatalf("qualify exposure-scale footprint: %v", err)
	}
	return footprint
}

func TestOutcomeCandidateVerificationRetainsExactFiveMemberAgreement(t *testing.T) {
	atoms := []string{
		outcomeCandidateTestDigest(1), outcomeCandidateTestDigest(2),
		outcomeCandidateTestDigest(3), outcomeCandidateTestDigest(4),
	}
	composite := outcomeCandidateTestDigest(5)
	expected := outcomeCandidateTestExpectation(t, append(append([]string(nil), atoms...), composite)...)

	verification, err := verifyOutcomeCandidateV1(expected, outcomeCandidateTestReproduction(atoms),
		outcomeCandidateTestExposure(int64(len(atoms)), composite))
	if err != nil {
		t.Fatalf("exact five-member candidate was refused: %v", err)
	}
	if err := verification.Validate(); err != nil {
		t.Fatalf("retained finalizer verification does not validate: %v", err)
	}
	if verification.Expected.Cardinality != 5 || len(verification.Expected.Members) != 5 ||
		verification.Expected.OrdinarySetSHA256 != verification.Observed.OrdinarySetSHA256 {
		t.Fatalf("verification did not retain exact member agreement: %+v", verification)
	}

	// The production radix identity is not an ordinary-set operand. Changing it
	// while leaving the exact members alone cannot change this comparison.
	otherRadix := outcomeCandidateTestExposure(int64(len(atoms)), composite)
	otherRadix.OutcomeSetSHA256 = outcomeCandidateTestDigest(999)
	second, err := verifyOutcomeCandidateV1(expected, outcomeCandidateTestReproduction(atoms), otherRadix)
	if err != nil {
		t.Fatalf("radix-only mutation affected ordinary-set verification: %v", err)
	}
	if second.Observed.OrdinarySetSHA256 != verification.Observed.OrdinarySetSHA256 {
		t.Fatal("radix identity leaked into the ordinary-set reconstruction")
	}
}

func TestOutcomeCandidateVerificationRejectsEachMemberLevelMutation(t *testing.T) {
	honestAtoms := []string{
		outcomeCandidateTestDigest(1), outcomeCandidateTestDigest(2),
		outcomeCandidateTestDigest(3), outcomeCandidateTestDigest(4),
	}
	honestComposite := outcomeCandidateTestDigest(5)
	expected := outcomeCandidateTestExpectation(t,
		append(append([]string(nil), honestAtoms...), honestComposite)...)

	tests := []struct {
		name                  string
		atoms                 []string
		composite             string
		synchronizedCountMove bool
	}{
		// Both signed counts move with the missing/extra atom. The receipt-side
		// equation actualOutcome=atomCount+1 remains true in either case.
		{"missing atom with synchronized counts", honestAtoms[:3], honestComposite, true},
		{"extra atom with synchronized counts", append(append([]string(nil), honestAtoms...), outcomeCandidateTestDigest(6)), honestComposite, true},
		// A changed field, type, operator, literal or stable role changes that
		// atom's Fact identity while retaining the same cardinality.
		{"one atom semantic identity changed", []string{outcomeCandidateTestDigest(10), honestAtoms[1], honestAtoms[2], honestAtoms[3]}, honestComposite, false},
		// Catalog, publication and scope are predicate-context inputs, so a wrong
		// binding consistently changes every atom Fact identity.
		{"predicate context binding changed", []string{outcomeCandidateTestDigest(11), outcomeCandidateTestDigest(12), outcomeCandidateTestDigest(13), outcomeCandidateTestDigest(14)}, honestComposite, false},
		// Normal form and result observation are composite inputs, so either
		// mutation changes the composite member.
		{"composite identity changed", honestAtoms, outcomeCandidateTestDigest(15), false},
		// This is the crucial formerly-undetected case: all five members and all
		// receipt counts/aggregate identities can be mutually coherent yet still
		// describe the wrong ordinary candidate set.
		{"entire coherent five-member set changed", []string{outcomeCandidateTestDigest(21), outcomeCandidateTestDigest(22), outcomeCandidateTestDigest(23), outcomeCandidateTestDigest(24)}, outcomeCandidateTestDigest(25), false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			exposure := outcomeCandidateTestExposure(int64(len(test.atoms)), test.composite)
			// The former Adapter view moves with the signed receipt. Missing and
			// extra atom cases therefore remain internally count-consistent on both
			// wires instead of being rejected by stale Sample arithmetic.
			sample := Sample{
				ActualOutcomeFacts: exposure.ActualOutcomeFacts,
				PredicateAtomCount: exposure.ActualPredicateAtomCount,
				CompositeCount:     exposure.ActualCompositeCount,
			}
			if sample.ActualOutcomeFacts != sample.PredicateAtomCount+sample.CompositeCount ||
				sample.ActualOutcomeFacts != exposure.ActualOutcomeFacts ||
				sample.PredicateAtomCount != exposure.ActualPredicateAtomCount {
				t.Fatal("receipt and Sample count mutations are not synchronized")
			}
			if test.synchronizedCountMove && sample.ActualOutcomeFacts == outcomeCandidateMemberCardinalityV1 {
				t.Fatal("missing/extra atom case did not move the receipt and Sample cardinalities")
			}
			_, err := verifyOutcomeCandidateV1(expected, outcomeCandidateTestReproduction(test.atoms), exposure)
			if err == nil {
				t.Fatal("member-level mutation passed strict Outcome verification")
			}
			assertTaskGateRejectionGate(t, err, rejectionGateFrozenMaterial)
		})
	}
}

func TestOutcomeCandidateExpectationRejectsAggregateOnlyAndNonCanonicalBindings(t *testing.T) {
	members := []string{
		outcomeCandidateTestDigest(1), outcomeCandidateTestDigest(2),
		outcomeCandidateTestDigest(3), outcomeCandidateTestDigest(4),
		outcomeCandidateTestDigest(5),
	}
	honest := outcomeCandidateTestExpectation(t, members...)
	tests := map[string]OutcomeCandidateExpectationV1{
		"aggregate renamed without exact members": {
			Cardinality: 5, OrdinarySetSHA256: honest.OrdinarySetSHA256,
		},
		"ordinary digest does not derive from members": func() OutcomeCandidateExpectationV1 {
			value := honest
			value.OrdinarySetSHA256 = outcomeCandidateTestDigest(100)
			return value
		}(),
		"members are not canonical": func() OutcomeCandidateExpectationV1 {
			value := honest
			value.Members = append([]string(nil), honest.Members...)
			value.Members[0], value.Members[1] = value.Members[1], value.Members[0]
			return value
		}(),
		"duplicate member collapses the set": func() OutcomeCandidateExpectationV1 {
			value := honest
			value.Members = append([]string(nil), honest.Members...)
			value.Members[1] = value.Members[0]
			return value
		}(),
	}
	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			if err := value.Validate(); err == nil {
				t.Fatal("invalid frozen Outcome candidate was accepted")
			}
		})
	}
}

func TestReproductionRetainsPreparedPredicateAtomMembers(t *testing.T) {
	material := outcomeCandidateScaleMaterial(t)
	receipt := gatewayReceiptFor(t, material)
	reproduced, err := ReproduceExecutionV3(receipt, material)
	if err != nil {
		t.Fatalf("reproduce execution: %v", err)
	}
	if len(reproduced.PreparedPredicateAtomSHA256) != 4 {
		t.Fatalf("Scale V5 reproduction retained %d predicate atom members, want 4",
			len(reproduced.PreparedPredicateAtomSHA256))
	}
	for index, digest := range reproduced.PreparedPredicateAtomSHA256 {
		if !validSHA256(digest) {
			t.Fatalf("prepared predicate atom %d is not lowercase SHA-256: %q", index+1, digest)
		}
		if index > 0 && reproduced.PreparedPredicateAtomSHA256[index-1] >= digest {
			t.Fatal("prepared predicate atom identities are not canonical and unique")
		}
	}
}

func TestOutcomeCandidateDomainLinkAcceptsCatalogEncodingOnlyAndRejectsRealMemberDifference(t *testing.T) {
	material := outcomeCandidateScaleMaterial(t)
	frozenCatalogSHA256 := outcomeCandidateTestDigest(777)
	oracle, err := finalv5oracle.GenerateExposureScaleOutcomeCandidate(
		finalv5oracle.ExposureScaleOutcomeRequest{
			CatalogSHA256: frozenCatalogSHA256, CandidateFacts: finalv5oracle.DependencyScale10K,
			SetOptions: finalv5oracle.StreamSetOptions{MaxInMemoryMembers: 2_048, CaptureMembers: 5},
		})
	if err != nil {
		t.Fatalf("generate complete-Catalog oracle: %v", err)
	}
	expected := OutcomeCandidateExpectationV1{Cardinality: oracle.CandidateCardinality,
		Members: oracle.Members, OrdinarySetSHA256: oracle.CandidateSetSHA256}
	linker := newOutcomeCandidateDomainLinkerV1()

	receipt := gatewayReceiptFor(t, material)
	reproduced, err := ReproduceExecutionV3(receipt, material)
	if err != nil {
		t.Fatalf("reproduce activated-profile execution: %v", err)
	}
	logicalCatalog, err := catalog.Load(material.CatalogPath)
	if err != nil {
		t.Fatalf("load activated profile Catalog: %v", err)
	}
	profileOracle, err := finalv5oracle.GenerateExposureScaleOutcomeCandidate(
		finalv5oracle.ExposureScaleOutcomeRequest{
			CatalogSHA256: logicalCatalog.SHA256, CandidateFacts: finalv5oracle.DependencyScale10K,
			SetOptions: finalv5oracle.StreamSetOptions{MaxInMemoryMembers: 2_048, CaptureMembers: 5},
		})
	if err != nil {
		t.Fatalf("generate profile-Catalog oracle: %v", err)
	}
	receipt.Exposure.ActualPredicateAtomCount = 4
	receipt.Exposure.ActualCompositeCount = 1
	receipt.Exposure.ActualOutcomeFacts = 5
	receipt.Exposure.PredicateContextSHA256 = reproduced.PreparedPredicateContextSHA256
	receipt.Exposure.PredicateSetSHA256 = reproduced.PreparedPredicateSetSHA256
	receipt.Exposure.CompositeOutcomeSHA256 = profileOracle.Composite.SHA256
	verification, err := verifyOutcomeCandidateV2(linker, expected, frozenCatalogSHA256,
		finalv5oracle.DependencyScale10K, material.CatalogPath, reproduced, receipt.Exposure)
	if err != nil {
		t.Fatalf("Catalog-only encoding difference was rejected: %v", err)
	}
	if verification.DomainLink == nil ||
		verification.Expected.OrdinarySetSHA256 == verification.Observed.OrdinarySetSHA256 ||
		verification.DomainLink.LinkedExpected.OrdinarySetSHA256 != verification.Observed.OrdinarySetSHA256 {
		t.Fatalf("domain link did not retain distinct frozen/profile encodings: %+v", verification)
	}
	if err := verification.Validate(); err != nil {
		t.Fatalf("linked verification does not validate: %v", err)
	}

	wrong := verification
	wrong.Observed.Members = append([]string(nil), verification.Observed.Members...)
	wrong.Observed.Members[0] = outcomeCandidateTestDigest(999)
	wrong.Observed, err = summarizeOutcomeCandidateMembers(wrong.Observed.Members)
	if err != nil {
		t.Fatalf("summarize wrong member set: %v", err)
	}
	if err := wrong.Validate(); err == nil {
		t.Fatal("a real predicate-member difference passed the Catalog-domain linker")
	}
}

// The formerly-undetected failure is not a malformed receipt. This test makes
// the production-side story coherent end to end -- a real preparation with
// four atoms, a production composite, a production radix set, synchronized
// signed counts, and the same values copied into a Sample -- then judges those
// five actual members against a different valid frozen ordinary-set oracle.
// Only the new member-level finalizer check rejects it.
func TestFinalizerRejectsAnEntirelyWrongButInternallyCoherentFiveMemberOutcomeSet(t *testing.T) {
	// Freeze the expected side first from the evaluation-only contract model.
	// Nothing produced below by preparation, receipt construction or radix
	// storage is admitted as an expected operand.
	honestMaterial := outcomeCandidateScaleMaterial(t)
	logicalCatalog, err := catalog.Load(honestMaterial.CatalogPath)
	if err != nil {
		t.Fatalf("load reviewed Scale Catalog: %v", err)
	}
	oracle, err := finalv5oracle.GenerateExposureScaleOutcomeCandidate(
		finalv5oracle.ExposureScaleOutcomeRequest{
			CatalogSHA256: logicalCatalog.SHA256, CandidateFacts: finalv5oracle.DependencyScale10K,
			SetOptions: finalv5oracle.StreamSetOptions{MaxInMemoryMembers: 2_048, CaptureMembers: 5},
		})
	if err != nil {
		t.Fatalf("generate independent Scale Outcome oracle: %v", err)
	}
	expected := OutcomeCandidateExpectationV1{
		Cardinality: oracle.CandidateCardinality, Members: append([]string(nil), oracle.Members...),
		OrdinarySetSHA256: oracle.CandidateSetSHA256,
	}
	if err := expected.Validate(); err != nil {
		t.Fatalf("independent Scale Outcome oracle is invalid: %v", err)
	}

	// The actual side consistently runs a wrong mandatory scope. That context
	// change moves every predicate atom; the resulting predicate set/context and
	// composite are then used consistently by preparation, receipt, Sample and
	// production radix storage.
	material := honestMaterial
	material.Grant.MandatoryScope = []byte(`{"partition_key":["2"]}`)
	receipt := gatewayReceiptFor(t, material)
	reproduced, err := ReproduceExecutionV3(receipt, material)
	if err != nil {
		t.Fatalf("reproduce Gateway-built Scale execution: %v", err)
	}
	if len(reproduced.PreparedPredicateAtomSHA256) != 4 {
		t.Fatalf("real Scale preparation has %d atoms, want 4", len(reproduced.PreparedPredicateAtomSHA256))
	}

	_, emptyWitness, err := finalv5oracle.SummarizeUnitWitnessSemanticSetRoles([]string{"candidate"},
		func(func(string) error) error { return nil }, finalv5oracle.StreamSetOptions{MaxInMemoryMembers: 5})
	if err != nil {
		t.Fatalf("summarize the wrong scope's empty dependency witness: %v", err)
	}
	outputRowKey, err := exposure.ComposeCanonicalKeyV2("group-row", "global")
	if err != nil {
		t.Fatalf("build aggregate output key: %v", err)
	}
	releaseFact, err := exposure.NewDerivedFactV2([]exposure.SnapshotBinding{{
		SourceNamespace: finalv5oracle.ExposureScaleSourceNamespace,
		Snapshot:        finalv5oracle.ExposureScaleSnapshot,
	}}, outputRowKey, "count(*)", "bigint", int64(0), emptyWitness)
	if err != nil {
		t.Fatalf("build the wrong scope's production release Fact: %v", err)
	}
	resultObservationSHA256, err := exposure.ReleaseOutcomeDigest([]exposure.FactID{releaseFact}, 1)
	if err != nil {
		t.Fatalf("build the wrong scope's production result observation: %v", err)
	}
	compositeFact, err := exposure.NewCompositeOutcomeFactV5(exposure.CompositeOutcomeFactV5{
		QueryNormalFormVersion:  queryplan.NormalFormVersionV4,
		QueryNormalFormSHA256:   reproduced.Prepared.NormalFormSHA256,
		ResultObservationSHA256: resultObservationSHA256,
		VisibleRows:             1,
		PredicateContextSHA256:  reproduced.PreparedPredicateContextSHA256,
		PredicateSetSHA256:      reproduced.PreparedPredicateSetSHA256,
		PredicateAtomCount:      int64(len(reproduced.PreparedPredicateAtomSHA256)),
	})
	if err != nil {
		t.Fatalf("build coherent production composite: %v", err)
	}
	compositeSHA256, err := compositeFact.Hash()
	if err != nil {
		t.Fatalf("hash coherent production composite: %v", err)
	}
	actualMembers := append([]string(nil), reproduced.PreparedPredicateAtomSHA256...)
	actualMembers = append(actualMembers, compositeSHA256)
	expectedMembers := make(map[string]struct{}, len(expected.Members))
	for _, member := range expected.Members {
		expectedMembers[member] = struct{}{}
	}
	for _, member := range actualMembers {
		if _, retained := expectedMembers[member]; retained {
			t.Fatalf("wrong production set did not change all five members; %s remained", member)
		}
	}
	radix, err := control.BuildOutcomeHashSetV5(actualMembers)
	if err != nil {
		t.Fatalf("build coherent production radix set: %v", err)
	}

	receipt.Exposure.ActualPredicateAtomCount = 4
	receipt.Exposure.ChargedPredicateAtomCount = 4
	receipt.Exposure.ActualCompositeCount = 1
	receipt.Exposure.ChargedCompositeCount = 1
	receipt.Exposure.ActualOutcomeFacts = 5
	receipt.Exposure.ChargedOutcomeFacts = 5
	receipt.Exposure.PredicateContextSHA256 = reproduced.PreparedPredicateContextSHA256
	receipt.Exposure.PredicateSetSHA256 = reproduced.PreparedPredicateSetSHA256
	receipt.Exposure.CompositeOutcomeSHA256 = compositeSHA256
	receipt.Exposure.OutcomeSetSHA256 = radix.Set.SetSHA256

	// This is the Adapter's old internally coherent view. Every Outcome member
	// it could copy from the receipt agrees, including the production radix set;
	// it still has no authority over the frozen ordinary-set oracle below.
	sample := Sample{
		OutcomeSetSHA256:    receipt.Exposure.OutcomeSetSHA256,
		ActualOutcomeFacts:  receipt.Exposure.ActualOutcomeFacts,
		ChargedOutcomeFacts: receipt.Exposure.ChargedOutcomeFacts,
		PredicateAtomCount:  receipt.Exposure.ActualPredicateAtomCount,
		CompositeCount:      receipt.Exposure.ActualCompositeCount,
	}
	if sample.OutcomeSetSHA256 != radix.Set.SetSHA256 || sample.ActualOutcomeFacts != 5 ||
		sample.ChargedOutcomeFacts != 5 || sample.PredicateAtomCount != 4 || sample.CompositeCount != 1 {
		t.Fatalf("Sample is not coherent with the signed production set: %+v", sample)
	}

	// Re-sign the now-coherent exposure evidence with the same deterministic
	// Gateway test key used by gatewayReceiptFor.
	receipt.Signature = ""
	signer, err := queryreceipt.NewSigner("repro-key",
		ed25519.NewKeyFromSeed([]byte(strings.Repeat("r", ed25519.SeedSize))))
	if err != nil {
		t.Fatalf("open receipt signer: %v", err)
	}
	receipt, err = signer.Sign(receipt)
	if err != nil {
		t.Fatalf("sign coherent receipt: %v", err)
	}
	verifier, err := queryreceipt.NewVerifier(map[string]ed25519.PublicKey{
		signer.KeyID(): signer.PublicKey(),
	})
	if err != nil {
		t.Fatalf("open receipt verifier: %v", err)
	}

	footprint := outcomeCandidateScaleFootprint(t, material.CatalogPath)
	inputs := IndependentInputsV3{
		CatalogPath: material.CatalogPath, Footprint: footprint,
		PostgreSQL: testRuntimeIdentity(), PathKind: PathPairedNovel,
		OperationID:      "scale/dependency-e2e/10k-overlap-0/novel",
		ContractIdentity: "strict-outcome-integration-test",
		VisibleSQL:       reproduced.VisibleSQL, CompanionSQL: reproduced.CompanionSQL,
	}
	carried := carriedFor(t, inputs)
	visible := reproduced.Visible
	carried.VisibleStatement = &visible
	carried.CompanionStatement = reproduced.Companion
	carried.VisiblePreparedTargetBindingSHA256, err =
		reproduced.Prepared.TargetSHA256(preparedbinding.RoleVisible)
	if err != nil {
		t.Fatalf("identify prepared visible target: %v", err)
	}
	carried.CompanionPreparedTargetBindingSHA256, err =
		reproduced.Prepared.TargetSHA256(preparedbinding.RoleCompanion)
	if err != nil {
		t.Fatalf("identify prepared companion target: %v", err)
	}
	// The Outcome candidate is verified through the domain linker since
	// 928ca83, so the trusted inputs must carry the linker, the frozen Catalog
	// digest the oracle was generated from, and the candidate fact count;
	// without them the finalizer refuses at "linker input is incomplete" before
	// the member assertion this test is about.
	trusted := TrustedInputsV3{
		CatalogPath: material.CatalogPath, Footprint: footprint,
		PostgreSQL: testRuntimeIdentity(), OperationID: inputs.OperationID,
		ContractIdentity: inputs.ContractIdentity, Material: &material,
		OutcomeCandidate: &expected, OutcomeCandidateCatalogSHA256: logicalCatalog.SHA256,
		OutcomeCandidateFacts:              finalv5oracle.DependencyScale10K,
		OutcomeCandidateLinker:             newOutcomeCandidateDomainLinkerV1(),
		SettlementWroteExecutionBindingRow: true,
	}
	_, err = finalizeTaskGateObservationV3Core(receipt, verifier, carried, trusted)
	if err == nil {
		t.Fatal("an entirely wrong but internally coherent five-member Outcome set was accepted")
	}
	assertTaskGateRejectionGate(t, err, rejectionGateFrozenMaterial)
	if !strings.Contains(err.Error(), "observed Outcome candidate members differ") {
		t.Fatalf("coherent wrong set was refused before the member assertion under test: %v", err)
	}
}
