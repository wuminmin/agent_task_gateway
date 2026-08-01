package exposure

import (
	"strings"
	"testing"
)

func TestPredicateAtomAndCompositeV5UseDisjointStableIdentities(t *testing.T) {
	contextDigest := strings.Repeat("1", 64)
	setDigest := strings.Repeat("2", 64)
	atom, err := NewPredicateAtomFactV5(PredicateAtomFactV5{
		PredicateContextSHA256: contextDigest, SemanticProductID: "orders", StableRole: "orders",
		PublicFieldID: "id", SQLType: "bigint", Operator: "EQ", CanonicalLiteral: "i:1",
	})
	if err != nil {
		t.Fatal(err)
	}
	composite, err := NewCompositeOutcomeFactV5(CompositeOutcomeFactV5{
		QueryNormalFormVersion: "taskgate-query-normal-form-v4", QueryNormalFormSHA256: strings.Repeat("3", 64),
		ResultObservationSHA256: strings.Repeat("4", 64), VisibleRows: 1,
		PredicateContextSHA256: contextDigest, PredicateSetSHA256: setDigest, PredicateAtomCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	atomHash, err := atom.Hash()
	if err != nil {
		t.Fatal(err)
	}
	compositeHash, err := composite.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if atomHash == compositeHash || atom.Kind != FactPredicateAtom || composite.Kind != FactCompositeOutcome {
		t.Fatalf("V5 fact kinds are not disjoint: atom=%s composite=%s", atomHash, compositeHash)
	}
	if atom.IsV3() || composite.IsV3() || !atom.IsV5() || !composite.IsV5() {
		t.Fatal("V5 facts were classified under the legacy outcome profile")
	}
}

func TestPredicateAtomRequiresCollationForText(t *testing.T) {
	_, err := NewPredicateAtomFactV5(PredicateAtomFactV5{
		PredicateContextSHA256: strings.Repeat("1", 64), SemanticProductID: "customer",
		StableRole: "customer", PublicFieldID: "name", SQLType: "text", Operator: "EQ", CanonicalLiteral: "s:alice",
	})
	if err == nil {
		t.Fatal("text atom without an attested collation was accepted")
	}
}

func TestObservationV5ValidatesCompositeAtomBinding(t *testing.T) {
	contextDigest := strings.Repeat("1", 64)
	atom, err := NewPredicateAtomFactV5(PredicateAtomFactV5{PredicateContextSHA256: contextDigest,
		SemanticProductID: "orders", StableRole: "orders", PublicFieldID: "id",
		SQLType: "bigint", Operator: "EQ", CanonicalLiteral: "i:1"})
	if err != nil {
		t.Fatal(err)
	}
	setDigest, err := PredicateSetHashV1([]FactID{atom})
	if err != nil {
		t.Fatal(err)
	}
	composite, err := NewCompositeOutcomeFactV5(CompositeOutcomeFactV5{
		QueryNormalFormVersion: "taskgate-query-normal-form-v4", QueryNormalFormSHA256: strings.Repeat("2", 64),
		ResultObservationSHA256: strings.Repeat("3", 64), PredicateContextSHA256: contextDigest,
		PredicateSetSHA256: setDigest, PredicateAtomCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (Observation{ProfileVersion: ProfileV5, Outcome: []FactID{composite, atom}}).Normalize(); err != nil {
		t.Fatal(err)
	}
	composite.PredicateAtomCount = 0
	if _, err := (Observation{ProfileVersion: ProfileV5, Outcome: []FactID{composite, atom}}).Normalize(); err == nil {
		t.Fatal("mismatched V5 composite count was accepted")
	}
}
