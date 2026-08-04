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
)

// The closed-world statement accounting compares each class of gateway_reader
// statement with a multiplicity derived from the activated profile. Two
// quantities drive that derivation: how many governed transactions the operation
// settles, and how many reporting views the Catalog declares to the Connector.
//
// The first is a property of the operation and is stated at each call site. The
// second is resolved here, once per served Catalog, because it belongs to the
// deployment rather than to any one experiment: the Connector attests every view
// in its schema expectation inside every governed transaction, whichever
// workload issued it.

var (
	reportingViewsOnce  sync.Once
	reportingViewsValue int64
	reportingViewsFor   string
	reportingViewsErr   error
)

// servedReportingViewCount is the N in the derived control multiplicity
// T * (5 + 2 * N), for the Catalog the Gateway signed into the Receipt.
//
// It is resolved by finding the registry profile that pins exactly that Catalog
// digest and counting the reporting views its Catalog bytes declare. Both steps
// are observations: a Catalog no profile pins, or a Catalog file whose bytes
// hash to something else, yields no count at all rather than a guess.
func servedReportingViewCount(catalogSHA256 string) (int64, error) {
	digest := strings.TrimSpace(catalogSHA256)
	if len(digest) != 64 {
		return 0, errors.New("a served Catalog digest is required to derive the control multiplicity")
	}
	reportingViewsOnce.Do(func() {
		reportingViewsFor = digest
		reportingViewsValue, reportingViewsErr = resolveReportingViewCount(digest)
	})
	if reportingViewsErr != nil {
		return 0, reportingViewsErr
	}
	// The count is cached for the run, so it is only reusable while the
	// deployment is still serving the Catalog it was derived from.
	if reportingViewsFor != digest {
		return 0, fmt.Errorf("the deployment served Catalog %s after the control multiplicity was derived from %s",
			digest, reportingViewsFor)
	}
	return reportingViewsValue, nil
}

func resolveReportingViewCount(catalogSHA256 string) (int64, error) {
	registryPath := strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_PROFILE_REGISTRY"))
	if registryPath == "" {
		return 0, errors.New("TASKGATE_FINAL_V5_PROFILE_REGISTRY is required to derive the control multiplicity")
	}
	payload, err := os.ReadFile(registryPath)
	if err != nil {
		return 0, fmt.Errorf("read profile registry: %w", err)
	}
	var registry finalv5profile.Registry
	if err := json.Unmarshal(payload, &registry); err != nil {
		return 0, fmt.Errorf("decode profile registry: %w", err)
	}
	var matched []finalv5profile.Profile
	for _, candidate := range registry.Profiles {
		if candidate.CatalogSHA256 == catalogSHA256 {
			matched = append(matched, candidate)
		}
	}
	if len(matched) == 0 {
		return 0, fmt.Errorf("no registry profile pins the served Catalog %s", catalogSHA256)
	}
	// Several profiles may legitimately share one Catalog. They must then agree
	// on the reporting-view count, because the Connector holds one schema
	// expectation per deployment; disagreement means the registry and the
	// Catalog have drifted apart and no multiplicity is derivable.
	count, err := reportingViewCount(matched[0])
	if err != nil {
		return 0, err
	}
	for _, profile := range matched[1:] {
		other, err := reportingViewCount(profile)
		if err != nil {
			return 0, err
		}
		if other != count {
			return 0, fmt.Errorf("profiles %s and %s pin one Catalog but derive %d and %d reporting views",
				matched[0].Alias, profile.Alias, count, other)
		}
	}
	return count, nil
}

// reportingViewCount counts the reporting views one profile's Catalog declares
// to the Connector. Products carrying a ViewContract are excluded because the
// Connector holds no schema expectation for them, exactly as the profile
// attestation computes its own reporting-view set.
func reportingViewCount(profile finalv5profile.Profile) (int64, error) {
	payload, err := os.ReadFile(profile.CatalogPath)
	if err != nil {
		return 0, fmt.Errorf("read profile Catalog: %w", err)
	}
	source, err := catalog.Parse(payload)
	if err != nil {
		return 0, fmt.Errorf("parse profile Catalog: %w", err)
	}
	// Counting views from a file that is not the Catalog the registry pinned
	// would turn the derivation back into a declaration.
	if source.SHA256 != profile.CatalogSHA256 {
		return 0, fmt.Errorf("profile Catalog at %s hashes to %s, registry pins %s",
			profile.CatalogPath, source.SHA256, profile.CatalogSHA256)
	}
	views := map[string]bool{}
	for _, product := range source.Products {
		if product.ViewContract != nil {
			continue
		}
		if strings.TrimSpace(product.ReportingView) == "" {
			return 0, fmt.Errorf("Catalog product %s declares no reporting view", product.Name)
		}
		views[product.ReportingView] = true
	}
	if len(views) == 0 {
		return 0, errors.New("profile Catalog declares no attested reporting view")
	}
	return int64(len(views)), nil
}
