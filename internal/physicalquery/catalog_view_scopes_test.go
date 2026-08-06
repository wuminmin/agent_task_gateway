package physicalquery

import (
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/queryplan"
)

// testScopedCatalog is a loaded Catalog whose products carry mandatory scopes,
// which is what makes the scope policy set load-bearing.
func testScopedCatalog() catalog.Catalog {
	return catalog.Catalog{
		CatalogVersion: "catalog-v4",
		SHA256:         strings.Repeat("d", 64),
		Products: []catalog.Product{
			{Name: "expense", Scopes: []string{"department"}},
			{Name: "headcount", Scopes: []string{"department"}},
		},
		Scopes: []catalog.Scope{
			{Name: "region", Type: catalog.ScopeTypeEnum, AllowedValues: []string{"emea", "apac"}},
			{Name: "department", Type: catalog.ScopeTypeEnum, AllowedValues: []string{"sales", "eng"}},
		},
		SnapshotPublications: []catalog.SnapshotPublication{
			testPublication("headcount"), testPublication("expense"),
		},
	}
}

// TestCatalogViewFromCatalogCanonicalizes proves the one constructor produces a
// canonical view regardless of the order the Catalog happened to declare things
// in. Two deployments that list the same policies differently must prepare to
// one identity, or the finalizer could never reproduce the Gateway's binding.
func TestCatalogViewFromCatalogCanonicalizes(t *testing.T) {
	view, err := CatalogViewFromCatalog(testScopedCatalog())
	if err != nil {
		t.Fatalf("build view: %v", err)
	}
	if len(view.Scopes) != 2 || view.Scopes[0].Name != "department" || view.Scopes[1].Name != "region" {
		t.Fatalf("scopes are not in canonical order: %+v", view.Scopes)
	}
	if got := view.Scopes[1].AllowedValues; got[0] != "apac" || got[1] != "emea" {
		t.Errorf("allowed values are not canonical: %v", got)
	}
	if view.SnapshotPublications[0].Name != "expense" {
		t.Errorf("publications are not in canonical order: %v", view.SnapshotPublications[0].Name)
	}
	if err := view.Validate(); err != nil {
		t.Errorf("a view from the shared constructor does not validate: %v", err)
	}

	// Declaration order must not reach the identity.
	shuffled := testScopedCatalog()
	shuffled.Scopes[0], shuffled.Scopes[1] = shuffled.Scopes[1], shuffled.Scopes[0]
	shuffled.Scopes[0].AllowedValues = []string{"eng", "sales"}
	other, err := CatalogViewFromCatalog(shuffled)
	if err != nil {
		t.Fatalf("build shuffled view: %v", err)
	}
	if digestOf(t, view) != digestOf(t, other) {
		t.Error("two declaration orders of one scope policy set reached different content digests")
	}
}

func digestOf(t *testing.T, view CatalogView) string {
	t.Helper()
	digest, err := view.ContentSHA256()
	if err != nil {
		t.Fatalf("content digest: %v", err)
	}
	return digest
}

// TestCatalogViewRejectsAmbiguousScopeIdentity covers the cases that would make
// a governance decision rest on something undetermined.
func TestCatalogViewRejectsAmbiguousScopeIdentity(t *testing.T) {
	for name, mutate := range map[string]func(*catalog.Catalog){
		"duplicate scope name": func(c *catalog.Catalog) {
			c.Scopes = append(c.Scopes, catalog.Scope{Name: "department", Type: catalog.ScopeTypeEnum})
		},
		"unnamed scope": func(c *catalog.Catalog) {
			c.Scopes = append(c.Scopes, catalog.Scope{Name: "  ", Type: catalog.ScopeTypeEnum})
		},
		"scope without a type": func(c *catalog.Catalog) {
			c.Scopes = append(c.Scopes, catalog.Scope{Name: "cost_centre"})
		},
		"duplicate allowed value": func(c *catalog.Catalog) {
			c.Scopes[1].AllowedValues = []string{"sales", "sales"}
		},
		"product requires an undeclared scope": func(c *catalog.Catalog) {
			c.Products[0].Scopes = []string{"cost_centre"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			loaded := testScopedCatalog()
			mutate(&loaded)
			if _, err := CatalogViewFromCatalog(loaded); err == nil {
				t.Fatal("the constructor accepted a catalog that cannot settle a governance decision")
			}
		})
	}
}

// TestCatalogViewValidateRejectsNonCanonicalScopes keeps the hand-built path
// honest. CatalogViewFromCatalog canonicalizes, but the struct's fields are
// exported, so Validate has to reject what the constructor would never produce.
func TestCatalogViewValidateRejectsNonCanonicalScopes(t *testing.T) {
	base, err := CatalogViewFromCatalog(testScopedCatalog())
	if err != nil {
		t.Fatalf("build view: %v", err)
	}
	for name, mutate := range map[string]func(*CatalogView){
		"scopes out of order": func(v *CatalogView) {
			v.Scopes = []catalog.Scope{v.Scopes[1], v.Scopes[0]}
		},
		"allowed values out of order": func(v *CatalogView) {
			v.Scopes[0].AllowedValues = []string{"sales", "eng"}
		},
		"declared scope removed while a product still requires it": func(v *CatalogView) {
			v.Scopes = v.Scopes[1:]
		},
	} {
		t.Run(name, func(t *testing.T) {
			view := base
			view.Scopes = append([]catalog.Scope(nil), base.Scopes...)
			mutate(&view)
			if err := view.Validate(); err == nil {
				t.Fatal("Validate accepted a non-canonical or incomplete scope set")
			}
		})
	}
}

// TestScopeValuesReachThePreparationIdentity is the load-bearing one for T1c-S1.
//
// Scopes are only worth carrying if changing one changes what was prepared. A
// scope policy that governs a View but sits outside the signed identity would
// let two preparations with different policies produce the same binding, and the
// finalizer's comparison would not notice.
func TestScopeValuesReachThePreparationIdentity(t *testing.T) {
	baseline := scopedInputs(t, testScopedCatalog())
	before, err := baseline.SHA256()
	if err != nil {
		t.Fatalf("baseline inputs digest: %v", err)
	}

	for name, mutate := range map[string]func(*catalog.Catalog){
		"an allowed value is added": func(c *catalog.Catalog) {
			c.Scopes[1].AllowedValues = append(c.Scopes[1].AllowedValues, "finance")
		},
		"an allowed value is removed": func(c *catalog.Catalog) {
			c.Scopes[1].AllowedValues = []string{"sales"}
		},
		"a scope type changes": func(c *catalog.Catalog) {
			c.Scopes[1].Type = catalog.ScopeTypeDateRange
			c.Scopes[1].AllowedValues = nil
			c.Scopes[1].Min, c.Scopes[1].Max = "2026-01-01", "2026-12-31"
		},
		"a scope bound changes": func(c *catalog.Catalog) {
			c.Scopes[0].Min = "2026-01-01"
		},
		"an unrelated scope is declared": func(c *catalog.Catalog) {
			c.Scopes = append(c.Scopes, catalog.Scope{Name: "cost_centre", Type: catalog.ScopeTypeEnum})
		},
	} {
		t.Run(name, func(t *testing.T) {
			loaded := testScopedCatalog()
			mutate(&loaded)
			after, err := scopedInputs(t, loaded).SHA256()
			if err != nil {
				t.Fatalf("mutated inputs digest: %v", err)
			}
			if after == before {
				t.Fatal("changing a Catalog scope policy did not move the preparation identity")
			}
		})
	}
}

// TestCatalogDigestCannotCarryAnotherCatalogsScopes is the pairing the spec
// calls out: presenting one Catalog's digest beside another Catalog's scope
// values must not reproduce the first Catalog's identity.
//
// Digest is caller-supplied and this package cannot recompute it -- the
// Catalog's digest is over artifact bytes a view does not carry -- so the
// defence is that the content is signed too.
func TestCatalogDigestCannotCarryAnotherCatalogsScopes(t *testing.T) {
	honest := scopedInputs(t, testScopedCatalog())
	expected, err := honest.SHA256()
	if err != nil {
		t.Fatalf("honest digest: %v", err)
	}

	// Same claimed provenance, different policy.
	forged := testScopedCatalog()
	forged.Scopes[1].AllowedValues = []string{"sales", "eng", "legal"}
	swapped := scopedInputs(t, forged)
	if swapped.Catalog.Digest != honest.Catalog.Digest {
		t.Fatal("the test did not actually hold the claimed catalog digest fixed")
	}
	got, err := swapped.SHA256()
	if err != nil {
		t.Fatalf("forged digest: %v", err)
	}
	if got == expected {
		t.Fatal("one Catalog's digest paired with another Catalog's scopes reproduced the original identity")
	}
}

// TestEquivalentScopePoliciesRequiresDeclaration proves scope propagation cannot
// rest on a policy the Catalog never declared.
func TestEquivalentScopePoliciesRequiresDeclaration(t *testing.T) {
	view, err := CatalogViewFromCatalog(testScopedCatalog())
	if err != nil {
		t.Fatalf("build view: %v", err)
	}
	if !view.EquivalentScopePolicies("department", "department") {
		t.Error("a declared scope is not equivalent to itself")
	}
	if view.EquivalentScopePolicies("department", "region") {
		t.Error("two scopes with different allowed values were treated as equivalent")
	}
	// The vacuous case: an undeclared name must never satisfy the proof, not
	// even against itself.
	if view.EquivalentScopePolicies("cost_centre", "cost_centre") {
		t.Error("an undeclared scope was treated as equivalent to itself")
	}
}

// scopedInputs builds a minimal non-ordinal preparation over a scoped Catalog.
func scopedInputs(t *testing.T, loaded catalog.Catalog) PreparationInputs {
	t.Helper()
	view, err := CatalogViewFromCatalog(loaded)
	if err != nil {
		t.Fatalf("build view: %v", err)
	}
	grant := testGrant()
	grant.ExposureProfile = ""
	return PreparationInputs{
		Plan:    queryPlanForScopeTest(),
		Grant:   grant,
		Catalog: view,
	}
}

func queryPlanForScopeTest() queryplan.QueryPlan {
	return queryplan.QueryPlan{Product: "expense", Columns: []string{"month"}, Limit: 10}
}
