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
	if len(config.Environment) != 4 ||
		config.Environment["GATEWAY_EVALUATION_CONCURRENCY_HTTP_ACTIVE"] != 10 ||
		config.Environment["GATEWAY_EVALUATION_CONCURRENCY_HTTP_QUEUE"] != 512 ||
		config.Environment["GATEWAY_CONNECTOR_MAX_CONNECTIONS"] != 32 ||
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
	tests := []struct {
		name    string
		mutate  func(map[string]any)
		message string
	}{
		{
			name: "connector wider than the fixed contract",
			mutate: func(environment map[string]any) {
				environment["GATEWAY_CONNECTOR_MAX_CONNECTIONS"] = float64(33)
			},
			message: "GATEWAY_CONNECTOR_MAX_CONNECTIONS must equal the source-controlled value 32",
		},
		{
			name: "active window drift",
			mutate: func(environment map[string]any) {
				environment["GATEWAY_EVALUATION_CONCURRENCY_HTTP_ACTIVE"] = float64(11)
			},
			message: "GATEWAY_EVALUATION_CONCURRENCY_HTTP_ACTIVE must equal the source-controlled value 10",
		},
		{
			name: "queue drift",
			mutate: func(environment map[string]any) {
				environment["GATEWAY_EVALUATION_CONCURRENCY_HTTP_QUEUE"] = float64(490)
			},
			message: "GATEWAY_EVALUATION_CONCURRENCY_HTTP_QUEUE must equal the source-controlled value 512",
		},
		{
			name: "missing active window",
			mutate: func(environment map[string]any) {
				delete(environment, "GATEWAY_EVALUATION_CONCURRENCY_HTTP_ACTIVE")
			},
			message: "complete closed capacity set",
		},
		{
			name: "unknown key",
			mutate: func(environment map[string]any) {
				delete(environment, "GATEWAY_CONTROL_MAX_OPEN_CONNECTIONS")
				environment["GATEWAY_EVALUATION_CONCURRENCY_UNKNOWN"] = float64(1)
			},
			message: "unsupported key",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var overrides map[string]any
			if err := json.Unmarshal(payload, &overrides); err != nil {
				t.Fatal(err)
			}
			profiles := overrides["profiles"].(map[string]any)
			profile := profiles[concurrencyDeploymentProfile].(map[string]any)
			environment := profile["environment"].(map[string]any)
			test.mutate(environment)
			mutated, err := json.Marshal(overrides)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "deployment-overrides-v1.json")
			if err := os.WriteFile(path, mutated, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ResolveProfileDeploymentConfig(registry, path, "source/deployment-overrides-v1.json", concurrencyDeploymentProfile); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("uncontrolled capacity error = %v, want %q", err, test.message)
			}
		})
	}
}
