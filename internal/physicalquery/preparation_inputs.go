package physicalquery

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/queryplan"
)

// The input domain separators.
const (
	preparationInputsDomain = "TASKGATE-PREPARATION-INPUTS-V1"
	preparationGrantDomain  = "TASKGATE-PREPARATION-GRANT-V1"
	snapshotBindingDomain   = "TASKGATE-PREPARATION-SNAPSHOT-BINDING-SET-V1"
	preparationPlanDomain   = "TASKGATE-PREPARATION-PLAN-V1"
)

// Grant is the authorization material preparation reads.
//
// It is deliberately NOT control.TaskGrant. That type sits beside the Control
// Store, so depending on it would put a store package in this one's import graph
// for the sake of six fields, and it carries members preparation has no business
// seeing -- the approval receipt, the budget. The Gateway maps its
// control.TaskGrant onto this; the finalizer maps its independently obtained
// frozen contract material onto the same type. Both then hand the same values to
// the same function.
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
	// profile produces none.
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

// Canonical returns the grant in canonical form: products sorted, columns sorted
// and deduplicated, scope re-encoded canonically.
//
// Canonicalizing rather than merely rejecting is deliberate. Two callers
// legitimately assemble the same authorization in different orders -- one walks
// a Catalog, the other reads a frozen contract -- and requiring them to agree on
// iteration order would make the identity depend on how each happened to build
// its map. What must not differ is the content, and Validate checks that.
func (grant Grant) Canonical() (Grant, error) {
	if err := grant.Validate(); err != nil {
		return Grant{}, err
	}
	canonical := Grant{
		ApprovedProducts:  canonicalStrings(grant.ApprovedProducts),
		ExposureProfile:   grant.ExposureProfile,
		PredicateLimits:   grant.PredicateLimits,
		ViewBindingDigest: grant.ViewBindingDigest, ViewRegistryRevision: grant.ViewRegistryRevision,
	}
	if len(grant.ApprovedColumns) > 0 {
		canonical.ApprovedColumns = make(map[string][]string, len(grant.ApprovedColumns))
		for product, columns := range grant.ApprovedColumns {
			canonical.ApprovedColumns[product] = canonicalStrings(columns)
		}
	}
	if len(grant.MandatoryScope) > 0 {
		// Re-encoded through the canonical encoder so two byte-different but
		// semantically equal scopes reach one identity.
		var scope any
		if err := json.Unmarshal(grant.MandatoryScope, &scope); err != nil {
			return Grant{}, fmt.Errorf("preparation grant mandatory scope is not JSON: %w", err)
		}
		encoded, err := approval.CanonicalJSON(scope)
		if err != nil {
			return Grant{}, fmt.Errorf("canonicalize preparation grant mandatory scope: %w", err)
		}
		canonical.MandatoryScope = encoded
	}
	return canonical, nil
}

// Validate rejects a grant that cannot settle a preparation deterministically.
func (grant Grant) Validate() error {
	if len(grant.ApprovedProducts) == 0 {
		return errors.New("preparation grant approves no data product")
	}
	approved := map[string]bool{}
	for _, product := range grant.ApprovedProducts {
		if strings.TrimSpace(product) == "" {
			return errors.New("preparation grant approves an unnamed data product")
		}
		if approved[product] {
			return fmt.Errorf("preparation grant lists product %q twice", product)
		}
		approved[product] = true
	}
	for product, columns := range grant.ApprovedColumns {
		// A column grant for a product the task never approved is not a harmless
		// extra: it would widen the surface a statement is compiled against
		// without appearing in the approved-product list a reviewer reads.
		if !approved[product] {
			return fmt.Errorf("preparation grant approves columns for %q, which is not an approved product", product)
		}
		seen := map[string]bool{}
		for _, column := range columns {
			if strings.TrimSpace(column) == "" {
				return fmt.Errorf("preparation grant approves an unnamed column for %q", product)
			}
			if seen[column] {
				return fmt.Errorf("preparation grant lists column %q of %q twice", column, product)
			}
			seen[column] = true
		}
	}
	if grant.ExposureProfile != "" {
		switch grant.ExposureProfile {
		case exposure.ProfileV1, exposure.ProfileV2, exposure.ProfileV3,
			exposure.ProfileV4, exposure.ProfileV5:
		default:
			return fmt.Errorf("preparation grant names unknown exposure profile %q", grant.ExposureProfile)
		}
	}
	if grant.ViewBindingDigest != "" && !validSHA256(grant.ViewBindingDigest) {
		return errors.New("preparation grant view binding digest is not a lowercase SHA-256")
	}
	if len(grant.MandatoryScope) > 0 && !json.Valid(grant.MandatoryScope) {
		return errors.New("preparation grant mandatory scope is not valid JSON")
	}
	return nil
}

// SHA256 is the grant's canonical domain-separated digest.
func (grant Grant) SHA256() (string, error) {
	canonical, err := grant.Canonical()
	if err != nil {
		return "", err
	}
	// The map is flattened to a sorted slice so the digest cannot depend on Go's
	// map iteration order.
	type productColumns struct {
		Product string   `json:"product"`
		Columns []string `json:"columns"`
	}
	columns := make([]productColumns, 0, len(canonical.ApprovedColumns))
	for product, values := range canonical.ApprovedColumns {
		columns = append(columns, productColumns{Product: product, Columns: values})
	}
	sort.Slice(columns, func(left, right int) bool { return columns[left].Product < columns[right].Product })
	return domainDigest(preparationGrantDomain, struct {
		ApprovedProducts     []string                  `json:"approved_products"`
		ApprovedColumns      []productColumns          `json:"approved_columns"`
		MandatoryScope       json.RawMessage           `json:"mandatory_scope,omitempty"`
		ExposureProfile      string                    `json:"exposure_profile"`
		PredicateLimits      queryplan.PredicateLimits `json:"predicate_limits"`
		ViewBindingDigest    string                    `json:"view_binding_digest,omitempty"`
		ViewRegistryRevision string                    `json:"view_registry_revision,omitempty"`
	}{canonical.ApprovedProducts, columns, canonical.MandatoryScope, canonical.ExposureProfile,
		canonical.PredicateLimits, canonical.ViewBindingDigest, canonical.ViewRegistryRevision})
}

// SnapshotBinding is one already-verified immutable snapshot publication, as the
// caller resolved it.
//
// This is what replaces the Gateway's in-memory snapshot registry. The Gateway
// resolves it from its live registry and the finalizer from retained Profile and
// Publication evidence; each verifies the artifact against the Catalog BEFORE
// calling this package and passes the verified values in. Preparation then
// depends on values rather than on whoever holds the registry.
//
// It carries the immutable metadata the derivation actually reads, not merely
// the digests that identify it: the sidecar relation the generated SQL joins and
// the row count the base-fact estimate uses. A descriptor that identified an
// artifact without describing it would force this package to go and look, which
// is the dependency the extraction exists to remove.
type SnapshotBinding struct {
	// PublicationName is the Catalog publication this binding is for.
	PublicationName string
	// The artifact's identity as the caller resolved it. Prepare requires each to
	// equal what the Catalog declares, so a caller that resolved a different
	// artifact fails rather than producing statements against it.
	DictionaryDigest string
	ManifestDigest   string
	SidecarDigest    string
	// SourceNamespace and Snapshot are the artifact's own source binding.
	SourceNamespace string
	Snapshot        string
	// OrdinalSidecar is the schema-qualified sidecar relation the generated
	// provenance statement joins, exactly as the Catalog publishes it.
	OrdinalSidecar string
	// RowCount is the immutable snapshot row count. The estimated-base-fact
	// derivation reads it, so a descriptor without it cannot prepare a V4 or V5
	// operation at all.
	RowCount uint64
}

// Validate rejects a binding that leaves the artifact ambiguous.
func (binding SnapshotBinding) Validate() error {
	for name, value := range map[string]string{
		"publication name": binding.PublicationName,
		"source namespace": binding.SourceNamespace,
		"snapshot":         binding.Snapshot,
		"ordinal sidecar":  binding.OrdinalSidecar,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("snapshot binding for %q carries no %s", binding.PublicationName, name)
		}
	}
	for name, digest := range map[string]string{
		"dictionary digest": binding.DictionaryDigest,
		"manifest digest":   binding.ManifestDigest,
		"sidecar digest":    binding.SidecarDigest,
	} {
		if !validSHA256(digest) {
			return fmt.Errorf("snapshot binding for %q has a %s that is not a lowercase SHA-256",
				binding.PublicationName, name)
		}
	}
	// A schema-qualified relation and nothing else. Anything the generated SQL
	// would have to quote differently is rejected rather than escaped.
	schema, view, found := strings.Cut(binding.OrdinalSidecar, ".")
	if !found || strings.TrimSpace(schema) == "" || strings.TrimSpace(view) == "" ||
		strings.Contains(view, ".") {
		return fmt.Errorf("snapshot binding for %q has ordinal sidecar %q, which is not schema.relation",
			binding.PublicationName, binding.OrdinalSidecar)
	}
	return nil
}

// RequireCatalog rejects a binding that does not describe the Catalog's own
// publication. It is what makes "the caller resolved the artifact" mean "the
// caller resolved THIS artifact".
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
		{"ordinal sidecar", binding.OrdinalSidecar, publication.OrdinalSidecar},
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
// It is a value rather than an interface: an interface would let a caller answer
// two lookups differently, and the whole point is that both sides prepare from
// one fixed Catalog. Digest enters the prepared identities, so preparing against
// a different Catalog produces a different binding rather than silently
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
	if !validSHA256(view.Digest) {
		return errors.New("catalog view digest is not a lowercase SHA-256")
	}
	if strings.TrimSpace(view.Version) == "" {
		return errors.New("catalog view carries no version")
	}
	if len(view.Products) == 0 {
		return errors.New("catalog view carries no data product")
	}
	for name, product := range view.Products {
		// The key and the product's own name must agree, or a lookup would return
		// a product that does not answer to what it was asked for.
		if name != product.Name {
			return fmt.Errorf("catalog view indexes product %q under key %q", product.Name, name)
		}
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

// PreparationInputs is everything one preparation depends on.
//
// Every member is immutable source material both sides can obtain for
// themselves: the Gateway from its loaded Catalog and verified registry, the
// finalizer from the frozen contract, the activated Profile Catalog and retained
// Publication evidence. Nothing here comes from the party whose claim is being
// checked, and nothing here is a Gateway type.
type PreparationInputs struct {
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
func (inputs PreparationInputs) Validate() error {
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
	// The universe must be complete, and must be the Catalog's.
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
			return fmt.Errorf("dictionary digest %s resolves to conflicting manifests",
				shortDigest(binding.DictionaryDigest))
		}
		manifests[binding.DictionaryDigest] = binding.ManifestDigest
	}
	return nil
}

// SnapshotBindingSetSHA256 is the canonical digest of the whole resolved
// universe, empty when the profile resolves none.
func (inputs PreparationInputs) SnapshotBindingSetSHA256() (string, error) {
	if len(inputs.SnapshotBindings) == 0 {
		return "", nil
	}
	bindings := make([]SnapshotBinding, 0, len(inputs.SnapshotBindings))
	for _, binding := range inputs.SnapshotBindings {
		bindings = append(bindings, binding)
	}
	sort.Slice(bindings, func(left, right int) bool {
		return bindings[left].PublicationName < bindings[right].PublicationName
	})
	return domainDigest(snapshotBindingDomain, bindings)
}

// PlanSHA256 is the canonical digest of the QueryPlan as supplied.
func (inputs PreparationInputs) PlanSHA256() (string, error) {
	return domainDigest(preparationPlanDomain, inputs.Plan)
}

// SHA256 is the identity of the whole input set.
//
// It is over the component digests rather than over the raw values, so the
// construction stays readable and each component keeps its own separately
// checkable identity. Two semantically equal input sets assembled in different
// orders reach the same value; any change to a grant, a scope, a Publication or
// a sidecar moves it.
func (inputs PreparationInputs) SHA256() (string, error) {
	if err := inputs.Validate(); err != nil {
		return "", err
	}
	grant, err := inputs.Grant.SHA256()
	if err != nil {
		return "", err
	}
	plan, err := inputs.PlanSHA256()
	if err != nil {
		return "", err
	}
	snapshots, err := inputs.SnapshotBindingSetSHA256()
	if err != nil {
		return "", err
	}
	return domainDigest(preparationInputsDomain, struct {
		Version        string `json:"version"`
		PlanSHA256     string `json:"plan_sha256"`
		GrantSHA256    string `json:"grant_sha256"`
		CatalogSHA256  string `json:"catalog_sha256"`
		CatalogVersion string `json:"catalog_version"`
		SnapshotsSHA   string `json:"snapshot_binding_set_sha256,omitempty"`
	}{PreparedOperationBindingV1Version, plan, grant, inputs.Catalog.Digest,
		inputs.Catalog.Version, snapshots})
}
