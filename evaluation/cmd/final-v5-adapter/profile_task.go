package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
	"taskbound.local/agent-data-gateway/internal/catalog"
)

const (
	adapterProfileAliasEnv       = "TASKGATE_FINAL_V5_PROFILE_ALIAS"
	adapterProfileRegistryEnv    = "TASKGATE_FINAL_V5_PROFILE_REGISTRY"
	adapterProfileRegistrySHAEnv = "TASKGATE_FINAL_V5_PROFILE_REGISTRY_SHA256"
	adapterRepositoryRootEnv     = "TASKGATE_FINAL_V5_REPO_ROOT"
	adapterLiveCatalogEnv        = "TASKGATE_FINAL_V5_CATALOG"
)

// resolveScaleProfileTask derives the authorization surface from the activated
// profile, then proves that the frozen Scale binding describes that exact
// surface. The publication binding remains the query/oracle contract; it is no
// longer the source of the Product set sent to request_data_task.
func resolveScaleProfileTask(operation experiment.AdapterOperation, frozen boundTaskRequest) (boundTaskRequest, error) {
	root := strings.TrimSpace(os.Getenv(adapterRepositoryRootEnv))
	registryPath := strings.TrimSpace(os.Getenv(adapterProfileRegistryEnv))
	liveCatalogPath := strings.TrimSpace(os.Getenv(adapterLiveCatalogEnv))
	alias := strings.TrimSpace(os.Getenv(adapterProfileAliasEnv))
	if root == "" || registryPath == "" || liveCatalogPath == "" || alias == "" {
		return boundTaskRequest{}, errors.New("Scale profile task derivation requires repository, registry, alias, and live Catalog inputs")
	}
	return deriveProfileBoundTask(root, registryPath, liveCatalogPath, alias, operation, frozen)
}

func deriveProfileBoundTask(root, registryPath, liveCatalogPath, alias string,
	operation experiment.AdapterOperation, frozen boundTaskRequest) (boundTaskRequest, error) {
	registryBytes, err := os.ReadFile(registryPath)
	if err != nil {
		return boundTaskRequest{}, fmt.Errorf("read profile registry: %w", err)
	}
	if expected := strings.TrimSpace(os.Getenv(adapterProfileRegistrySHAEnv)); expected != "" {
		observed := sha(string(registryBytes))
		if !validDigest(expected) || observed != expected {
			return boundTaskRequest{}, errors.New("profile registry bytes differ from the pre-start SHA-256 binding")
		}
	}
	var registry finalv5profile.Registry
	if err := strictProfileRegistry(registryBytes, &registry); err != nil {
		return boundTaskRequest{}, fmt.Errorf("decode profile registry: %w", err)
	}
	if registry.SchemaVersion != 1 || registry.RegistryVersion != finalv5profile.RegistryVersion {
		return boundTaskRequest{}, errors.New("profile registry header is invalid")
	}
	var matches []finalv5profile.Profile
	for _, profile := range registry.Profiles {
		if profile.Alias == alias {
			matches = append(matches, profile)
		}
	}
	if len(matches) != 1 {
		return boundTaskRequest{}, fmt.Errorf("profile registry has %d entries for alias %q", len(matches), alias)
	}
	profile := matches[0]
	coordinate := strings.Join([]string{operation.ExperimentID, operation.WorkloadID, operation.Scale, operation.Mode}, "/")
	if !containsString(profile.Experiments, operation.ExperimentID) || !containsString(profile.Cells, coordinate) {
		return boundTaskRequest{}, fmt.Errorf("profile %s does not own Scale cell %s", alias, coordinate)
	}
	if profile.CatalogPath == "" || !validDigest(profile.CatalogSHA256) || len(profile.Closure.Products) == 0 {
		return boundTaskRequest{}, errors.New("profile registry entry omits its Catalog or Product closure identity")
	}

	expectedCatalogPath := filepath.Join(root, filepath.FromSlash(profile.CatalogPath))
	expectedInfo, err := os.Lstat(expectedCatalogPath)
	if err != nil || !expectedInfo.Mode().IsRegular() || expectedInfo.Mode()&os.ModeSymlink != 0 {
		return boundTaskRequest{}, errors.New("profile registry Catalog path is absent or unsafe")
	}
	liveInfo, err := os.Lstat(liveCatalogPath)
	if err != nil || !liveInfo.Mode().IsRegular() || liveInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(expectedInfo, liveInfo) {
		return boundTaskRequest{}, errors.New("live Catalog path differs from the profile registry catalog_path")
	}
	logical, err := catalog.Load(expectedCatalogPath)
	if err != nil {
		return boundTaskRequest{}, fmt.Errorf("load profile Catalog: %w", err)
	}
	if logical.SHA256 != profile.CatalogSHA256 {
		return boundTaskRequest{}, errors.New("profile Catalog bytes differ from the registry digest")
	}
	products := append([]string(nil), profile.Closure.Products...)
	policy, err := logical.ResolveTaskPolicy(products)
	if err != nil {
		return boundTaskRequest{}, fmt.Errorf("profile Product closure has no compatible approval route: %w", err)
	}
	if string(policy.ApprovalRoute.Mode) != "manual" || policy.ApprovalRoute.Approver != "bob" {
		return boundTaskRequest{}, errors.New("profile Product closure does not use the campaign's manual bob approval route")
	}
	columns := make(map[string][]string, len(policy.Products))
	requiredScopes := map[string]bool{}
	for _, product := range policy.Products {
		if len(product.Fields) == 0 {
			return boundTaskRequest{}, fmt.Errorf("profile Product %s declares no fields", product.Name)
		}
		columns[product.Name] = product.FieldNames()
		for _, scope := range product.Scopes {
			requiredScopes[scope] = true
		}
	}
	scopes := make(map[string][]string, len(requiredScopes))
	for name := range requiredScopes {
		definition, found := adapterCatalogScope(logical, name)
		if !found || definition.Type != catalog.ScopeTypeEnum || len(definition.AllowedValues) == 0 {
			return boundTaskRequest{}, fmt.Errorf("profile scope %s is not a closed non-empty enum", name)
		}
		scopes[name] = append([]string(nil), definition.AllowedValues...)
	}
	if !reflect.DeepEqual(frozen.DataProducts, products) || !reflect.DeepEqual(frozen.Columns, columns) ||
		!reflect.DeepEqual(frozen.Scopes, scopes) {
		return boundTaskRequest{}, errors.New("frozen Scale task differs from the registry/Catalog-derived authorization closure")
	}
	derived := frozen
	derived.DataProducts, derived.Columns, derived.Scopes = products, columns, scopes
	if err := validateBoundTask(derived); err != nil {
		return boundTaskRequest{}, fmt.Errorf("validate derived Scale task: %w", err)
	}
	return derived, nil
}

func strictProfileRegistry(payload []byte, registry *finalv5profile.Registry) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(registry); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return fmt.Errorf("trailing JSON: %w", err)
	}
	return nil
}

func adapterCatalogScope(logical *catalog.Catalog, name string) (catalog.Scope, bool) {
	for _, scope := range logical.Scopes {
		if scope.Name == name {
			return scope, true
		}
	}
	return catalog.Scope{}, false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
