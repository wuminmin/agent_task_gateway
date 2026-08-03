package finalv5oracle

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

func TestCanonicalFactFixedVectors(t *testing.T) {
	row, err := BuildV2BaseRowFact(V2BaseRowInput{
		SourceNamespace: "travel.expense", Snapshot: "snapshot-1", EntityKey: "row-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	cell, err := BuildV2BaseCellFact(V2BaseCellInput{
		SourceNamespace: "travel.expense", Snapshot: "snapshot-1", EntityKey: "row-1",
		Field: "amount", SQLType: "numeric", CanonicalValue: "n:1",
	})
	if err != nil {
		t.Fatal(err)
	}
	contextDigest := strings.Repeat("1", 64)
	atom, err := BuildV5PredicateAtomFact(V5PredicateAtomInput{
		PredicateContextSHA256: contextDigest, SemanticProductID: "orders", StableRole: "orders",
		PublicFieldID: "id", SQLType: "bigint", Operator: "EQ", CanonicalLiteral: "i:1",
	})
	if err != nil {
		t.Fatal(err)
	}
	predicateSet, err := HashV5PredicateSet([]CanonicalFact{atom})
	if err != nil {
		t.Fatal(err)
	}
	composite, err := BuildV5CompositeOutcomeFact(V5CompositeOutcomeInput{
		QueryNormalFormVersion: "taskgate-query-normal-form-v4", QueryNormalFormSHA256: strings.Repeat("3", 64),
		ResultObservationSHA256: strings.Repeat("4", 64), VisibleRows: 1,
		PredicateContextSHA256: contextDigest, PredicateSetSHA256: predicateSet, PredicateAtomCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	derived, err := BuildV2DerivedFact(V2DerivedInput{
		SnapshotBundle: []V2SnapshotBinding{
			{SourceNamespace: "final_v5.other", Snapshot: "snapshot-b"},
			{SourceNamespace: "final_v5.exposure_scale", Snapshot: "snapshot-a"},
		},
		OutputRowKey: "group-1", NormalizedExpression: "count(*)", SQLType: "bigint",
		CanonicalValue: "i:2000", WitnessCommitment: strings.Repeat("b", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	vectors := []struct {
		name       string
		fact       CanonicalFact
		payloadHex string
		factSHA256 string
	}{
		{name: "v2-row", fact: row,
			payloadHex: "0000000000000008626173652d726f7700000000000000147461736b676174652d6578706f737572652d7632000000000000000e74726176656c2e657870656e7365000000000000000a736e617073686f742d310000000000000005726f772d31",
			factSHA256: "e0e52440715590913ed7ae66f9523ef03a48c1b52ad14b8942a9cffca527acb7"},
		{name: "v2-cell", fact: cell,
			payloadHex: "0000000000000009626173652d63656c6c00000000000000147461736b676174652d6578706f737572652d7632000000000000000e74726176656c2e657870656e7365000000000000000a736e617073686f742d310000000000000005726f772d310000000000000006616d6f756e7400000000000000076e756d6572696300000000000000036e3a31",
			factSHA256: "3f8010a6355d05b003ab82b0deb878f1ccf4cebaafdfed92f8512a0c3cdb933a"},
		{name: "v2-derived", fact: derived,
			payloadHex: "00000000000000076465726976656400000000000000147461736b676174652d6578706f737572652d76320000000000000002000000000000001766696e616c5f76352e6578706f737572655f7363616c65000000000000000a736e617073686f742d61000000000000000e66696e616c5f76352e6f74686572000000000000000a736e617073686f742d62000000000000000767726f75702d310000000000000008636f756e74282a290000000000000006626967696e740000000000000006693a32303030000000000000004062626262626262626262626262626262626262626262626262626262626262626262626262626262626262626262626262626262626262626262626262626262",
			factSHA256: "9cfd0ea77190aaf6f05f5ee25a382f5a0b9eee4760a40891be63fdff9b5ad11a"},
		{name: "v5-atom", fact: atom,
			payloadHex: "000000000000000e7072656469636174652d61746f6d00000000000000147461736b676174652d6578706f737572652d7635000000000000001f7461736b676174652d7072656469636174652d666f6f747072696e742d763100000000000000403131313131313131313131313131313131313131313131313131313131313131313131313131313131313131313131313131313131313131313131313131313100000000000000066f726465727300000000000000066f72646572730000000000000002696400000000000000000000000000000006626967696e7400000000000000000000000000000000000000000000000245510000000000000003693a31",
			factSHA256: "a7c2b24e1b57c75fcb4b6aff01f5a9e125f97c26a60d2a21cea4f845623747a2"},
		{name: "v5-composite", fact: composite,
			payloadHex: "0000000000000011636f6d706f736974652d6f7574636f6d6500000000000000147461736b676174652d6578706f737572652d7635000000000000001d7461736b676174652d71756572792d6e6f726d616c2d666f726d2d76340000000000000040333333333333333333333333333333333333333333333333333333333333333333333333333333333333333333333333333333333333333333333333333333330000000000000040343434343434343434343434343434343434343434343434343434343434343434343434343434343434343434343434343434343434343434343434343434340000000000000001000000000000001f7461736b676174652d7072656469636174652d666f6f747072696e742d76310000000000000040313131313131313131313131313131313131313131313131313131313131313131313131313131313131313131313131313131313131313131313131313131310000000000000040653634306133363032623234656634303964313565306363636166343632663839613239653230646233363134636166363630363933613164383539623062620000000000000001",
			factSHA256: "3d2a07e1a13c13ab6f2b59ffb590f630c0cae00e5709bc9c18da13833821463d"},
	}
	for _, vector := range vectors {
		t.Run(vector.name, func(t *testing.T) {
			gotPayload := hex.EncodeToString(vector.fact.Payload)
			if gotPayload != vector.payloadHex || vector.fact.SHA256 != vector.factSHA256 {
				t.Fatalf("fixed vector mismatch\npayload=%s\nhash=%s\npredicate_set=%s", gotPayload, vector.fact.SHA256, predicateSet)
			}
			if err := ValidateCanonicalFact(vector.fact); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestV2DerivedBundleOrderAndMutations(t *testing.T) {
	base := V2DerivedInput{SnapshotBundle: []V2SnapshotBinding{
		{SourceNamespace: "z.source", Snapshot: "s2"}, {SourceNamespace: "a.source", Snapshot: "s1"}},
		OutputRowKey: "group-key", NormalizedExpression: "sum(final_v5_exposure_scale.metric)",
		SQLType: "numeric", CanonicalValue: "n:3/2", WitnessCommitment: strings.Repeat("c", 64)}
	first, err := BuildV2DerivedFact(base)
	if err != nil {
		t.Fatal(err)
	}
	reordered := base
	reordered.SnapshotBundle = []V2SnapshotBinding{base.SnapshotBundle[1], base.SnapshotBundle[0]}
	second, err := BuildV2DerivedFact(reordered)
	if err != nil {
		t.Fatal(err)
	}
	if first.SHA256 != second.SHA256 || !bytes.Equal(first.Payload, second.Payload) {
		t.Fatal("snapshot bundle enumeration order changed derived identity")
	}
	mutations := []V2DerivedInput{}
	for _, mutate := range []func(*V2DerivedInput){
		func(value *V2DerivedInput) { value.SnapshotBundle[0].Snapshot = "s3" },
		func(value *V2DerivedInput) { value.OutputRowKey = "other-group" },
		func(value *V2DerivedInput) { value.NormalizedExpression = "count(*)" },
		func(value *V2DerivedInput) { value.SQLType, value.CanonicalValue = "bigint", "i:1" },
		func(value *V2DerivedInput) { value.CanonicalValue = "n:2" },
		func(value *V2DerivedInput) { value.WitnessCommitment = strings.Repeat("d", 64) },
	} {
		changed := base
		changed.SnapshotBundle = append([]V2SnapshotBinding(nil), base.SnapshotBundle...)
		mutate(&changed)
		mutations = append(mutations, changed)
	}
	for index, mutation := range mutations {
		changed, err := BuildV2DerivedFact(mutation)
		if err != nil {
			t.Fatalf("derived mutation %d: %v", index, err)
		}
		if changed.SHA256 == first.SHA256 {
			t.Fatalf("derived mutation %d reused original identity", index)
		}
	}
	duplicateNamespace := base
	duplicateNamespace.SnapshotBundle = []V2SnapshotBinding{{SourceNamespace: "same", Snapshot: "s1"}, {SourceNamespace: "same", Snapshot: "s2"}}
	if _, err := BuildV2DerivedFact(duplicateNamespace); err == nil {
		t.Fatal("duplicate snapshot namespace was accepted")
	}
}

func TestCanonicalFactMutationsChangeIdentityAndStaleVectorsFail(t *testing.T) {
	base := V2BaseCellInput{SourceNamespace: "final_v5.exposure_scale", Snapshot: "publication-v1",
		EntityKey: strings.Repeat("a", 64), Field: "exposure_scale.partition_key", SQLType: "integer", CanonicalValue: "i:1"}
	original, err := BuildV2BaseCellFact(base)
	if err != nil {
		t.Fatal(err)
	}
	mutations := []V2BaseCellInput{
		{SourceNamespace: "final_v5.exposure_scale.changed", Snapshot: base.Snapshot, EntityKey: base.EntityKey, Field: base.Field, SQLType: base.SQLType, CanonicalValue: base.CanonicalValue},
		{SourceNamespace: base.SourceNamespace, Snapshot: "publication-v2", EntityKey: base.EntityKey, Field: base.Field, SQLType: base.SQLType, CanonicalValue: base.CanonicalValue},
		{SourceNamespace: base.SourceNamespace, Snapshot: base.Snapshot, EntityKey: strings.Repeat("b", 64), Field: base.Field, SQLType: base.SQLType, CanonicalValue: base.CanonicalValue},
		{SourceNamespace: base.SourceNamespace, Snapshot: base.Snapshot, EntityKey: base.EntityKey, Field: "exposure_scale.changed", SQLType: base.SQLType, CanonicalValue: base.CanonicalValue},
		{SourceNamespace: base.SourceNamespace, Snapshot: base.Snapshot, EntityKey: base.EntityKey, Field: base.Field, SQLType: base.SQLType, CanonicalValue: "i:2"},
	}
	for index, mutation := range mutations {
		changed, err := BuildV2BaseCellFact(mutation)
		if err != nil {
			t.Fatalf("mutation %d: %v", index, err)
		}
		if changed.SHA256 == original.SHA256 || bytes.Equal(changed.Payload, original.Payload) {
			t.Fatalf("mutation %d reused the original fact", index)
		}
	}
	stalePayload := original
	stalePayload.Payload = append([]byte(nil), stalePayload.Payload...)
	stalePayload.Payload[len(stalePayload.Payload)-1] ^= 1
	if err := ValidateCanonicalFact(stalePayload); err == nil {
		t.Fatal("payload mutation with stale hash was accepted")
	}
	staleHash := original
	staleHash.SHA256 = strings.Repeat("0", 64)
	if err := ValidateCanonicalFact(staleHash); err == nil {
		t.Fatal("hash mutation with unchanged payload was accepted")
	}
}

func TestCanonicalFactRejectsIllegalTypesLiteralsAndCollations(t *testing.T) {
	baseCell := V2BaseCellInput{SourceNamespace: "source", Snapshot: "snapshot", EntityKey: "key",
		Field: "role.id", SQLType: "bigint", CanonicalValue: "i:1"}
	for _, mutation := range []V2BaseCellInput{
		{SourceNamespace: "source", Snapshot: "snapshot", EntityKey: "key", Field: "role.id", SQLType: "int8", CanonicalValue: "i:1"},
		{SourceNamespace: "source", Snapshot: "snapshot", EntityKey: "key", Field: "role.id", SQLType: "bigint", CanonicalValue: "i:01"},
	} {
		if _, err := BuildV2BaseCellFact(mutation); err == nil {
			t.Fatalf("accepted illegal base cell %+v", mutation)
		}
	}
	if _, err := BuildV2BaseCellFact(baseCell); err != nil {
		t.Fatal(err)
	}
	atom := V5PredicateAtomInput{PredicateContextSHA256: strings.Repeat("1", 64), SemanticProductID: "customer",
		StableRole: "customer", PublicFieldID: "name", SQLType: "text", Operator: "EQ", CanonicalLiteral: "s:alice"}
	if _, err := BuildV5PredicateAtomFact(atom); err == nil {
		t.Fatal("text atom without collation was accepted")
	}
	atom.CollationName, atom.CollationVersion = "C", "builtin"
	if _, err := BuildV5PredicateAtomFact(atom); err != nil {
		t.Fatal(err)
	}
	atom.Operator = "ILIKE"
	if _, err := BuildV5PredicateAtomFact(atom); err == nil {
		t.Fatal("unsupported operator was accepted")
	}
}

func TestPredicateSetIsOrderIndependentAndExactDuplicateFree(t *testing.T) {
	contextDigest := strings.Repeat("a", 64)
	build := func(literal string) CanonicalFact {
		fact, err := BuildV5PredicateAtomFact(V5PredicateAtomInput{PredicateContextSHA256: contextDigest,
			SemanticProductID: "orders", StableRole: "orders", PublicFieldID: "id",
			SQLType: "bigint", Operator: "EQ", CanonicalLiteral: literal})
		if err != nil {
			t.Fatal(err)
		}
		return fact
	}
	one, two := build("i:1"), build("i:2")
	left, err := HashV5PredicateSet([]CanonicalFact{one, two, one})
	if err != nil {
		t.Fatal(err)
	}
	right, err := HashV5PredicateSet([]CanonicalFact{two, one})
	if err != nil {
		t.Fatal(err)
	}
	if left != right {
		t.Fatal("predicate set depends on order or exact duplicates")
	}
	changed, _ := HashV5PredicateSet([]CanonicalFact{one})
	if changed == left {
		t.Fatal("removing an atom did not change the predicate set")
	}
}
