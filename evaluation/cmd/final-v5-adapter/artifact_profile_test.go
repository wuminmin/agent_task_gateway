package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
)

const (
	testArtifactCatalog = "533837084c0df141a0fac6e74788a4c2b9eb84611c1f96d4c760806745b4f709"
	testArtifactClosure = "a86cd4df5cad6e2685d1930caeccdbe3a865194fcef33faeca86b7c2c592bdbd"
)

func writeTestRegistry(t *testing.T, mutate func(*finalv5profile.Profile)) string {
	t.Helper()
	profile := finalv5profile.Profile{ID: finalv5profile.ProfileID(testArtifactClosure),
		Alias:         artifactProfileAlias,
		Closure:       finalv5profile.Closure{SHA256: testArtifactClosure, Publications: []string{"final-v5-result-heavy-v1"}},
		CatalogSHA256: testArtifactCatalog, TargetedRunEligible: true,
		Status: finalv5profile.ProfileStatus{ClosureComplete: true, CatalogMaterializable: true,
			LiveRouteAvailable: true, ActivationSupported: true, ActivationSmokePassed: true}}
	if mutate != nil {
		mutate(&profile)
	}
	payload, err := json.Marshal(finalv5profile.Registry{SchemaVersion: 1,
		RegistryVersion: finalv5profile.RegistryVersion,
		Profiles:        []finalv5profile.Profile{profile}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestArtifactProfileBindingIsDerivedFromTheClearedRegistryProfile(t *testing.T) {
	t.Setenv("TASKGATE_FINAL_V5_PROFILE_REGISTRY", writeTestRegistry(t, nil))
	t.Setenv("TASKGATE_FINAL_V5_DATASET_BINDING_SHA256", strings.Repeat("a", 64))

	binding, err := resolveArtifactProfileBinding(testArtifactCatalog)
	if err != nil {
		t.Fatalf("a cleared profile produced no binding: %v", err)
	}
	shared, err := experiment.ResolveProfileBinding(os.Getenv("TASKGATE_FINAL_V5_PROFILE_REGISTRY"),
		artifactProfileAlias, os.Getenv("TASKGATE_FINAL_V5_DATASET_BINDING_SHA256"))
	if err != nil {
		t.Fatalf("the shared resolver rejected the same inputs: %v", err)
	}
	if !binding.Equal(*shared) {
		t.Fatalf("adapter binding %+v differs from shared resolver %+v", *binding, *shared)
	}
	if binding.ProfileID != finalv5profile.ProfileID(testArtifactClosure) ||
		binding.ClosureSHA256 != testArtifactClosure || binding.CatalogSHA256 != testArtifactCatalog {
		t.Fatalf("binding identity = %+v", binding)
	}
	expected, err := experiment.CanonicalPublicationSetSHA256([]string{"final-v5-result-heavy-v1"})
	if err != nil {
		t.Fatal(err)
	}
	if binding.PublicationIdentity != expected {
		t.Fatal("publication identity is not the canonical Publication-set digest")
	}
	if err := experiment.RequireProfileBinding(experiment.Sample{ProfileBinding: binding}); err != nil {
		t.Fatalf("the derived binding does not satisfy the finalizer: %v", err)
	}
}

// An Artifact targeted run must not be able to bootstrap the readiness it
// depends on. A profile the registry has not cleared produces no binding, so no
// sample can be finalized.
func TestArtifactProfileBindingFailsClosed(t *testing.T) {
	for name, testCase := range map[string]struct {
		mutate  func(*finalv5profile.Profile)
		catalog string
		dataset string
	}{
		"profile not targeted-run eligible": {
			mutate:  func(p *finalv5profile.Profile) { p.TargetedRunEligible = false },
			catalog: testArtifactCatalog, dataset: strings.Repeat("a", 64)},
		"profile has no activation smoke": {
			mutate: func(p *finalv5profile.Profile) {
				p.Status.ActivationSupported = false
			},
			catalog: testArtifactCatalog, dataset: strings.Repeat("a", 64)},
		"deployment serves a different Catalog": {
			mutate: nil, catalog: strings.Repeat("b", 64), dataset: strings.Repeat("a", 64)},
		"no dataset binding digest": {
			mutate: nil, catalog: testArtifactCatalog, dataset: ""},
		"malformed dataset binding digest": {
			mutate: nil, catalog: testArtifactCatalog, dataset: "short"},
		"profile absent from the registry": {
			mutate:  func(p *finalv5profile.Profile) { p.Alias = "something-else" },
			catalog: testArtifactCatalog, dataset: strings.Repeat("a", 64)},
		"no publications in the closure": {
			mutate:  func(p *finalv5profile.Profile) { p.Closure.Publications = nil },
			catalog: testArtifactCatalog, dataset: strings.Repeat("a", 64)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("TASKGATE_FINAL_V5_PROFILE_REGISTRY", writeTestRegistry(t, testCase.mutate))
			t.Setenv("TASKGATE_FINAL_V5_DATASET_BINDING_SHA256", testCase.dataset)
			if _, err := resolveArtifactProfileBinding(testCase.catalog); err == nil {
				t.Fatalf("%s produced a profile binding", name)
			}
		})
	}
}

// Without the registry there is no binding at all: the adapter cannot fall back
// to an assumed identity.
func TestArtifactProfileBindingRequiresTheRegistry(t *testing.T) {
	t.Setenv("TASKGATE_FINAL_V5_PROFILE_REGISTRY", "")
	t.Setenv("TASKGATE_FINAL_V5_DATASET_BINDING_SHA256", strings.Repeat("a", 64))
	if _, err := resolveArtifactProfileBinding(testArtifactCatalog); err == nil {
		t.Fatal("a missing profile registry produced a binding")
	}
}

func TestArtifactProfileBindingCacheRejectsCatalogDrift(t *testing.T) {
	artifactProfileOnce = sync.Once{}
	artifactProfileBinding = nil
	artifactProfileErr = nil
	t.Cleanup(func() {
		artifactProfileOnce = sync.Once{}
		artifactProfileBinding = nil
		artifactProfileErr = nil
	})
	t.Setenv("TASKGATE_FINAL_V5_PROFILE_REGISTRY", writeTestRegistry(t, nil))
	t.Setenv("TASKGATE_FINAL_V5_DATASET_BINDING_SHA256", strings.Repeat("a", 64))

	first, err := artifactProfileBindingFor(testArtifactCatalog)
	if err != nil {
		t.Fatalf("resolve the first Catalog: %v", err)
	}
	// Callers receive a copy, so mutating one sample cannot corrupt the cached
	// deployment binding used by later cells.
	first.ProfileID = "profile-0000000000000000"
	second, err := artifactProfileBindingFor(testArtifactCatalog)
	if err != nil {
		t.Fatalf("reuse the unchanged Catalog: %v", err)
	}
	if second.ProfileID != finalv5profile.ProfileID(testArtifactClosure) {
		t.Fatal("a caller mutated the cached deployment binding")
	}

	if _, err := artifactProfileBindingFor(strings.Repeat("b", 64)); err == nil ||
		!strings.Contains(err.Error(), "Catalog digest changed") {
		t.Fatalf("mid-run Catalog drift error = %v", err)
	}
}
