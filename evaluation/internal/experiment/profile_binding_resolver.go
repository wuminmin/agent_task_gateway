package experiment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
)

// TargetedProfileIdentity is the exact source-controlled identity and
// pre-measurement clearance of one profile registry entry.  Keeping this
// resolution separate from ProfileBinding lets a deployment binding acquire
// the profile identity before the deployment-binding file itself has a digest;
// ResolveProfileBinding then adds that digest without reimplementing any of the
// registry or clearance rules.
type TargetedProfileIdentity struct {
	RegistrySHA256        string
	ContractRelease       string
	ProfileID             string
	ProfileAlias          string
	ClosureSHA256         string
	CatalogPath           string
	CatalogSHA256         string
	PublicationIdentity   string
	Publications          []string
	WorkloadCells         []string
	ActivationSupported   bool
	ActivationSmokePassed bool
	TargetedRunEligible   bool
}

// ResolveTargetedProfileIdentity resolves one registry alias through the same
// fail-closed clearance gate used by every measured ProfileBinding.
func ResolveTargetedProfileIdentity(registryPath, alias string) (TargetedProfileIdentity, error) {
	var identity TargetedProfileIdentity
	registryPath = strings.TrimSpace(registryPath)
	if registryPath == "" {
		return identity, errors.New("profile registry path is required")
	}
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return identity, errors.New("profile alias is required")
	}

	payload, err := os.ReadFile(registryPath)
	if err != nil {
		return identity, fmt.Errorf("read profile registry: %w", err)
	}
	var registry finalv5profile.Registry
	if err := decodeProfileRegistry(payload, &registry); err != nil {
		return identity, fmt.Errorf("decode profile registry: %w", err)
	}
	if registry.SchemaVersion != 1 || registry.RegistryVersion != finalv5profile.RegistryVersion {
		return identity, errors.New("profile registry header is invalid")
	}

	matches := make([]finalv5profile.Profile, 0, 1)
	for _, candidate := range registry.Profiles {
		if candidate.Alias == alias {
			matches = append(matches, candidate)
		}
	}
	switch len(matches) {
	case 0:
		return identity, fmt.Errorf("profile registry has no %q profile", alias)
	case 1:
	default:
		return identity, fmt.Errorf("profile registry has %d profiles with alias %q", len(matches), alias)
	}
	profile := matches[0]

	// A targeted run may only consume readiness established before the run. It
	// cannot use its own measurements to bootstrap the clearance it depends on.
	if !profile.TargetedRunEligible {
		return identity, fmt.Errorf("profile %s is not targeted-run eligible: %+v", profile.Alias, profile.Status)
	}
	if !profile.Status.ActivationSupported {
		return identity, fmt.Errorf("profile %s has no recorded live activation smoke", profile.Alias)
	}
	if !profile.Status.ActivationSmokePassed {
		return identity, fmt.Errorf("profile %s has no passing live activation smoke evidence", profile.Alias)
	}
	if !profile.Status.TargetedRunEligible() {
		return identity, fmt.Errorf("profile %s targeted-run eligibility disagrees with its preconditions", profile.Alias)
	}
	if profile.ID != finalv5profile.ProfileID(profile.Closure.SHA256) {
		return identity, fmt.Errorf("profile %s ID is not derived from its closure digest", profile.Alias)
	}

	publicationIdentity, err := CanonicalPublicationSetSHA256(profile.Closure.Publications)
	if err != nil {
		return identity, fmt.Errorf("profile %s Publication set: %w", profile.Alias, err)
	}
	digest := sha256.Sum256(payload)
	return TargetedProfileIdentity{
		RegistrySHA256:        hex.EncodeToString(digest[:]),
		ContractRelease:       registry.ContractRelease,
		ProfileID:             profile.ID,
		ProfileAlias:          profile.Alias,
		ClosureSHA256:         profile.Closure.SHA256,
		CatalogPath:           profile.CatalogPath,
		CatalogSHA256:         profile.CatalogSHA256,
		PublicationIdentity:   publicationIdentity,
		Publications:          append([]string(nil), profile.Closure.Publications...),
		WorkloadCells:         append([]string(nil), profile.Cells...),
		ActivationSupported:   profile.Status.ActivationSupported,
		ActivationSmokePassed: profile.Status.ActivationSmokePassed,
		TargetedRunEligible:   profile.TargetedRunEligible,
	}, nil
}

// ResolveProfileBinding constructs the one deployment ProfileBinding named by
// alias. Both the orchestrator and the Adapter use this resolver so the
// canonical Publication-set digest and the registry clearance gates have one
// implementation.
//
// The live Catalog comparison remains the Adapter's responsibility: its value
// comes from the Gateway-signed Receipt, while this resolver only constructs
// the registry side of the binding.
func ResolveProfileBinding(registryPath, alias, datasetBindingSHA256 string) (*ProfileBinding, error) {
	identity, err := ResolveTargetedProfileIdentity(registryPath, alias)
	if err != nil {
		return nil, err
	}
	binding := &ProfileBinding{
		Version:              ProfileBindingVersion,
		ProfileID:            identity.ProfileID,
		ClosureSHA256:        identity.ClosureSHA256,
		CatalogSHA256:        identity.CatalogSHA256,
		DatasetBindingSHA256: strings.TrimSpace(datasetBindingSHA256),
		PublicationIdentity:  identity.PublicationIdentity,
	}
	if err := binding.Validate(); err != nil {
		return nil, fmt.Errorf("profile %s binding: %w", identity.ProfileAlias, err)
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
