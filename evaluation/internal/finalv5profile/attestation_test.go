package finalv5profile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
)

func loadAttestationRegistry(t *testing.T) SchemaAttestationRegistry {
	t.Helper()
	value, err := os.ReadFile(filepath.Join(repositoryRoot, "config/profiles/schema-attestations-v1.json"))
	if err != nil {
		t.Fatalf("read schema attestations: %v", err)
	}
	var registry SchemaAttestationRegistry
	if err := json.Unmarshal(value, &registry); err != nil {
		t.Fatalf("decode schema attestations: %v", err)
	}
	if err := registry.Validate(); err != nil {
		t.Fatalf("schema attestation registry is invalid: %v", err)
	}
	return registry
}

// Every profile whose Catalog a deployment can publish must carry its own
// attestation, and no other profile may carry one.
func TestEveryMaterializableProfileIsAttested(t *testing.T) {
	profiles := loadRegistry(t)
	attestations := loadAttestationRegistry(t)
	for _, profile := range profiles.Profiles {
		attestation, found := attestations.Lookup(profile.ID)
		if profile.Status.CatalogMaterializable {
			if !found {
				t.Fatalf("materializable profile %q has no schema attestation", profile.Alias)
			}
			if attestation.ClosureSHA256 != profile.Closure.SHA256 {
				t.Fatalf("profile %q attestation binds a different closure", profile.Alias)
			}
			if attestation.ProductSetSHA256 != CanonicalNameSetSHA256("product-set", profile.Closure.Products) {
				t.Fatalf("profile %q attestation binds a different Product set", profile.Alias)
			}
			continue
		}
		// depth4-semantic-view and exposure-scale are not materializable. A
		// fabricated digest for them would be worse than none.
		if found {
			t.Fatalf("non-materializable profile %q carries a schema attestation", profile.Alias)
		}
	}
}

// The generated profile Catalog must actually carry its own attestation, not
// the digest computed for the full Catalog.
func TestProfileCatalogCarriesItsOwnSchemaAttestation(t *testing.T) {
	profiles := loadRegistry(t)
	attestations := loadAttestationRegistry(t)
	full, err := catalog.Load(filepath.Join(repositoryRoot, "config/catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	fullDigest := full.Sources[0].SchemaDigest
	seen := map[string]bool{}
	for _, profile := range profiles.Profiles {
		if !profile.Status.CatalogMaterializable {
			continue
		}
		loaded, err := catalog.Load(filepath.Join(repositoryRoot, profile.CatalogPath))
		if err != nil {
			t.Fatal(err)
		}
		attestation, _ := attestations.Lookup(profile.ID)
		if loaded.Sources[0].SchemaDigest != attestation.SchemaDigest {
			t.Fatalf("profile %q Catalog carries %q, its attestation is %q",
				profile.Alias, loaded.Sources[0].SchemaDigest, attestation.SchemaDigest)
		}
		// A profile that declares fewer Products than the full Catalog cannot
		// legitimately share the full Catalog's attestation.
		if len(profile.Closure.Products) < len(full.Products) &&
			loaded.Sources[0].SchemaDigest == fullDigest {
			t.Fatalf("profile %q still inherits the full Catalog schema digest", profile.Alias)
		}
		if seen[profile.CatalogSHA256] {
			t.Fatalf("two profiles share Catalog digest %s", profile.CatalogSHA256)
		}
		seen[profile.CatalogSHA256] = true
	}
}

// Changing only the schema attestation must change the Catalog bytes and their
// digest while leaving the closure identity and the profile ID alone.
func TestSchemaAttestationChangesCatalogButNotProfileIdentity(t *testing.T) {
	profiles := loadRegistry(t)
	profile := profileByAlias(t, profiles, "result-heavy")
	path := filepath.Join(repositoryRoot, profile.CatalogPath)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := catalog.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	mutated := strings.Replace(string(original),
		"schema_digest: "+loaded.Sources[0].SchemaDigest,
		"schema_digest: "+strings.Repeat("a", 64), 1)
	if mutated == string(original) {
		t.Fatal("the profile Catalog does not carry a schema digest")
	}
	temporary := filepath.Join(t.TempDir(), "mutated.catalog.yaml")
	if err := os.WriteFile(temporary, []byte(mutated), 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded, err := catalog.Load(temporary)
	if err != nil {
		t.Fatalf("a schema-digest-only edit made the Catalog unloadable: %v", err)
	}
	if reloaded.SHA256 == loaded.SHA256 {
		t.Fatal("changing the schema digest did not change the Catalog digest")
	}
	// The closure is computed from Products, Publications, Sources and Scopes,
	// so a schema attestation can never move a profile ID.
	closure, reasons, err := ComputeClosure(reloaded, profile.Closure.Products)
	if err != nil || len(reasons) != 0 {
		t.Fatalf("closure over the mutated Catalog: reasons=%+v err=%v", reasons, err)
	}
	if closure.SHA256 != profile.Closure.SHA256 {
		t.Fatalf("closure digest moved from %s to %s", profile.Closure.SHA256, closure.SHA256)
	}
	if ProfileID(closure.SHA256) != profile.ID {
		t.Fatalf("profile ID moved from %s to %s", profile.ID, ProfileID(closure.SHA256))
	}
}

// An activation that presents a stale Catalog digest must be refused, and the
// refusal must be recorded rather than silently corrected.
func TestActivationRejectsAStaleCatalogDigest(t *testing.T) {
	profiles := loadRegistry(t)
	profile := profileByAlias(t, profiles, "result-heavy")
	evidence := passingEvidence(t, profile)
	// The digest the full Catalog would have produced before C14.
	evidence.CatalogSHA256 = strings.Repeat("b", 64)
	if err := ValidateActivationEvidence(evidence, profile); err == nil {
		t.Fatal("an activation against a stale Catalog digest was accepted")
	}
	// The reverse case: the evidence claims the right Catalog but the profile
	// registry moved on.
	stale := profile
	stale.CatalogSHA256 = strings.Repeat("c", 64)
	if err := ValidateActivationEvidence(passingEvidence(t, profile), stale); err == nil {
		t.Fatal("an activation against a drifted registry Catalog digest was accepted")
	}
}

// A change to the reporting-view set must invalidate the attestation that
// described the previous set.
func TestReportingViewSetChangeInvalidatesTheAttestation(t *testing.T) {
	attestations := loadAttestationRegistry(t)
	if len(attestations.Profiles) == 0 {
		t.Fatal("no attestation to mutate")
	}
	original := attestations.Profiles[0]
	if original.ReportingViewSetSHA256 != CanonicalNameSetSHA256("reporting-view-set", original.ReportingViews) {
		t.Fatal("the recorded view-set digest does not describe its own views")
	}
	widened := append(append([]string(nil), original.ReportingViews...), "reporting.some_other_view")
	if CanonicalNameSetSHA256("reporting-view-set", widened) == original.ReportingViewSetSHA256 {
		t.Fatal("adding a reporting view did not change the view-set digest")
	}
	// The registry validator must reject a record whose digest no longer
	// describes its own view list.
	mutated := attestations
	mutated.Profiles = append([]SchemaAttestation(nil), attestations.Profiles...)
	mutated.Profiles[0].ReportingViews = widened
	if err := mutated.Validate(); err == nil {
		t.Fatal("a stale reporting-view-set digest was accepted")
	}
}

// The name-set digest is order independent and domain separated, so a Product
// set and a view set with the same members never collide.
func TestCanonicalNameSetDigestIsStable(t *testing.T) {
	forward := CanonicalNameSetSHA256("product-set", []string{"b", "a", "c"})
	reversed := CanonicalNameSetSHA256("product-set", []string{"c", "b", "a"})
	if forward != reversed {
		t.Fatal("the name-set digest depends on input order")
	}
	if CanonicalNameSetSHA256("reporting-view-set", []string{"a", "b", "c"}) == forward {
		t.Fatal("two domains produced the same digest for the same members")
	}
	if CanonicalNameSetSHA256("product-set", []string{"a", "b"}) == forward {
		t.Fatal("dropping a member did not change the digest")
	}
}

// The attestation registry validator must reject an incomplete record.
func TestSchemaAttestationRegistryFailsClosed(t *testing.T) {
	base := loadAttestationRegistry(t)
	for name, mutate := range map[string]func(*SchemaAttestationRegistry){
		"wrong version": func(r *SchemaAttestationRegistry) { r.AttestationVersion = "v0" },
		"duplicate profile": func(r *SchemaAttestationRegistry) {
			r.Profiles = append(append([]SchemaAttestation(nil), r.Profiles...), r.Profiles[0])
		},
		"non digest schema digest": func(r *SchemaAttestationRegistry) {
			r.Profiles = append([]SchemaAttestation(nil), r.Profiles...)
			r.Profiles[0].SchemaDigest = "not-a-digest"
		},
		"no reporting views": func(r *SchemaAttestationRegistry) {
			r.Profiles = append([]SchemaAttestation(nil), r.Profiles...)
			r.Profiles[0].ReportingViews = nil
		},
		"not generated from a fresh deployment": func(r *SchemaAttestationRegistry) {
			r.Profiles = append([]SchemaAttestation(nil), r.Profiles...)
			r.Profiles[0].GeneratedFromFreshDeployment = false
		},
		"missing tool digest": func(r *SchemaAttestationRegistry) {
			r.Profiles = append([]SchemaAttestation(nil), r.Profiles...)
			r.Profiles[0].SchemaDigestToolSHA256 = ""
		},
	} {
		registry := base
		mutate(&registry)
		if err := registry.Validate(); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}
