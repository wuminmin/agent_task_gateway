package finalv5oracle

import (
	"slices"
	"strings"
	"testing"
)

func TestLegalV5OutcomeFixedVectorAndExactReplay(t *testing.T) {
	input := V5OutcomeVectorInput{
		Atoms: []V5PredicateAtomInput{{SemanticProductID: "orders", StableRole: "orders", PublicFieldID: "id",
			SQLType: "bigint", Operator: "EQ", CanonicalLiteral: "i:1"}},
		QueryNormalFormVersion: "taskgate-query-normal-form-v4",
		QueryNormalFormSHA256:  strings.Repeat("3", 64), ResultObservationSHA256: strings.Repeat("4", 64),
		VisibleRows: 1, PredicateContextSHA256: strings.Repeat("1", 64),
	}
	first, err := BuildV5OutcomeVector(input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildV5OutcomeVector(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Atoms) != 1 || len(first.Members) != 2 || first.Atoms[0].SHA256 != "a7c2b24e1b57c75fcb4b6aff01f5a9e125f97c26a60d2a21cea4f845623747a2" ||
		first.PredicateSetSHA256 != "e640a3602b24ef409d15e0cccaf462f89a29e20db3614caf660693a1d859b0bb" ||
		first.Composite.SHA256 != "3d2a07e1a13c13ab6f2b59ffb590f630c0cae00e5709bc9c18da13833821463d" ||
		first.OutcomeSetSHA256 != "7ab957ab7056f4bb6235519df8be8c262636138a9722c902f7b228a28ba3e1e9" {
		t.Fatalf("legal V5 fixed vector = %+v", first)
	}
	if first.PredicateSetSHA256 != second.PredicateSetSHA256 || first.Composite.SHA256 != second.Composite.SHA256 ||
		first.OutcomeSetSHA256 != second.OutcomeSetSHA256 || !slices.Equal(first.Members, second.Members) {
		t.Fatal("exact V5 replay changed outcome identity")
	}
}

func TestLegalV5OutcomeMutationsAndEmptyOutputRemainExplicit(t *testing.T) {
	base := V5OutcomeVectorInput{
		Atoms: []V5PredicateAtomInput{{SemanticProductID: "orders", StableRole: "orders", PublicFieldID: "id",
			SQLType: "bigint", Operator: "EQ", CanonicalLiteral: "i:1"}},
		QueryNormalFormVersion: "taskgate-query-normal-form-v4", QueryNormalFormSHA256: strings.Repeat("2", 64),
		ResultObservationSHA256: strings.Repeat("3", 64), PredicateContextSHA256: strings.Repeat("1", 64),
	}
	original, err := BuildV5OutcomeVector(base)
	if err != nil {
		t.Fatal(err)
	}
	literal := base
	literal.Atoms = append([]V5PredicateAtomInput(nil), base.Atoms...)
	literal.Atoms[0].CanonicalLiteral = "i:2"
	changedLiteral, err := BuildV5OutcomeVector(literal)
	if err != nil {
		t.Fatal(err)
	}
	if changedLiteral.Atoms[0].SHA256 == original.Atoms[0].SHA256 || changedLiteral.Composite.SHA256 == original.Composite.SHA256 {
		t.Fatal("literal mutation did not change atom and bound composite")
	}
	normalForm := base
	normalForm.QueryNormalFormSHA256 = strings.Repeat("4", 64)
	changedNormalForm, err := BuildV5OutcomeVector(normalForm)
	if err != nil {
		t.Fatal(err)
	}
	if changedNormalForm.Atoms[0].SHA256 != original.Atoms[0].SHA256 || changedNormalForm.Composite.SHA256 == original.Composite.SHA256 {
		t.Fatal("normal-form mutation changed atom or failed to change composite")
	}
	observation := base
	observation.ResultObservationSHA256 = strings.Repeat("5", 64)
	changedObservation, err := BuildV5OutcomeVector(observation)
	if err != nil {
		t.Fatal(err)
	}
	if changedObservation.Atoms[0].SHA256 != original.Atoms[0].SHA256 || changedObservation.Composite.SHA256 == original.Composite.SHA256 {
		t.Fatal("result-observation mutation changed atom or failed to change composite")
	}

	empty, err := BuildV5OutcomeVector(V5OutcomeVectorInput{
		QueryNormalFormVersion: "taskgate-query-normal-form-v4", QueryNormalFormSHA256: strings.Repeat("6", 64),
		ResultObservationSHA256: strings.Repeat("7", 64), VisibleRows: 0, PredicateContextSHA256: strings.Repeat("8", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Atoms) != 0 || len(empty.Members) != 1 || empty.Members[0] != empty.Composite.SHA256 || !validSHA256(empty.OutcomeSetSHA256) {
		t.Fatalf("empty/zero result has no explicit composite outcome: %+v", empty)
	}
}

func TestOpaqueOutcomeMerkleSetsAreExactDeterministicAndSeparate(t *testing.T) {
	request := OpaqueOutcomeSetRequest{RootCardinality: 10, CandidateCardinality: 4, OverlapCardinality: 2,
		SampleIndex: 7, Seed: 20260801, SetOptions: StreamSetOptions{MaxInMemoryMembers: 4, CaptureMembers: 20, TempDir: t.TempDir()}}
	first, err := GenerateOpaqueOutcomeSets(request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateOpaqueOutcomeSets(request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Existing.Cardinality != 10 || first.Candidate.Cardinality != 4 || first.Overlap.Cardinality != 2 ||
		first.Novel.Cardinality != 2 || first.Union.Cardinality != 12 {
		t.Fatalf("opaque set algebra = %+v", first)
	}
	intersection := intersectTestMembers(first.Candidate.Members, first.Existing.Members)
	if !slices.Equal(intersection, first.Overlap.Members) {
		t.Fatalf("opaque intersection=%v overlap=%v", intersection, first.Overlap.Members)
	}
	if first.Candidate.SetSHA256 != second.Candidate.SetSHA256 || first.Union.SetSHA256 != second.Union.SetSHA256 {
		t.Fatal("opaque control exact replay changed its member sets")
	}
	if OpaqueOutcomeRootMember(0).SHA256 != "30aeac39f950f34dba0e22ad6a9ae6b8e69008b4a7bf38cb38aaf9962596a91d" ||
		OpaqueOutcomeNovelMember(20260801, 7, 0).SHA256 != "49a02130a713062d343c8ee37858112bb51c55982747bb0b1da8608a1c9a9ab9" {
		t.Fatalf("opaque fixed members root=%s novel=%s", OpaqueOutcomeRootMember(0).SHA256,
			OpaqueOutcomeNovelMember(20260801, 7, 0).SHA256)
	}
	for _, opaque := range []OpaqueOutcomeMember{OpaqueOutcomeRootMember(0), OpaqueOutcomeNovelMember(20260801, 7, 0)} {
		forged := CanonicalFact{Profile: OracleExposureProfileV5, Kind: OracleFactKindPredicateAtom, SHA256: opaque.SHA256}
		if err := ValidateCanonicalFact(forged); err == nil {
			t.Fatal("opaque Merkle member was accepted as a legal V5 semantic fact")
		}
	}
}

func TestOpaqueOutcomeOverlapAndSeedMutationsChangeExpectedSets(t *testing.T) {
	base := OpaqueOutcomeSetRequest{RootCardinality: 20, CandidateCardinality: 6, OverlapCardinality: 3,
		SampleIndex: 2, Seed: 99, SetOptions: StreamSetOptions{MaxInMemoryMembers: 8, CaptureMembers: 30, TempDir: t.TempDir()}}
	original, err := GenerateOpaqueOutcomeSets(base)
	if err != nil {
		t.Fatal(err)
	}
	overlapMutation := base
	overlapMutation.OverlapCardinality = 4
	changedOverlap, err := GenerateOpaqueOutcomeSets(overlapMutation)
	if err != nil {
		t.Fatal(err)
	}
	if changedOverlap.Overlap.Cardinality == original.Overlap.Cardinality || changedOverlap.Overlap.SetSHA256 == original.Overlap.SetSHA256 ||
		changedOverlap.Novel.Cardinality == original.Novel.Cardinality || changedOverlap.Union.SetSHA256 == original.Union.SetSHA256 {
		t.Fatal("opaque overlap mutation was not reflected in set expectations")
	}
	seedMutation := base
	seedMutation.Seed++
	changedSeed, err := GenerateOpaqueOutcomeSets(seedMutation)
	if err != nil {
		t.Fatal(err)
	}
	if changedSeed.Candidate.SetSHA256 == original.Candidate.SetSHA256 || changedSeed.Novel.SetSHA256 == original.Novel.SetSHA256 {
		t.Fatal("opaque seed mutation reused candidate/novel identities")
	}
}
