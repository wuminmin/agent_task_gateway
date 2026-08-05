package physicalquery

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/queryplan"
)

// Grant is the authorization material preparation reads.
//
// It is deliberately NOT control.TaskGrant. That type is declared beside the
// Control Store, so depending on it would put a store package in this one's
// import graph for the sake of six fields -- and it carries members preparation
// has no business seeing, such as the approval receipt and the budget. The
// Gateway maps its control.TaskGrant onto this; the finalizer maps its
// independently obtained frozen contract material onto the same type. Both then
// hand the same values to the same function.
type Grant struct {
	// ApprovedProducts and ApprovedColumns are the task's authorized surface.
	ApprovedProducts []string
	ApprovedColumns  map[string][]string
	// MandatoryScope is the scope predicate every generated statement carries.
	MandatoryScope json.RawMessage
	// ExposureProfile selects the accounting profile, and with it whether an
	// ordinal program, a predicate footprint or neither is prepared.
	ExposureProfile string
	// PredicateLimits bound the V5 predicate footprint. They are zero when the
	// profile does not produce one.
	PredicateLimits queryplan.PredicateLimits
	// ViewBindingDigest and ViewRegistryRevision pin the compiled semantic View
	// artifact this operation was authorized against, empty when it uses none.
	ViewBindingDigest    string
	ViewRegistryRevision string
}

// ExposureEnabled reports whether the grant accounts exposure at all.
func (grant Grant) ExposureEnabled() bool { return strings.TrimSpace(grant.ExposureProfile) != "" }

// UsesOrdinalProgram reports whether the profile compiles an ordinal program and
// binds snapshot sidecars. V4 introduced it and V5 kept it.
func (grant Grant) UsesOrdinalProgram() bool {
	return grant.ExposureProfile == exposure.ProfileV4 || grant.ExposureProfile == exposure.ProfileV5
}

// UsesPredicateFootprint reports whether the profile prepares a V5 predicate
// footprint.
func (grant Grant) UsesPredicateFootprint() bool { return grant.ExposureProfile == exposure.ProfileV5 }

// UsesNormalizedIdentity reports whether the profile requires the V2 canonical
// identity and normalized form. V2 introduced it and every later profile kept it.
func (grant Grant) UsesNormalizedIdentity() bool {
	switch grant.ExposureProfile {
	case exposure.ProfileV2, exposure.ProfileV3, exposure.ProfileV4, exposure.ProfileV5:
		return true
	default:
		return false
	}
}

// Validate rejects a grant that cannot settle a preparation deterministically.
func (grant Grant) Validate() error {
	if len(grant.ApprovedProducts) == 0 {
		return errors.New("preparation grant approves no data product")
	}
	seen := map[string]bool{}
	for _, product := range grant.ApprovedProducts {
		if strings.TrimSpace(product) == "" {
			return errors.New("preparation grant approves an unnamed data product")
		}
		if seen[product] {
			return fmt.Errorf("preparation grant lists product %q twice", product)
		}
		seen[product] = true
	}
	if grant.ExposureProfile != "" {
		switch grant.ExposureProfile {
		case exposure.ProfileV1, exposure.ProfileV2, exposure.ProfileV3,
			exposure.ProfileV4, exposure.ProfileV5:
		default:
			return fmt.Errorf("preparation grant names unknown exposure profile %q", grant.ExposureProfile)
		}
	}
	return nil
}

// SnapshotBinding is one already-verified immutable snapshot publication, as the
// caller resolved it.
//
// This is what replaces the Gateway's in-memory snapshot registry. The Gateway
// resolves it from its live registry and the finalizer from retained Profile and
// Publication evidence; each verifies the artifact against the Catalog BEFORE
// calling this package, and passes the verified digests in. Preparation then
// depends on values rather than on whoever happens to hold the registry.
type SnapshotBinding struct {
	// PublicationName is the Catalog publication this binding is for.
	PublicationName string
	// DictionaryDigest, ManifestDigest and SidecarDigest are the artifact's
	// identity as the caller resolved it. Prepare requires each to equal what the
	// Catalog declares, so a caller that resolved a different artifact fails here
	// rather than producing statements against it.
	DictionaryDigest string
	ManifestDigest   string
	SidecarDigest    string
	// SourceNamespace and Snapshot are the artifact's own source binding.
	SourceNamespace string
	Snapshot        string
}

// Validate rejects a binding that leaves the artifact ambiguous.
func (binding SnapshotBinding) Validate() error {
	for name, value := range map[string]string{
		"publication name":  binding.PublicationName,
		"dictionary digest": binding.DictionaryDigest,
		"manifest digest":   binding.ManifestDigest,
		"sidecar digest":    binding.SidecarDigest,
		"source namespace":  binding.SourceNamespace,
		"snapshot":          binding.Snapshot,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("snapshot binding for %q carries no %s", binding.PublicationName, name)
		}
	}
	return nil
}

// RequireCatalog rejects a binding that does not describe the Catalog's own
// publication. It is the check that makes "the caller resolved the artifact"
// mean "the caller resolved THIS artifact".
func (binding SnapshotBinding) RequireCatalog(publication catalog.SnapshotPublication) error {
	if err := binding.Validate(); err != nil {
		return err
	}
	for _, pair := range []struct {
		name          string
		bound, wanted string
	}{
		{"publication name", binding.PublicationName, publication.Name},
		{"dictionary digest", binding.DictionaryDigest, publication.DictionaryDigest},
		{"manifest digest", binding.ManifestDigest, publication.ManifestDigest},
		{"sidecar digest", binding.SidecarDigest, publication.SidecarDigest},
		{"source namespace", binding.SourceNamespace, publication.SourceNamespace},
		{"snapshot", binding.Snapshot, publication.Snapshot},
	} {
		if pair.bound != pair.wanted {
			return fmt.Errorf("snapshot publication %q binds %s %q, the Catalog declares %q",
				publication.Name, pair.name, pair.bound, pair.wanted)
		}
	}
	return nil
}

// CatalogView is the immutable Catalog material preparation reads.
//
// It is a value rather than an interface: an interface would let a caller
// answer lookups differently on two calls, and the whole point is that the
// Gateway and the finalizer prepare from the same fixed Catalog. Digest is the
// Catalog's own SHA-256 and enters the prepared identities, so preparing against
// a different Catalog produces different bindings rather than silently
// different SQL.
type CatalogView struct {
	Digest   string
	Version  string
	Products map[string]catalog.Product
	// SnapshotPublications is the whole Catalog-wide publication set, in
	// canonical order. The full set is required, not merely the ones this plan
	// touches: the ordinal dictionary universe is Catalog-wide by construction,
	// and a query-local member set would pin a root ledger to whichever product
	// it first executed and make exact cross-product union impossible.
	SnapshotPublications []catalog.SnapshotPublication
}

// Validate rejects a Catalog view that cannot settle a preparation.
func (view CatalogView) Validate() error {
	if strings.TrimSpace(view.Digest) == "" {
		return errors.New("catalog view carries no digest")
	}
	if len(view.Products) == 0 {
		return errors.New("catalog view carries no data product")
	}
	seen := map[string]bool{}
	for _, publication := range view.SnapshotPublications {
		if strings.TrimSpace(publication.Name) == "" {
			return errors.New("catalog view carries an unnamed snapshot publication")
		}
		if seen[publication.Name] {
			return fmt.Errorf("catalog view lists snapshot publication %q twice", publication.Name)
		}
		seen[publication.Name] = true
	}
	if !sort.SliceIsSorted(view.SnapshotPublications, func(left, right int) bool {
		return view.SnapshotPublications[left].Name < view.SnapshotPublications[right].Name
	}) {
		return errors.New("catalog view snapshot publications are not in canonical order")
	}
	return nil
}

// LookupProduct resolves one data product.
func (view CatalogView) LookupProduct(name string) (catalog.Product, bool) {
	product, found := view.Products[name]
	return product, found
}

// LookupSnapshotPublication resolves one publication by name.
func (view CatalogView) LookupSnapshotPublication(name string) (catalog.SnapshotPublication, bool) {
	for _, publication := range view.SnapshotPublications {
		if publication.Name == name {
			return publication, true
		}
	}
	return catalog.SnapshotPublication{}, false
}

// Inputs is everything one preparation depends on.
//
// Every member is immutable source material that the Gateway and the finalizer
// can each obtain for themselves: the Gateway from its loaded Catalog and
// verified registry, the finalizer from the frozen contract, the activated
// Profile Catalog and retained Publication evidence. Nothing here comes from the
// party whose claim is being checked, and nothing here is a Gateway type.
type Inputs struct {
	Plan    queryplan.QueryPlan
	Grant   Grant
	Catalog CatalogView
	// SnapshotBindings is keyed by publication name. It must cover every
	// publication in the Catalog when the profile compiles an ordinal program,
	// for the dictionary-universe reason recorded on CatalogView.
	SnapshotBindings map[string]SnapshotBinding
}

// Validate rejects inputs that cannot settle a preparation, before any statement
// is built.
func (inputs Inputs) Validate() error {
	if err := inputs.Grant.Validate(); err != nil {
		return err
	}
	if err := inputs.Catalog.Validate(); err != nil {
		return err
	}
	if !inputs.Grant.UsesOrdinalProgram() {
		if len(inputs.SnapshotBindings) != 0 {
			return fmt.Errorf("exposure profile %q compiles no ordinal program but %d snapshot binding(s) were supplied",
				inputs.Grant.ExposureProfile, len(inputs.SnapshotBindings))
		}
		return nil
	}
	// The universe must be complete and must be the Catalog's.
	for _, publication := range inputs.Catalog.SnapshotPublications {
		binding, found := inputs.SnapshotBindings[publication.Name]
		if !found {
			return fmt.Errorf("exposure profile %q binds the Catalog-wide dictionary universe, "+
				"but publication %q has no resolved snapshot binding",
				inputs.Grant.ExposureProfile, publication.Name)
		}
		if err := binding.RequireCatalog(publication); err != nil {
			return err
		}
	}
	for name := range inputs.SnapshotBindings {
		if _, found := inputs.Catalog.LookupSnapshotPublication(name); !found {
			return fmt.Errorf("snapshot binding %q names a publication this Catalog does not declare", name)
		}
	}
	// One dictionary digest must not resolve to two manifests, which would make
	// the dictionary universe ambiguous.
	manifests := map[string]string{}
	for _, publication := range inputs.Catalog.SnapshotPublications {
		binding := inputs.SnapshotBindings[publication.Name]
		if previous, exists := manifests[binding.DictionaryDigest]; exists && previous != binding.ManifestDigest {
			return fmt.Errorf("dictionary digest %s resolves to conflicting manifests", binding.DictionaryDigest)
		}
		manifests[binding.DictionaryDigest] = binding.ManifestDigest
	}
	return nil
}
