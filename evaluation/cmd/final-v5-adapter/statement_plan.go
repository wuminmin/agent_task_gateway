package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/catalogschema"
)

// The closed-world statement accounting compares each class of gateway_reader
// statement with a multiplicity derived from the activated profile. The
// per-entry attestation classes scale with E, the exact number of ordered
// ExpectedSchema entries the Connector holds.
//
// E is resolved here, once per served Catalog, because it belongs to the
// deployment rather than to any one experiment, and it is resolved through the
// same shared builder cmd/gateway calls at startup so the two cannot disagree.

var (
	expectedSchemaOnce  sync.Once
	expectedSchemaValue catalogschema.Result
	expectedSchemaFornn string
	expectedSchemaErr   error
)

// servedExpectedSchema is the exact ordered ExpectedSchema for the Catalog the
// Gateway signed into the Receipt, carrying both the entry count E and its
// canonical digest.
//
// It is resolved by finding the registry profile that pins exactly that Catalog
// digest and building from its bytes. Both steps are observations: a Catalog no
// profile pins, or a Catalog file whose bytes hash to something else, yields
// nothing rather than a guess.
func servedExpectedSchema(catalogSHA256 string) (catalogschema.Result, error) {
	digest := strings.TrimSpace(catalogSHA256)
	if len(digest) != 64 {
		return catalogschema.Result{}, errors.New("a served Catalog digest is required to derive the control multiplicity")
	}
	expectedSchemaOnce.Do(func() {
		expectedSchemaFornn = digest
		expectedSchemaValue, expectedSchemaErr = resolveExpectedSchema(digest)
	})
	if expectedSchemaErr != nil {
		return catalogschema.Result{}, expectedSchemaErr
	}
	// The derivation is cached for the run, so it is only reusable while the
	// deployment is still serving the Catalog it came from.
	if expectedSchemaFornn != digest {
		return catalogschema.Result{}, fmt.Errorf("the deployment served Catalog %s after the ExpectedSchema was derived from %s",
			digest, expectedSchemaFornn)
	}
	return expectedSchemaValue, nil
}

func resolveExpectedSchema(catalogSHA256 string) (catalogschema.Result, error) {
	registryPath := strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_PROFILE_REGISTRY"))
	if registryPath == "" {
		return catalogschema.Result{}, errors.New("TASKGATE_FINAL_V5_PROFILE_REGISTRY is required to derive the control multiplicity")
	}
	payload, err := os.ReadFile(registryPath)
	if err != nil {
		return catalogschema.Result{}, fmt.Errorf("read profile registry: %w", err)
	}
	var registry finalv5profile.Registry
	if err := json.Unmarshal(payload, &registry); err != nil {
		return catalogschema.Result{}, fmt.Errorf("decode profile registry: %w", err)
	}
	var matched []finalv5profile.Profile
	for _, candidate := range registry.Profiles {
		if candidate.CatalogSHA256 == catalogSHA256 {
			matched = append(matched, candidate)
		}
	}
	if len(matched) == 0 {
		return catalogschema.Result{}, fmt.Errorf("no registry profile pins the served Catalog %s", catalogSHA256)
	}
	// Several profiles may legitimately share one Catalog. They must then derive
	// the same ExpectedSchema, because the Connector holds one schema
	// expectation per deployment; disagreement means the registry and the
	// Catalog have drifted apart and no multiplicity is derivable.
	built, err := expectedSchemaFor(matched[0])
	if err != nil {
		return catalogschema.Result{}, err
	}
	for _, profile := range matched[1:] {
		other, err := expectedSchemaFor(profile)
		if err != nil {
			return catalogschema.Result{}, err
		}
		if other.Digest != built.Digest {
			return catalogschema.Result{}, fmt.Errorf("profiles %s and %s pin one Catalog but derive different ExpectedSchema",
				matched[0].Alias, profile.Alias)
		}
	}
	return built, nil
}

// expectedSchemaFor derives the exact ordered ExpectedSchema one profile's
// Catalog produces, through the same shared builder the Gateway calls at
// startup.
//
// This replaces the contracts v1.4 derivation, which counted *distinct
// reporting views*. That was wrong: the builder appends one entry per governed
// Product without deduplication, so a Catalog with two Products on one view
// holds two ExpectedSchema entries and performs two attestation passes. The
// unique-view map undercounted exactly those Catalogs.
func expectedSchemaFor(profile finalv5profile.Profile) (catalogschema.Result, error) {
	payload, err := os.ReadFile(profile.CatalogPath)
	if err != nil {
		return catalogschema.Result{}, fmt.Errorf("read profile Catalog: %w", err)
	}
	source, err := catalog.Parse(payload)
	if err != nil {
		return catalogschema.Result{}, fmt.Errorf("parse profile Catalog: %w", err)
	}
	// Deriving from a file that is not the Catalog the registry pinned would
	// turn the derivation back into a declaration.
	if source.SHA256 != profile.CatalogSHA256 {
		return catalogschema.Result{}, fmt.Errorf("profile Catalog at %s hashes to %s, registry pins %s",
			profile.CatalogPath, source.SHA256, profile.CatalogSHA256)
	}
	return catalogschema.Build(source)
}
