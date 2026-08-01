package queryplan

import (
	"encoding/json"
	"strings"
	"testing"
)

func footprintProduct() Product {
	return Product{Name: "customer", SourceNamespace: "crm", Snapshot: "2026-08-01", StableRole: "customer",
		Columns: map[string]struct{}{"id": {}, "name": {}}, ColumnTypes: map[string]string{"id": "bigint", "name": "text"},
		ColumnCollations: map[string]string{"name": "C"}, CollationVersions: map[string]string{"name": "glibc-2.39"}}
}

func buildFootprintForTest(t *testing.T, filters []Filter) PredicateFootprint {
	t.Helper()
	product := footprintProduct()
	result, err := BuildPredicateFootprint(QueryPlan{Product: product.Name, Columns: []string{"id"}, Filters: filters},
		PredicateBindings{CatalogSHA256: strings.Repeat("a", 64), Products: map[string]Product{product.Name: product}},
		strings.Repeat("b", 64), PredicateLimits{})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func atomHashes(t *testing.T, footprint PredicateFootprint) map[string]struct{} {
	t.Helper()
	result := make(map[string]struct{}, len(footprint.Atoms))
	for _, atom := range footprint.Atoms {
		hash, err := atom.Hash()
		if err != nil {
			t.Fatal(err)
		}
		result[hash] = struct{}{}
	}
	return result
}

func TestPredicateFootprintINPackingConservationAndDuplicateIdempotence(t *testing.T) {
	packed := buildFootprintForTest(t, []Filter{{Column: "id", Op: "IN", Value: []any{
		json.Number("1"), json.Number("2"), json.Number("2"), json.Number("3")}}})
	var splitUnion = make(map[string]struct{})
	for _, value := range []string{"1", "2", "3"} {
		part := buildFootprintForTest(t, []Filter{{Column: "id", Op: "=", Value: json.Number(value)}})
		for hash := range atomHashes(t, part) {
			splitUnion[hash] = struct{}{}
		}
	}
	packedHashes := atomHashes(t, packed)
	if len(packedHashes) != 3 || len(splitUnion) != 3 || packed.DuplicateCount != 1 || packed.RawLiteralCount != 4 {
		t.Fatalf("unexpected packed footprint: %+v", packed)
	}
	for hash := range splitUnion {
		if _, present := packedHashes[hash]; !present {
			t.Fatalf("packed IN omitted split atom %s", hash)
		}
	}
}

func TestPredicateFootprintNOTINAndNullArePreserved(t *testing.T) {
	footprint := buildFootprintForTest(t, []Filter{{Column: "id", Op: "NOT IN", Value: []any{json.Number("1"), nil, nil}}})
	if footprint.RawLiteralCount != 3 || footprint.UniqueAtomCount != 2 || footprint.DuplicateCount != 1 || footprint.NullAtomCount != 1 {
		t.Fatalf("NULL footprint mismatch: %+v", footprint)
	}
	for _, atom := range footprint.Atoms {
		if atom.Operator != "NE" {
			t.Fatalf("NOT IN produced %q rather than NE", atom.Operator)
		}
	}
}

func TestPredicateContextSeparatesScopeAndIgnoresProjection(t *testing.T) {
	product := footprintProduct()
	bindings := PredicateBindings{CatalogSHA256: strings.Repeat("a", 64), Products: map[string]Product{product.Name: product}}
	left, err := BuildPredicateFootprint(QueryPlan{Product: product.Name, Columns: []string{"id"}, Filters: []Filter{{Column: "id", Op: "=", Value: json.Number("1")}}},
		bindings, strings.Repeat("b", 64), PredicateLimits{})
	if err != nil {
		t.Fatal(err)
	}
	reprojected, err := BuildPredicateFootprint(QueryPlan{Product: product.Name, Columns: []string{"name"}, Filters: []Filter{{Column: "id", Op: "=", Value: json.Number("1")}}},
		bindings, strings.Repeat("b", 64), PredicateLimits{})
	if err != nil {
		t.Fatal(err)
	}
	otherScope, err := BuildPredicateFootprint(QueryPlan{Product: product.Name, Columns: []string{"id"}, Filters: []Filter{{Column: "id", Op: "=", Value: json.Number("1")}}},
		bindings, strings.Repeat("c", 64), PredicateLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if left.ContextSHA256 != reprojected.ContextSHA256 || left.AtomSetSHA256 != reprojected.AtomSetSHA256 {
		t.Fatal("projection incorrectly changed predicate identity")
	}
	if left.ContextSHA256 == otherScope.ContextSHA256 || left.AtomSetSHA256 == otherScope.AtomSetSHA256 {
		t.Fatal("effective scope failed to separate predicate identity")
	}
}
