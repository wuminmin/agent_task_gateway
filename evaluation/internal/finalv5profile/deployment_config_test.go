package finalv5profile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepositoryProfileDeploymentOverridesAreClosedAndProfileScoped(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	config, err := ResolveProfileDeploymentConfig(filepath.Join(root, "config/profiles/registry.json"),
		filepath.Join(root, "config/profiles/deployment-overrides-v1.json"),
		"source/deployment-overrides-v1.json", concurrencyDeploymentProfile)
	if err != nil {
		t.Fatal(err)
	}
	if config.Environment["GATEWAY_CONNECTOR_MAX_CONNECTIONS"] != 32 ||
		config.Environment["GATEWAY_CONTROL_MAX_OPEN_CONNECTIONS"] != 32 {
		t.Fatalf("concurrency deployment environment = %#v", config.Environment)
	}
	ordinary, err := ResolveProfileDeploymentConfig(filepath.Join(root, "config/profiles/registry.json"),
		filepath.Join(root, "config/profiles/deployment-overrides-v1.json"),
		"source/deployment-overrides-v1.json", "analytics-orders")
	if err != nil {
		t.Fatal(err)
	}
	if len(ordinary.Environment) != 0 {
		t.Fatalf("ordinary profile inherited deployment overrides: %#v", ordinary.Environment)
	}
}

func TestProfileDeploymentOverridesRejectUnknownProfileAndUncontrolledValues(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	registry := filepath.Join(root, "config/profiles/registry.json")
	source := filepath.Join(root, "config/profiles/deployment-overrides-v1.json")
	if _, err := ResolveProfileDeploymentConfig(registry, source, "source/deployment-overrides-v1.json", "unknown-profile"); err == nil || !strings.Contains(err.Error(), "unknown profile") {
		t.Fatalf("unknown profile error = %v", err)
	}
	payload, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	var overrides map[string]any
	if err := json.Unmarshal(payload, &overrides); err != nil {
		t.Fatal(err)
	}
	profiles := overrides["profiles"].(map[string]any)
	profile := profiles[concurrencyDeploymentProfile].(map[string]any)
	environment := profile["environment"].(map[string]any)
	// 33 satisfies the executable lower bound, but is not the exact value
	// selected by this source-controlled deployment contract.
	environment["GATEWAY_CONNECTOR_MAX_CONNECTIONS"] = float64(33)
	mutated, err := json.Marshal(overrides)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "deployment-overrides-v1.json")
	if err := os.WriteFile(path, mutated, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveProfileDeploymentConfig(registry, path, "source/deployment-overrides-v1.json", concurrencyDeploymentProfile); err == nil || !strings.Contains(err.Error(), "source-controlled minimum 32") {
		t.Fatalf("uncontrolled capacity error = %v", err)
	}
}
