package experiment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
)

// ResolveProfileBinding constructs the one deployment ProfileBinding named by
// alias. Both the orchestrator and the Adapter use this resolver so the
// canonical Publication-set digest and the registry clearance gates have one
// implementation.
//
// The live Catalog comparison remains the Adapter's responsibility: its value
// comes from the Gateway-signed Receipt, while this resolver only constructs
// the registry side of the binding.
func ResolveProfileBinding(registryPath, alias, datasetBindingSHA256 string) (*ProfileBinding, error) {
	registryPath = strings.TrimSpace(registryPath)
	if registryPath == "" {
		return nil, errors.New("profile registry path is required")
	}
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return nil, errors.New("profile alias is required")
	}

	payload, err := os.ReadFile(registryPath)
	if err != nil {
		return nil, fmt.Errorf("read profile registry: %w", err)
	}
	var registry finalv5profile.Registry
	if err := decodeProfileRegistry(payload, &registry); err != nil {
		return nil, fmt.Errorf("decode profile registry: %w", err)
	}
	if registry.SchemaVersion != 1 || registry.RegistryVersion != finalv5profile.RegistryVersion {
		return nil, errors.New("profile registry header is invalid")
	}

	matches := make([]finalv5profile.Profile, 0, 1)
	for _, candidate := range registry.Profiles {
		if candidate.Alias == alias {
			matches = append(matches, candidate)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("profile registry has no %q profile", alias)
	case 1:
	default:
		return nil, fmt.Errorf("profile registry has %d profiles with alias %q", len(matches), alias)
	}
	profile := matches[0]

	// A targeted run may only consume readiness established before the run. It
	// cannot use its own measurements to bootstrap the clearance it depends on.
	if !profile.TargetedRunEligible {
		return nil, fmt.Errorf("profile %s is not targeted-run eligible: %+v", profile.Alias, profile.Status)
	}
	if !profile.Status.ActivationSupported {
		return nil, fmt.Errorf("profile %s has no recorded live activation smoke", profile.Alias)
	}
	if !profile.Status.ActivationSmokePassed {
		return nil, fmt.Errorf("profile %s has no passing live activation smoke evidence", profile.Alias)
	}
	if !profile.Status.TargetedRunEligible() {
		return nil, fmt.Errorf("profile %s targeted-run eligibility disagrees with its preconditions", profile.Alias)
	}
	if profile.ID != finalv5profile.ProfileID(profile.Closure.SHA256) {
		return nil, fmt.Errorf("profile %s ID is not derived from its closure digest", profile.Alias)
	}

	publicationIdentity, err := CanonicalPublicationSetSHA256(profile.Closure.Publications)
	if err != nil {
		return nil, fmt.Errorf("profile %s Publication set: %w", profile.Alias, err)
	}
	binding := &ProfileBinding{
		Version:              ProfileBindingVersion,
		ProfileID:            profile.ID,
		ClosureSHA256:        profile.Closure.SHA256,
		CatalogSHA256:        profile.CatalogSHA256,
		DatasetBindingSHA256: strings.TrimSpace(datasetBindingSHA256),
		PublicationIdentity:  publicationIdentity,
	}
	if err := binding.Validate(); err != nil {
		return nil, fmt.Errorf("profile %s binding: %w", profile.Alias, err)
	}
	return binding, nil
}

func decodeProfileRegistry(payload []byte, registry *finalv5profile.Registry) error {
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
