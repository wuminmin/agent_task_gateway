package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
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
	binding, err := experiment.ResolveProfileBinding(registryPath, artifactProfileAlias,
		strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_DATASET_BINDING_SHA256")))
	if err != nil {
		return nil, err
	}
	// The Gateway must be serving exactly the profile Catalog the registry
	// pinned. Comparing against the Receipt's Catalog digest is what makes this
	// an observation rather than a declaration.
	if binding.CatalogSHA256 != catalogSHA256 {
		return nil, fmt.Errorf("the deployment serves Catalog %s, profile %s pins %s",
			catalogSHA256, artifactProfileAlias, binding.CatalogSHA256)
	}
	return binding, nil
}
