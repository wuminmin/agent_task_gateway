package queryplan

import (
	"encoding/json"
	"strings"
	"testing"
)

func footprintProduct() Product {
	return Product{Name: "customer", SourceNamespace: "crm", Snapshot: "2026-08-01", StableRole: "customer",
		Columns: map[string]struct{}{"id": {}, "name": {}}, ColumnTypes: map[string]string{"id": "bigint", "name": "text"},
		ColumnCollations: map[string]string{"name": "C"}, CollationVersions: map[string]string{"name": "glibc-2.39"},
		StableEntityKey: []string{"id"}}
}

func buildFootprintForTest(t *testing.T, filters []Filter) PredicateFootprint {
	t.Helper()
	product := footprintProduct()
	result, err := BuildPredicateFootprint(QueryPlan{Product: product.Name, Columns: []string{"id"}, Filters: filters},
		PredicateBindings{CatalogSHA256: strings.Repeat("a", 64), Products: map[PredicateProductKey]Product{
			{Role: product.StableRole, Product: product.Name}: product,
		}},
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
	bindings := PredicateBindings{CatalogSHA256: strings.Repeat("a", 64), Products: map[PredicateProductKey]Product{
		{Role: product.StableRole, Product: product.Name}: product,
	}}
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

func TestPredicateFootprintResolvesUnionBranchesByRoleAndProduct(t *testing.T) {
	product := footprintProduct()
	plan := QueryPlan{From: &From{UnionDistinct: &UnionDistinct{
		Role: "customer", Columns: []string{"id", "name"},
		Left: Scan{Product: product.Name, Role: "left_branch", Filters: []Filter{
			{Column: "id", Op: "<=", Value: json.Number("10")},
			{Column: "name", Op: "=", Value: "shared"},
		}},
		Right: Scan{Product: product.Name, Role: "right_branch", Filters: []Filter{
			{Column: "id", Op: "<=", Value: json.Number("20")},
			{Column: "name", Op: "=", Value: "shared"},
		}},
	}}, Columns: []string{"customer.id"}, Filters: []Filter{{
		Column: "customer.name", Op: "=", Value: "outer",
	}}}
	leftKey := PredicateProductKey{Role: "left_branch", Product: product.Name}
	rightKey := PredicateProductKey{Role: "right_branch", Product: product.Name}
	build := func(products map[PredicateProductKey]Product) (PredicateFootprint, error) {
		return BuildPredicateFootprint(plan, PredicateBindings{
			CatalogSHA256: strings.Repeat("a", 64), Products: products,
		}, strings.Repeat("b", 64), PredicateLimits{})
	}

	compiled, err := CompileRelational(plan, map[string]Product{product.Name: product})
	if err != nil {
		t.Fatal(err)
	}
	compiledProducts, err := PredicateProductsForSources(
		map[string]Product{product.Name: product}, compiled.Sources)
	if err != nil {
		t.Fatal(err)
	}
	forward, err := build(compiledProducts)
	if err != nil {
		t.Fatal(err)
	}
	reversedProducts := make(map[PredicateProductKey]Product, 2)
	reversedProducts[rightKey] = product
	reversedProducts[leftKey] = product
	reversed, err := build(reversedProducts)
	if err != nil {
		t.Fatal(err)
	}
	if forward.ContextSHA256 != reversed.ContextSHA256 || forward.AtomSetSHA256 != reversed.AtomSetSHA256 {
		t.Fatal("predicate footprint depends on composite-binding insertion order")
	}
	if forward.RawLiteralCount != 5 || forward.UniqueAtomCount != 5 || forward.DuplicateCount != 0 {
		t.Fatalf("branch-qualified footprint counts = %+v", forward)
	}
	roles := map[string]int{}
	for _, atom := range forward.Atoms {
		roles[atom.StableRole]++
		if atom.SemanticProductID != product.Name {
			t.Fatalf("UNION atom product = %q", atom.SemanticProductID)
		}
	}
	if roles["left_branch"] != 2 || roles["right_branch"] != 2 || roles["customer"] != 1 {
		t.Fatalf("branch-qualified atom roles = %v", roles)
	}
}

func TestPredicateFootprintRequiresEveryUnionBranchBinding(t *testing.T) {
	product := footprintProduct()
	plan := QueryPlan{From: &From{UnionDistinct: &UnionDistinct{
		Role: "customer", Columns: []string{"id"},
		Left: Scan{Product: product.Name, Role: "left_branch", Filters: []Filter{{
			Column: "id", Op: "<=", Value: json.Number("10"),
		}}},
		Right: Scan{Product: product.Name, Role: "right_branch", Filters: []Filter{{
			Column: "id", Op: "<=", Value: json.Number("20"),
		}}},
	}}, Columns: []string{"customer.id"}}
	_, err := BuildPredicateFootprint(plan, PredicateBindings{
		CatalogSHA256: strings.Repeat("a", 64), Products: map[PredicateProductKey]Product{
			{Role: "left_branch", Product: product.Name}: product,
		},
	}, strings.Repeat("b", 64), PredicateLimits{})
	if err == nil || !strings.Contains(err.Error(), `source role "right_branch"`) {
		t.Fatalf("missing right-branch binding error = %v", err)
	}

	wrong := product
	wrong.Name = "other"
	_, err = BuildPredicateFootprint(plan, PredicateBindings{
		CatalogSHA256: strings.Repeat("a", 64), Products: map[PredicateProductKey]Product{
			{Role: "left_branch", Product: product.Name}:  wrong,
			{Role: "right_branch", Product: product.Name}: product,
		},
	}, strings.Repeat("b", 64), PredicateLimits{})
	if err == nil || !strings.Contains(err.Error(), `contains product "other"`) {
		t.Fatalf("mismatched role/product binding error = %v", err)
	}
}

func TestPredicateFootprintRejectsNonCanonicalLegacyProductRole(t *testing.T) {
	product := footprintProduct()
	_, err := BuildPredicateFootprint(QueryPlan{
		Product: product.Name, Columns: []string{"id"}, Filters: []Filter{{
			Column: "id", Op: "=", Value: json.Number("1"),
		}},
	}, PredicateBindings{
		CatalogSHA256: strings.Repeat("a", 64), Products: map[PredicateProductKey]Product{
			{Role: product.StableRole, Product: product.Name}: product,
			{Role: "display_alias", Product: product.Name}:    product,
		},
	}, strings.Repeat("b", 64), PredicateLimits{})
	if err == nil || !strings.Contains(err.Error(), "does not use its stable role") {
		t.Fatalf("non-canonical legacy product role error = %v", err)
	}
}
