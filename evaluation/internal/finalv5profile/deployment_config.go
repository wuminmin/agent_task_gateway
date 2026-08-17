package finalv5profile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/internal/concurrencyfixture"
)

const (
	ProfileDeploymentOverridesVersion = "taskgate-profile-deployment-overrides-v1"
	ProfileDeploymentConfigVersion    = "taskgate-profile-deployment-config-v1"
	concurrencyDeploymentProfile      = "concurrency-expense-detail"
)

var profileDeploymentEnvironmentValues = map[string]int64{
	"GATEWAY_EVALUATION_CONCURRENCY_HTTP_ACTIVE": int64(concurrencyfixture.ServiceActiveWindow),
	"GATEWAY_EVALUATION_CONCURRENCY_HTTP_QUEUE":  512,
	"GATEWAY_CONNECTOR_MAX_CONNECTIONS":          int64(concurrencyfixture.MinimumProductionPoolWidth),
	"GATEWAY_CONTROL_MAX_OPEN_CONNECTIONS":       int64(concurrencyfixture.MinimumProductionPoolWidth),
}

type ProfileDeploymentOverride struct {
	Environment map[string]int64 `json:"environment"`
}

type ProfileDeploymentOverrides struct {
	SchemaVersion int                                  `json:"schema_version"`
	Record        string                               `json:"record"`
	Profiles      map[string]ProfileDeploymentOverride `json:"profiles"`
}

// ProfileDeploymentConfig is the credential-free, retained record of the
// source-controlled environment applied before Compose resolves one profile.
type ProfileDeploymentConfig struct {
	SchemaVersion int              `json:"schema_version"`
	Record        string           `json:"record"`
	SourcePath    string           `json:"source_path"`
	SourceSHA256  string           `json:"source_sha256"`
	ProfileAlias  string           `json:"profile_alias"`
	Environment   map[string]int64 `json:"environment"`
}

func ResolveProfileDeploymentConfig(registryPath, overridesPath, retainedSourcePath, alias string) (ProfileDeploymentConfig, error) {
	var result ProfileDeploymentConfig
	if strings.TrimSpace(alias) == "" || strings.TrimSpace(retainedSourcePath) == "" ||
		filepath.IsAbs(retainedSourcePath) || filepath.Clean(retainedSourcePath) != retainedSourcePath {
		return result, errors.New("profile alias and safe retained source path are required")
	}
	registryPayload, err := readRegularProfileFile(registryPath)
	if err != nil {
		return result, fmt.Errorf("profile registry: %w", err)
	}
	var registry Registry
	if err := decodeProfileDeploymentJSON(registryPayload, &registry); err != nil {
		return result, fmt.Errorf("decode profile registry: %w", err)
	}
	aliases := make(map[string]bool, len(registry.Profiles))
	for _, profile := range registry.Profiles {
		if profile.Alias == "" || aliases[profile.Alias] {
			return result, errors.New("profile registry aliases are incomplete or duplicated")
		}
		aliases[profile.Alias] = true
	}
	if !aliases[alias] {
		return result, fmt.Errorf("profile deployment configuration names unknown profile %q", alias)
	}

	overridesPayload, err := readRegularProfileFile(overridesPath)
	if err != nil {
		return result, fmt.Errorf("profile deployment overrides: %w", err)
	}
	var overrides ProfileDeploymentOverrides
	if err := decodeProfileDeploymentJSON(overridesPayload, &overrides); err != nil {
		return result, fmt.Errorf("decode profile deployment overrides: %w", err)
	}
	if err := validateClosedProfileDeploymentOverrides(overrides); err != nil {
		return result, err
	}
	for profileAlias := range overrides.Profiles {
		if !aliases[profileAlias] {
			return result, fmt.Errorf("profile deployment overrides contain unknown profile %q", profileAlias)
		}
	}
	environment := map[string]int64{}
	if override, ok := overrides.Profiles[alias]; ok {
		for name, value := range override.Environment {
			environment[name] = value
		}
	}
	digest := sha256.Sum256(overridesPayload)
	return ProfileDeploymentConfig{SchemaVersion: 1, Record: ProfileDeploymentConfigVersion,
		SourcePath: retainedSourcePath, SourceSHA256: hex.EncodeToString(digest[:]),
		ProfileAlias: alias, Environment: environment}, nil
}

func validateClosedProfileDeploymentOverrides(overrides ProfileDeploymentOverrides) error {
	if overrides.SchemaVersion != 1 || overrides.Record != ProfileDeploymentOverridesVersion || len(overrides.Profiles) != 1 {
		return errors.New("profile deployment overrides header or closed profile set is invalid")
	}
	for profileAlias, override := range overrides.Profiles {
		if profileAlias != concurrencyDeploymentProfile {
			return fmt.Errorf("profile %q has no source-controlled deployment override contract", profileAlias)
		}
		if err := validateProfileDeploymentEnvironment(override.Environment); err != nil {
			return fmt.Errorf("profile %q: %w", profileAlias, err)
		}
	}
	return nil
}

func validateProfileDeploymentEnvironment(environment map[string]int64) error {
	if len(environment) != len(profileDeploymentEnvironmentValues) {
		return errors.New("deployment environment must contain the complete closed capacity set")
	}
	for name, value := range environment {
		expected, ok := profileDeploymentEnvironmentValues[name]
		if !ok {
			return fmt.Errorf("deployment environment contains unsupported key %q", name)
		}
		if value != expected {
			return fmt.Errorf("deployment environment %s must equal the source-controlled value %d", name, expected)
		}
	}
	return nil
}

func readRegularProfileFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("input must be a regular non-symlink file")
	}
	return os.ReadFile(path)
}

func decodeProfileDeploymentJSON(payload []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("JSON has trailing content")
	}
	return nil
}
