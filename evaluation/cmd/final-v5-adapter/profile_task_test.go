package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5binding"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
)

func scaleProfileTestPaths(t *testing.T) (string, string, string) {
	t.Helper()
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	return root, filepath.Join(root, "config/profiles/registry.json"),
		filepath.Join(root, "config/profiles/exposure-scale.catalog.yaml")
}

func scaleProfileTestOperation() experiment.AdapterOperation {
	return experiment.AdapterOperation{
		ExperimentID: "scale", WorkloadID: "dependency-e2e",
		Scale: "10k-overlap-0", Mode: "novel",
	}
}

func TestScaleTaskAuthorizationComesFromExactProfileClosure(t *testing.T) {
	root, registryPath, catalogPath := scaleProfileTestPaths(t)
	registryBytes, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(adapterRepositoryRootEnv, root)
	t.Setenv(adapterProfileRegistryEnv, registryPath)
	t.Setenv(adapterLiveCatalogEnv, catalogPath)
	t.Setenv(adapterProfileAliasEnv, "exposure-scale")
	t.Setenv(adapterProfileRegistrySHAEnv, sha(string(registryBytes)))

	frozen := finalv5binding.FixedScaleTask()
	derived, err := resolveScaleProfileTask(scaleProfileTestOperation(), frozen)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(derived, frozen) {
		t.Fatalf("derived profile closure changed the frozen query/oracle contract: got %+v want %+v", derived, frozen)
	}
}

func TestScaleTaskAuthorizationHasNoFrozenProductFallback(t *testing.T) {
	root, registryPath, catalogPath := scaleProfileTestPaths(t)
	registryBytes, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	var registry finalv5profile.Registry
	if err := json.Unmarshal(registryBytes, &registry); err != nil {
		t.Fatal(err)
	}
	mutated := false
	for index := range registry.Profiles {
		if registry.Profiles[index].Alias == "exposure-scale" {
			registry.Profiles[index].Closure.Products = []string{"profile_closure_negative_control"}
			mutated = true
		}
	}
	if !mutated {
		t.Fatal("exposure-scale profile is absent")
	}
	payload, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	mutatedRegistry := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(mutatedRegistry, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(adapterProfileRegistrySHAEnv, "")

	_, err = deriveProfileBoundTask(root, mutatedRegistry, catalogPath, "exposure-scale", scaleProfileTestOperation(), finalv5binding.FixedScaleTask())
	if err == nil || !strings.Contains(err.Error(), "profile Product closure has no compatible approval route") {
		t.Fatalf("frozen task unexpectedly repaired a bad profile closure: %v", err)
	}
}
