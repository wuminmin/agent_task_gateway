package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
)

// The Artifact cells run on the Result-heavy profile. Every sample therefore
// has to name the profile it ran against, so a later reader can prove the six
// cells all executed on the same activated closure, Catalog, Dataset binding
// and Publication set -- and that it was a profile a live activation smoke had
// actually cleared.
//
// The binding is derived from the source-controlled registry and the operator's
// declared run inputs. Nothing here invents an identity: a registry that does
// not clear the profile, or a Catalog whose bytes differ from the one the
// registry pinned, leaves the adapter unable to produce a binding at all.
const artifactProfileAlias = "result-heavy"

var (
	artifactProfileOnce    sync.Once
	artifactProfileBinding *experiment.ProfileBinding
	artifactProfileErr     error
)

// artifactProfileBindingFor resolves the Result-heavy ProfileBinding once per
// process and reuses it for every cell, so all six samples are provably bound
// to one activation rather than to six independently asserted identities.
func artifactProfileBindingFor(catalogSHA256 string) (*experiment.ProfileBinding, error) {
	artifactProfileOnce.Do(func() {
		artifactProfileBinding, artifactProfileErr = resolveArtifactProfileBinding(catalogSHA256)
	})
	if artifactProfileErr != nil {
		return nil, artifactProfileErr
	}
	// Guard against a mid-run Catalog change: the cached binding is only valid
	// for the Catalog the deployment is still serving.
	if artifactProfileBinding.CatalogSHA256 != catalogSHA256 {
		return nil, errors.New("the live Catalog digest changed during the artifact run")
	}
	binding := *artifactProfileBinding
	return &binding, nil
}

func resolveArtifactProfileBinding(catalogSHA256 string) (*experiment.ProfileBinding, error) {
	registryPath := strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_PROFILE_REGISTRY"))
	if registryPath == "" {
		return nil, errors.New("TASKGATE_FINAL_V5_PROFILE_REGISTRY is required for an artifact run")
	}
	payload, err := os.ReadFile(registryPath)
	if err != nil {
		return nil, fmt.Errorf("read profile registry: %w", err)
	}
	var registry finalv5profile.Registry
	if err := json.Unmarshal(payload, &registry); err != nil {
		return nil, fmt.Errorf("decode profile registry: %w", err)
	}
	var profile finalv5profile.Profile
	found := false
	for _, candidate := range registry.Profiles {
		if candidate.Alias == artifactProfileAlias {
			profile, found = candidate, true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("profile registry has no %q profile", artifactProfileAlias)
	}
	// A targeted run may only execute a profile the registry has cleared for
	// one. This is the gate that stops an Artifact run from being used to
	// bootstrap the very readiness it depends on.
	if !profile.TargetedRunEligible {
		return nil, fmt.Errorf("profile %s is not targeted-run eligible: %+v", profile.Alias, profile.Status)
	}
	if !profile.Status.ActivationSupported {
		return nil, fmt.Errorf("profile %s has no recorded live activation smoke", profile.Alias)
	}
	// The Gateway must be serving exactly the profile Catalog the registry
	// pinned. Comparing against the Receipt's Catalog digest is what makes this
	// an observation rather than a declaration.
	if profile.CatalogSHA256 != catalogSHA256 {
		return nil, fmt.Errorf("the deployment serves Catalog %s, profile %s pins %s",
			catalogSHA256, profile.Alias, profile.CatalogSHA256)
	}
	publicationIdentity, err := experiment.CanonicalPublicationSetSHA256(profile.Closure.Publications)
	if err != nil {
		return nil, err
	}
	datasetBindingSHA := strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_DATASET_BINDING_SHA256"))
	if len(datasetBindingSHA) != 64 {
		return nil, errors.New("TASKGATE_FINAL_V5_DATASET_BINDING_SHA256 must be the SHA-256 of the run's Dataset Binding")
	}
	binding := &experiment.ProfileBinding{Version: experiment.ProfileBindingVersion,
		ProfileID: profile.ID, ClosureSHA256: profile.Closure.SHA256,
		CatalogSHA256: profile.CatalogSHA256, DatasetBindingSHA256: datasetBindingSHA,
		PublicationIdentity: publicationIdentity}
	if err := binding.Validate(); err != nil {
		return nil, err
	}
	return binding, nil
}
