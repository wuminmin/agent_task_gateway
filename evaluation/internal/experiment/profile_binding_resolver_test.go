package experiment

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
)

const (
	resolverTestClosure = "a86cd4df5cad6e2685d1930caeccdbe3a865194fcef33faeca86b7c2c592bdbd"
	resolverTestCatalog = "533837084c0df141a0fac6e74788a4c2b9eb84611c1f96d4c760806745b4f709"
	resolverTestAlias   = "result-heavy"
)

func resolverTestRegistry() finalv5profile.Registry {
	status := finalv5profile.ProfileStatus{
		ClosureComplete:       true,
		CatalogMaterializable: true,
		LiveRouteAvailable:    true,
		ActivationSupported:   true,
		ActivationSmokePassed: true,
	}
	return finalv5profile.Registry{
		SchemaVersion:   1,
		RegistryVersion: finalv5profile.RegistryVersion,
		Profiles: []finalv5profile.Profile{{
			ID:                  finalv5profile.ProfileID(resolverTestClosure),
			Alias:               resolverTestAlias,
			Closure:             finalv5profile.Closure{SHA256: resolverTestClosure, Publications: []string{"z-publication", "a-publication"}},
			Status:              status,
			TargetedRunEligible: status.TargetedRunEligible(),
			CatalogSHA256:       resolverTestCatalog,
		}},
	}
}

func writeResolverRegistry(t *testing.T, registry finalv5profile.Registry) string {
	t.Helper()
	payload, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeResolverRegistryBytes(t *testing.T, payload []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveProfileBindingConstructsAllFields(t *testing.T) {
	registryPath := writeResolverRegistry(t, resolverTestRegistry())
	dataset := strings.Repeat("c", 64)
	binding, err := ResolveProfileBinding("  "+registryPath+"  ", "  "+resolverTestAlias+"  ", "  "+dataset+"  ")
	if err != nil {
		t.Fatalf("resolve a cleared profile: %v", err)
	}
	publicationIdentity, err := CanonicalPublicationSetSHA256([]string{"a-publication", "z-publication"})
	if err != nil {
		t.Fatal(err)
	}
	want := ProfileBinding{
		Version:              ProfileBindingVersion,
		ProfileID:            finalv5profile.ProfileID(resolverTestClosure),
		ClosureSHA256:        resolverTestClosure,
		CatalogSHA256:        resolverTestCatalog,
		DatasetBindingSHA256: dataset,
		PublicationIdentity:  publicationIdentity,
	}
	if !binding.Equal(want) {
		t.Fatalf("binding = %+v, want %+v", *binding, want)
	}
	if err := binding.Validate(); err != nil {
		t.Fatalf("resolved binding is invalid: %v", err)
	}
}

func TestResolveProfileBindingRejectsInvalidRegistryProfiles(t *testing.T) {
	tests := map[string]func(*finalv5profile.Registry){
		"unsupported registry version": func(registry *finalv5profile.Registry) {
			registry.RegistryVersion = "taskgate-final-v5-workload-closure-profile-v0"
		},
		"profile absent": func(registry *finalv5profile.Registry) {
			registry.Profiles[0].Alias = "another-profile"
		},
		"duplicate alias": func(registry *finalv5profile.Registry) {
			registry.Profiles = append(registry.Profiles, registry.Profiles[0])
		},
		"not targeted-run eligible": func(registry *finalv5profile.Registry) {
			registry.Profiles[0].TargetedRunEligible = false
		},
		"activation unsupported": func(registry *finalv5profile.Registry) {
			registry.Profiles[0].Status.ActivationSupported = false
		},
		"eligibility precondition false": func(registry *finalv5profile.Registry) {
			registry.Profiles[0].Status.LiveRouteAvailable = false
		},
		"profile ID not closure-derived": func(registry *finalv5profile.Registry) {
			registry.Profiles[0].ID = "profile-0000000000000000"
		},
		"malformed closure digest": func(registry *finalv5profile.Registry) {
			registry.Profiles[0].Closure.SHA256 = "short"
		},
		"malformed Catalog digest": func(registry *finalv5profile.Registry) {
			registry.Profiles[0].CatalogSHA256 = "short"
		},
		"empty Publication set": func(registry *finalv5profile.Registry) {
			registry.Profiles[0].Closure.Publications = nil
		},
		"duplicate Publication": func(registry *finalv5profile.Registry) {
			registry.Profiles[0].Closure.Publications = []string{"same", "same"}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			registry := resolverTestRegistry()
			mutate(&registry)
			path := writeResolverRegistry(t, registry)
			if _, err := ResolveProfileBinding(path, resolverTestAlias, strings.Repeat("c", 64)); err == nil {
				t.Fatalf("%s registry produced a binding", name)
			}
		})
	}
}

func TestResolveProfileBindingRequiresPassedActivationSmoke(t *testing.T) {
	registry := resolverTestRegistry()
	// This is deliberately hand-inconsistent: ActivationSupported and the
	// derived eligibility inputs remain true. The evidence bit must still be a
	// separate, load-bearing clearance gate.
	registry.Profiles[0].Status.ActivationSmokePassed = false
	if !registry.Profiles[0].Status.ActivationSupported ||
		!registry.Profiles[0].Status.TargetedRunEligible() ||
		!registry.Profiles[0].TargetedRunEligible {
		t.Fatal("test fixture did not preserve the hand-inconsistent positive claims")
	}
	path := writeResolverRegistry(t, registry)
	if _, err := ResolveProfileBinding(path, resolverTestAlias, strings.Repeat("c", 64)); err == nil ||
		!strings.Contains(err.Error(), "no passing live activation smoke evidence") {
		t.Fatalf("activation_smoke_passed=false error = %v", err)
	}
}

func TestResolveProfileBindingStrictlyDecodesRegistry(t *testing.T) {
	valid, err := json.Marshal(resolverTestRegistry())
	if err != nil {
		t.Fatal(err)
	}
	unknownTopLevel := append([]byte(nil), valid[:len(valid)-1]...)
	unknownTopLevel = append(unknownTopLevel, []byte(`,"unexpected":true}`)...)
	unknownNested := bytes.Replace(valid, []byte(`"profile_id"`),
		[]byte(`"unexpected":true,"profile_id"`), 1)
	for name, payload := range map[string][]byte{
		"empty":                   nil,
		"malformed":               []byte(`{`),
		"unknown top-level field": unknownTopLevel,
		"unknown nested field":    unknownNested,
		"multiple JSON values":    append(append([]byte(nil), valid...), []byte(` {}`)...),
		"trailing garbage":        append(append([]byte(nil), valid...), '!'),
	} {
		t.Run(name, func(t *testing.T) {
			path := writeResolverRegistryBytes(t, payload)
			if _, err := ResolveProfileBinding(path, resolverTestAlias, strings.Repeat("c", 64)); err == nil {
				t.Fatalf("%s registry produced a binding", name)
			}
		})
	}
}

func TestResolveProfileBindingRequiresInputs(t *testing.T) {
	registryPath := writeResolverRegistry(t, resolverTestRegistry())
	dataset := strings.Repeat("c", 64)
	for name, input := range map[string]struct {
		registry, alias, dataset string
	}{
		"registry path":            {alias: resolverTestAlias, dataset: dataset},
		"profile alias":            {registry: registryPath, dataset: dataset},
		"dataset digest":           {registry: registryPath, alias: resolverTestAlias},
		"malformed dataset digest": {registry: registryPath, alias: resolverTestAlias, dataset: "short"},
		"unreadable registry":      {registry: filepath.Join(t.TempDir(), "absent.json"), alias: resolverTestAlias, dataset: dataset},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ResolveProfileBinding(input.registry, input.alias, input.dataset); err == nil {
				t.Fatalf("missing or malformed %s produced a binding", name)
			}
		})
	}
}
