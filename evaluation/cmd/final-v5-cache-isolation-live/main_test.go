package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
)

func TestTaskAuthorizationClosureComesFromSourceControlledProfileRegistry(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	registryBytes, err := os.ReadFile(filepath.Join(root, "config/profiles/registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	var registry registryDocument
	if err := json.Unmarshal(registryBytes, &registry); err != nil {
		t.Fatal(err)
	}
	intersectionBytes, err := os.ReadFile(filepath.Join(root,
		"evaluation/final-v5-wsl2/profiles/product-intersection-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var intersection intersectionWire
	if err := json.Unmarshal(intersectionBytes, &intersection); err != nil {
		t.Fatal(err)
	}
	requiredProfiles := map[string]bool{}
	for _, pair := range intersection.Pairs {
		if pair.Applicable {
			requiredProfiles[pair.LeftProfileID] = true
			requiredProfiles[pair.RightProfileID] = true
		}
	}
	if len(requiredProfiles) == 0 {
		t.Fatal("source-controlled intersection matrix has no applicable profiles")
	}
	var nonceProfile registryProfile
	for _, profile := range registry.Profiles {
		if requiredProfiles[profile.ID] {
			authorization, err := deriveTaskAuthorization(root, profile, "provsql_orders")
			if err != nil {
				t.Fatalf("derive %s authorization: %v", profile.Alias, err)
			}
			if !reflect.DeepEqual(authorization.Products, profile.Closure.Products) {
				t.Fatalf("%s authorization Products = %v, want registry closure %v",
					profile.Alias, authorization.Products, profile.Closure.Products)
			}
			logical, err := catalog.Load(filepath.Join(root, profile.CatalogPath))
			if err != nil {
				t.Fatal(err)
			}
			policy, err := logical.ResolveTaskPolicy(authorization.Products)
			if err != nil {
				t.Fatalf("%s derived closure does not resolve through the source-controlled approval route: %v",
					profile.Alias, err)
			}
			for _, product := range policy.Products {
				if !reflect.DeepEqual(authorization.Columns[product.Name], product.FieldNames()) {
					t.Fatalf("authorization columns for %s = %v, want Catalog fields %v",
						product.Name, authorization.Columns[product.Name], product.FieldNames())
				}
			}
			delete(requiredProfiles, profile.ID)
		}
		if profile.Alias == "provsql-nonce-join" {
			nonceProfile = profile
		}
	}
	if len(requiredProfiles) != 0 {
		t.Fatalf("applicable profiles absent from registry: %v", requiredProfiles)
	}
	if nonceProfile.ID == "" {
		t.Fatal("source-controlled registry omits provsql-nonce-join")
	}
	authorization, err := deriveTaskAuthorization(root, nonceProfile, "provsql_orders")
	if err != nil {
		t.Fatal(err)
	}
	logical, err := catalog.Load(filepath.Join(root, nonceProfile.CatalogPath))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := logical.ResolveTaskPolicy(authorization.Products)
	if err != nil {
		t.Fatalf("derived closure does not resolve through the source-controlled approval route: %v", err)
	}
	if policy.BudgetProfile != "final-v5-provsql-low-v1" {
		t.Fatalf("derived closure selected budget profile %q", policy.BudgetProfile)
	}
	if values, ok := authorization.Scopes["partition_key"].([]string); !ok || !reflect.DeepEqual(values, []string{"1"}) {
		t.Fatalf("authorization partition scope = %#v, want Catalog value [1]", authorization.Scopes["partition_key"])
	}

	// Removing one registry closure member must not be repaired by a hidden
	// runner-side list: the product-scoped Catalog route rejects the subset.
	mutated := nonceProfile
	mutated.Closure.Products = []string{"provsql_lineitem", "provsql_orders"}
	if _, err := deriveTaskAuthorization(root, mutated, "provsql_orders"); err == nil {
		t.Fatal("registry closure mutation unexpectedly retained a compatible approval route")
	}
}
