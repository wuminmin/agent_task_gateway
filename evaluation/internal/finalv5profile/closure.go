// Package finalv5profile derives Final-V5 deployment profiles from workload
// cells rather than from experiment names.
//
// A profile is the canonical minimal transitive Product closure required by one
// or more workload cells. Experiments are paper-level families; a profile is
// what a single Catalog-bound Gateway instance activates at one time. One
// experiment may span several profiles, and several experiments may share one
// profile whenever their closures are identical.
//
// Profile identity is the canonical closure digest, never a hand-written name.
// The readable alias exists for logs and evidence only.
//
// Readiness is deliberately not one boolean. A closure can be structurally
// complete while the live Catalog has no route for it, and a Catalog can be
// materializable long before a runtime can activate it. Collapsing those into a
// single "routable" flag hid exactly that difference, so this package tracks
// five independent states and calls a profile routable only when all five hold.
package finalv5profile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/internal/catalog"
)

// RegistryVersion identifies the closure algorithm and the canonical encoding
// used for closure digests. Changing either must change this string.
const RegistryVersion = "taskgate-final-v5-workload-closure-profile-v2"

// MaxHotBytesPerInstance is the production HOT artifact ceiling of one
// Catalog-bound Gateway instance. Profiles are activated one at a time, so this
// is checked per profile and never as a sum across mutually exclusive profiles.
const MaxHotBytesPerInstance = int64(160 << 20)

// HotLimitScope is the exact wording evidence and the paper must use. The
// ceiling is a property of one Catalog-bound Gateway instance, not a claim that
// one instance carries every enterprise data product at once.
const HotLimitScope = "per Catalog-bound Gateway instance HOT limit"

// ActivationPolicy is the operational contract every profile carries.
const ActivationPolicy = "restart a Catalog-bound Gateway on this profile Catalog; verify its SHA-256; " +
	"activate only this closure; isolate the previous profile's HOT cache, semantic cache, Publications and Task bindings; " +
	"record activation evidence; activation and restart are never part of a measured sample"

// sentinelDigest is the deliberate all-zero fail-closed value a reviewed
// Catalog candidate carries until a deployment generates the real digest.
var sentinelDigest = strings.Repeat("0", 64)

// Profile requirement classes. A cell that never binds a Gateway Catalog is not
// a coverage gap; it is a different kind of unit and must say so rather than be
// silently absent from the denominator.
const (
	// RequirementCatalogBound: the cell requests Catalog Products and therefore
	// must map to exactly one deployment profile.
	RequirementCatalogBound = "catalog_bound"
	// RequirementControlOnly: the cell runs entirely against Control-side or
	// kernel-only paths and requests no Catalog Product.
	RequirementControlOnly = "control_only"
	// RequirementProfileExempt: the cell is reviewed as exempt from profile
	// mapping for a recorded reason.
	RequirementProfileExempt = "profile_exempt"
)

// WorkloadCell is one preregistered cell together with the Products its Query
// Contract requests.
type WorkloadCell struct {
	ExperimentID string   `json:"experiment_id" yaml:"experiment_id"`
	WorkloadID   string   `json:"workload_id" yaml:"workload_id"`
	Scale        string   `json:"scale" yaml:"scale"`
	Mode         string   `json:"mode" yaml:"mode"`
	Products     []string `json:"products" yaml:"products"`
	// ProfileRequirement is machine readable and always set. A cell is never
	// dropped from the audit because it happens not to need a Catalog.
	ProfileRequirement string `json:"profile_requirement" yaml:"profile_requirement"`
	// RequirementReason explains a non catalog_bound classification.
	RequirementReason string `json:"requirement_reason,omitempty" yaml:"requirement_reason,omitempty"`
}

func (cell WorkloadCell) String() string {
	return strings.Join([]string{cell.ExperimentID, cell.WorkloadID, cell.Scale, cell.Mode}, "/")
}

// Closure is the canonical minimal transitive Product closure one profile
// activates. Only structural members take part in its identity: routes,
// budgets, digests and byte counts are properties of a deployment, not of the
// closure, so they must never change a profile ID.
type Closure struct {
	Products     []string `json:"products"`
	Publications []string `json:"publications"`
	Sources      []string `json:"sources"`
	Scopes       []string `json:"scopes"`
	SHA256       string   `json:"closure_sha256"`
}

// UnresolvedReason names one specific blocker of one specific state.
type UnresolvedReason struct {
	State   string `json:"state"`
	Code    string `json:"code"`
	Subject string `json:"subject,omitempty"`
	Detail  string `json:"detail"`
}

// ProfileStatus separates the five independent readiness questions. A closure
// that is structurally complete and materializable may still have no live
// approval route, and a Catalog may be materializable long before a runtime can
// activate it; one boolean cannot express that.
type ProfileStatus struct {
	ClosureComplete       bool `json:"closure_complete"`
	CatalogMaterializable bool `json:"catalog_materializable"`
	LiveRouteAvailable    bool `json:"live_route_available"`
	// ActivationSupported is per profile and is derived from the activation
	// support manifest, never from a global "the activator exists" constant. A
	// profile the harness could activate but never has is not supported.
	ActivationSupported bool `json:"activation_supported"`
	// ActivationSmokePassed records the evidence behind ActivationSupported. It
	// is reported, not gating: ActivationSupported is the contract state.
	ActivationSmokePassed    bool               `json:"activation_smoke_passed"`
	TargetedValidationPassed bool               `json:"targeted_validation_passed"`
	UnresolvedReasons        []UnresolvedReason `json:"unresolved_reasons"`
}

// Routable is true only when every state holds. Only a routable profile may
// carry a Publication Campaign.
func (status ProfileStatus) Routable() bool {
	return status.ClosureComplete && status.CatalogMaterializable && status.LiveRouteAvailable &&
		status.ActivationSupported && status.TargetedValidationPassed
}

// TargetedRunEligible is a derived convenience, not a sixth contract state. It
// says a profile may be activated for a pilot, an activation smoke or a
// targeted non-publication validation -- everything except a Publication
// Campaign, which still requires Routable.
//
// Passing an activation smoke never sets TargetedValidationPassed: activating a
// Catalog is not the same as executing the profile's workload cells.
func (status ProfileStatus) TargetedRunEligible() bool {
	return status.ClosureComplete && status.CatalogMaterializable &&
		status.LiveRouteAvailable && status.ActivationSupported
}

// HotArtifact is one activated HOT artifact and its observed byte count.
type HotArtifact struct {
	Publication string `json:"publication"`
	SHA256      string `json:"hot_index_digest"`
	Bytes       int64  `json:"bytes"`
}

// Profile is one registry entry. ID is derived from the closure digest, so two
// identical closures can never become two profiles and two different closures
// can never share an ID.
type Profile struct {
	ID                  string        `json:"profile_id"`
	Alias               string        `json:"alias"`
	Closure             Closure       `json:"closure"`
	Status              ProfileStatus `json:"status"`
	Routable            bool          `json:"routable"`
	TargetedRunEligible bool          `json:"targeted_run_eligible"`
	BudgetProfiles      []string      `json:"budget_profiles"`
	Cells               []string      `json:"workload_cells"`
	Experiments         []string      `json:"experiments"`
	CatalogSHA256       string        `json:"catalog_sha256"`
	CatalogPath         string        `json:"catalog_path"`
	HotArtifacts        []HotArtifact `json:"hot_artifacts"`
	TotalHotBytes       int64         `json:"total_hot_bytes"`
	MaxHotLimitBytes    int64         `json:"max_hot_limit_bytes"`
	ActivationPolicy    string        `json:"required_activation_policy"`
	WithinHotLimitBytes bool          `json:"within_hot_limit"`
}

// Registry is the complete source-controlled profile set. Every preregistered
// cell appears in exactly one profile: a cell is never dropped because its
// Product is not live yet, it is carried by a profile whose status says so.
type Registry struct {
	SchemaVersion    int       `json:"schema_version"`
	RegistryVersion  string    `json:"registry_version"`
	ContractRelease  string    `json:"contract_release"`
	MaxHotLimitBytes int64     `json:"max_hot_limit_bytes"`
	HotLimitScope    string    `json:"max_hot_limit_scope"`
	Profiles         []Profile `json:"profiles"`
}

// ComputeClosure derives the canonical minimal transitive Product closure
// against the master Catalog. Structural gaps are reported as reasons rather
// than errors, so a cell whose Product is not live yet still yields a
// first-class profile candidate instead of disappearing from the registry.
func ComputeClosure(master *catalog.Catalog, requested []string) (Closure, []UnresolvedReason, error) {
	if master == nil {
		return Closure{}, nil, errors.New("closure requires a master Catalog")
	}
	if len(requested) == 0 {
		return Closure{}, nil, errors.New("closure requires at least one Product")
	}
	var reasons []UnresolvedReason
	products := map[string]catalog.Product{}
	names := map[string]bool{}
	pending := append([]string(nil), requested...)
	for len(pending) > 0 {
		name := pending[0]
		pending = pending[1:]
		if names[name] {
			continue
		}
		names[name] = true
		product, found := lookupProduct(master, name)
		if !found {
			reasons = append(reasons, UnresolvedReason{State: "closure_complete",
				Code: "product_absent_from_master_catalog", Subject: name,
				Detail: "the master Catalog candidate does not define this Product"})
			continue
		}
		products[name] = product
		// A semantic View product reads through terminal Products. Its public
		// scopes name them, so the closure follows those scopes rather than
		// trusting a hand-maintained dependency list.
		pending = append(pending, terminalProducts(master, product)...)
	}

	closure := Closure{}
	publications := map[string]bool{}
	sources := map[string]bool{}
	scopes := map[string]bool{}
	closure.Products = sortedKeys(names)
	for _, name := range closure.Products {
		product, present := products[name]
		if !present {
			continue
		}
		if strings.TrimSpace(product.Source) == "" {
			reasons = append(reasons, UnresolvedReason{State: "closure_complete",
				Code: "product_has_no_source", Subject: name, Detail: "the Product declares no datasource"})
		} else {
			sources[product.Source] = true
		}
		for _, scope := range product.Scopes {
			if !hasScope(master, scope) {
				reasons = append(reasons, UnresolvedReason{State: "closure_complete",
					Code: "scope_absent", Subject: scope,
					Detail: "the master Catalog omits a scope this Product requires"})
				continue
			}
			scopes[scope] = true
		}
		if product.SnapshotPublication == "" {
			// A semantic View product has no immutable Publication of its own;
			// its terminals carry the published rows.
			continue
		}
		publication, found := lookupPublication(master, product.SnapshotPublication)
		if !found {
			reasons = append(reasons, UnresolvedReason{State: "closure_complete",
				Code: "publication_absent", Subject: product.SnapshotPublication,
				Detail: "the master Catalog omits a Publication this Product binds"})
			continue
		}
		if publication.Snapshot != product.Snapshot {
			reasons = append(reasons, UnresolvedReason{State: "closure_complete",
				Code: "publication_snapshot_mismatch", Subject: publication.Name,
				Detail: "the Publication snapshot differs from the Product snapshot"})
		}
		if strings.TrimSpace(publication.OrdinalSidecar) == "" {
			reasons = append(reasons, UnresolvedReason{State: "closure_complete",
				Code: "ordinal_sidecar_absent", Subject: publication.Name,
				Detail: "the Publication declares no ordinal sidecar"})
		}
		publications[publication.Name] = true
		sources[publication.Source] = true
	}
	closure.Publications = sortedKeys(publications)
	closure.Scopes = sortedKeys(scopes)
	for _, name := range sortedKeys(sources) {
		if !hasSource(master, name) {
			reasons = append(reasons, UnresolvedReason{State: "closure_complete",
				Code: "source_absent", Subject: name, Detail: "the master Catalog omits this datasource"})
			continue
		}
		closure.Sources = append(closure.Sources, name)
	}
	if len(closure.Publications) == 0 {
		reasons = append(reasons, UnresolvedReason{State: "closure_complete",
			Code:   "no_immutable_publication",
			Detail: "the closure activates no immutable Publication"})
	}
	closure.SHA256 = closureDigest(closure)
	sortReasons(reasons)
	return closure, reasons, nil
}

// EvaluateStatus answers the deployment-dependent questions against the live
// Catalog and the observed HOT artifacts, keeping each state separate.
func EvaluateStatus(closure Closure, closureReasons []UnresolvedReason, live *catalog.Catalog,
	hot map[string]HotArtifact, activationSupported bool) (ProfileStatus, []string) {
	status := ProfileStatus{ClosureComplete: len(closureReasons) == 0,
		ActivationSupported: activationSupported, ActivationSmokePassed: activationSupported,
		UnresolvedReasons: append([]UnresolvedReason(nil), closureReasons...)}
	if !status.ActivationSupported {
		status.UnresolvedReasons = append(status.UnresolvedReasons, UnresolvedReason{
			State: "activation_supported", Code: "live_activation_smoke_not_executed",
			Detail: "no live activation smoke has been recorded for this profile"})
	}
	materializable := true
	for _, name := range closure.Products {
		if _, found := lookupProduct(live, name); !found {
			materializable = false
			status.UnresolvedReasons = append(status.UnresolvedReasons, UnresolvedReason{
				State: "catalog_materializable", Code: "product_absent_from_live_catalog", Subject: name,
				Detail: "the live Catalog does not publish this Product"})
		}
	}
	for _, name := range closure.Publications {
		publication, found := lookupPublication(live, name)
		if !found {
			materializable = false
			status.UnresolvedReasons = append(status.UnresolvedReasons, UnresolvedReason{
				State: "catalog_materializable", Code: "publication_absent_from_live_catalog", Subject: name,
				Detail: "the live Catalog does not declare this Publication"})
			continue
		}
		for _, pair := range []struct{ label, digest string }{
			{"sidecar_digest", publication.SidecarDigest},
			{"dictionary_digest", publication.DictionaryDigest},
			{"manifest_digest", publication.ManifestDigest},
		} {
			if len(pair.digest) != 64 || pair.digest == sentinelDigest {
				materializable = false
				status.UnresolvedReasons = append(status.UnresolvedReasons, UnresolvedReason{
					State: "catalog_materializable", Code: "publication_digest_not_generated",
					Subject: name + "." + pair.label,
					Detail:  "the Publication digest is absent or still the fail-closed candidate sentinel"})
			}
		}
		if _, measured := hot[name]; !measured {
			materializable = false
			status.UnresolvedReasons = append(status.UnresolvedReasons, UnresolvedReason{
				State: "catalog_materializable", Code: "hot_artifact_not_observed", Subject: name,
				Detail: "no deployment has produced this Publication's HOT artifact"})
		}
	}
	status.CatalogMaterializable = materializable

	var budgets []string
	switch {
	case !materializable:
		status.UnresolvedReasons = append(status.UnresolvedReasons, UnresolvedReason{
			State: "live_route_available", Code: "route_not_evaluable",
			Detail: "the closure is not materializable, so no live route can be resolved"})
	default:
		policy, err := live.ResolveTaskPolicy(closure.Products)
		if err != nil {
			status.UnresolvedReasons = append(status.UnresolvedReasons, UnresolvedReason{
				State: "live_route_available", Code: "no_approval_route_for_closure",
				Subject: strings.Join(closure.Products, ","),
				Detail:  "the live Catalog resolves no approval route for this exact Product closure: " + err.Error()})
		} else {
			status.LiveRouteAvailable = true
			budgets = []string{policy.BudgetProfile}
		}
	}
	// A targeted non-publication run is what proves a profile end to end. No
	// static state can assert it; a campaign records it separately.
	status.UnresolvedReasons = append(status.UnresolvedReasons, UnresolvedReason{
		State: "targeted_validation_passed", Code: "targeted_run_not_executed",
		Detail: "no targeted non-publication run has executed this profile's cells"})
	sortReasons(status.UnresolvedReasons)
	return status, budgets
}

// terminalProducts resolves the Products a View product reads through by
// matching its public scopes onto the Products that declare them natively.
func terminalProducts(master *catalog.Catalog, product catalog.Product) []string {
	if product.ViewContract == nil {
		return nil
	}
	terminals := map[string]bool{}
	for _, scope := range product.Scopes {
		field := strings.TrimSuffix(scope, "_partition_key")
		for _, candidate := range master.Products {
			if candidate.Name == product.Name || candidate.ViewContract != nil {
				continue
			}
			if strings.HasSuffix(candidate.Name, field) || strings.HasSuffix(candidate.StableRelationRole, field) {
				terminals[candidate.Name] = true
			}
		}
	}
	return sortedKeys(terminals)
}

// closureDigest is the canonical closure identity: a length-delimited,
// domain-separated encoding of the sorted structural members. Reordering,
// renaming, adding or dropping any member changes it; a route, budget, digest
// or byte count never does.
func closureDigest(closure Closure) string {
	hash := sha256.New()
	hash.Write([]byte(RegistryVersion + "\x00"))
	for _, group := range [][]string{closure.Products, closure.Publications, closure.Sources, closure.Scopes} {
		fmt.Fprintf(hash, "%d\x00", len(group))
		for _, member := range group {
			fmt.Fprintf(hash, "%d\x00%s\x00", len(member), member)
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// ProfileID derives the stable profile identity from the closure digest.
func ProfileID(closureSHA256 string) string {
	if len(closureSHA256) != 64 {
		return ""
	}
	return "profile-" + closureSHA256[:16]
}

// BuildInput carries everything Build needs to classify a profile.
type BuildInput struct {
	Master *catalog.Catalog
	Live   *catalog.Catalog
	// ProfileCatalogs are independently activatable Catalogs keyed by closure
	// digest. They take precedence over the default live Catalog for status
	// evaluation, because a per-profile deployment is not required to publish
	// the union of every workload closure.
	ProfileCatalogs     map[string]*catalog.Catalog
	Cells               []WorkloadCell
	Aliases             map[string]string
	Hot                 map[string]HotArtifact
	ActivationSupported bool
}

// Build groups cells into profiles. Cells whose closures are identical are
// merged into one profile; no profile is ever created because two cells came
// from differently named experiments, and no cell is ever dropped.
func Build(input BuildInput) ([]Profile, error) {
	if len(input.Cells) == 0 {
		return nil, errors.New("profile registry requires at least one workload cell")
	}
	byDigest := map[string]*Profile{}
	seenCells := map[string]bool{}
	for _, cell := range input.Cells {
		if seenCells[cell.String()] {
			return nil, fmt.Errorf("workload cell %s is declared twice", cell)
		}
		seenCells[cell.String()] = true
		if cell.ProfileRequirement != RequirementCatalogBound {
			continue
		}
		closure, reasons, err := ComputeClosure(input.Master, cell.Products)
		if err != nil {
			return nil, fmt.Errorf("workload cell %s: %w", cell, err)
		}
		profile, present := byDigest[closure.SHA256]
		if !present {
			status, budgets := EvaluateStatus(closure, reasons, input.Live, input.Hot,
				input.ActivationSupported)
			// A standalone profile Catalog is an alternate materialization source,
			// not an alternate route evaluator for closures the default Catalog
			// already publishes. This preserves existing no-route classifications.
			if !status.CatalogMaterializable {
				if profileCatalog, found := input.ProfileCatalogs[closure.SHA256]; found {
					status, budgets = EvaluateStatus(closure, reasons, profileCatalog, input.Hot,
						input.ActivationSupported)
				}
			}
			profile = &Profile{ID: ProfileID(closure.SHA256), Closure: closure, Status: status,
				Routable: status.Routable(), TargetedRunEligible: status.TargetedRunEligible(), BudgetProfiles: budgets,
				MaxHotLimitBytes: MaxHotBytesPerInstance, ActivationPolicy: ActivationPolicy}
			byDigest[closure.SHA256] = profile
		}
		profile.Cells = append(profile.Cells, cell.String())
		if !contains(profile.Experiments, cell.ExperimentID) {
			profile.Experiments = append(profile.Experiments, cell.ExperimentID)
		}
	}
	profiles := make([]Profile, 0, len(byDigest))
	for _, digest := range sortedKeys(byDigest) {
		profile := byDigest[digest]
		sort.Strings(profile.Cells)
		sort.Strings(profile.Experiments)
		alias, named := input.Aliases[digest]
		if !named || strings.TrimSpace(alias) == "" {
			return nil, fmt.Errorf("closure %s has no reviewed alias", digest)
		}
		profile.Alias = alias
		if profile.Status.CatalogMaterializable {
			profile.CatalogPath = "config/profiles/" + alias + ".catalog.yaml"
		}
		profiles = append(profiles, *profile)
	}
	if err := validateAliases(profiles); err != nil {
		return nil, err
	}
	return profiles, nil
}

// validateAliases proves the identity rules: one alias per closure, one closure
// per alias, and a profile ID that is derived from the closure digest.
func validateAliases(profiles []Profile) error {
	byAlias := map[string]string{}
	byID := map[string]string{}
	for _, profile := range profiles {
		if previous, present := byAlias[profile.Alias]; present && previous != profile.Closure.SHA256 {
			return fmt.Errorf("alias %q names two different closures", profile.Alias)
		}
		byAlias[profile.Alias] = profile.Closure.SHA256
		if profile.ID != ProfileID(profile.Closure.SHA256) {
			return fmt.Errorf("profile %q ID is not derived from its closure digest", profile.Alias)
		}
		if previous, present := byID[profile.ID]; present && previous != profile.Closure.SHA256 {
			return fmt.Errorf("profile ID %q covers two different closures", profile.ID)
		}
		byID[profile.ID] = profile.Closure.SHA256
	}
	return nil
}

// ValidateRegistry enforces the completeness rules a campaign depends on.
func ValidateRegistry(registry Registry, cells []WorkloadCell) error {
	if registry.SchemaVersion != 1 || registry.RegistryVersion != RegistryVersion ||
		registry.MaxHotLimitBytes != MaxHotBytesPerInstance || registry.HotLimitScope != HotLimitScope {
		return errors.New("profile registry header is invalid")
	}
	assigned := map[string]int{}
	for _, profile := range registry.Profiles {
		if profile.ID != ProfileID(profile.Closure.SHA256) {
			return fmt.Errorf("profile %q ID is not derived from its closure digest", profile.Alias)
		}
		if profile.Closure.SHA256 != closureDigest(profile.Closure) {
			return fmt.Errorf("profile %q closure digest does not describe its own members", profile.Alias)
		}
		if profile.MaxHotLimitBytes != MaxHotBytesPerInstance {
			return fmt.Errorf("profile %q records a different HOT limit", profile.Alias)
		}
		if profile.Routable != profile.Status.Routable() {
			return fmt.Errorf("profile %q routable flag disagrees with its five states", profile.Alias)
		}
		if profile.TargetedRunEligible != profile.Status.TargetedRunEligible() {
			return fmt.Errorf("profile %q targeted_run_eligible disagrees with its four preconditions", profile.Alias)
		}
		if profile.Routable && !profile.TargetedRunEligible {
			return fmt.Errorf("profile %q is routable without being targeted-run eligible", profile.Alias)
		}
		if !profile.Routable && len(profile.Status.UnresolvedReasons) == 0 {
			return fmt.Errorf("profile %q is not routable and records no structured reason", profile.Alias)
		}
		if profile.Status.CatalogMaterializable {
			if profile.CatalogPath == "" || profile.CatalogSHA256 == "" {
				return fmt.Errorf("materializable profile %q has no generated Catalog", profile.Alias)
			}
			if profile.TotalHotBytes > profile.MaxHotLimitBytes || !profile.WithinHotLimitBytes {
				return fmt.Errorf("profile %q activates %d HOT bytes against the %d byte %s",
					profile.Alias, profile.TotalHotBytes, profile.MaxHotLimitBytes, HotLimitScope)
			}
			var total int64
			for _, artifact := range profile.HotArtifacts {
				if artifact.Bytes <= 0 || !contains(profile.Closure.Publications, artifact.Publication) {
					return fmt.Errorf("profile %q records a HOT artifact outside its closure", profile.Alias)
				}
				total += artifact.Bytes
			}
			if total != profile.TotalHotBytes {
				return fmt.Errorf("profile %q HOT total does not match its artifacts", profile.Alias)
			}
		} else if profile.CatalogPath != "" || profile.CatalogSHA256 != "" || len(profile.HotArtifacts) != 0 {
			return fmt.Errorf("non-materializable profile %q carries a generated Catalog or HOT artifact", profile.Alias)
		}
		for _, cell := range profile.Cells {
			assigned[cell]++
		}
	}
	if err := validateAliases(registry.Profiles); err != nil {
		return err
	}
	catalogBound := 0
	for _, cell := range cells {
		if cell.ProfileRequirement != RequirementCatalogBound {
			if assigned[cell.String()] != 0 {
				return fmt.Errorf("%s cell %s was mapped to a profile", cell.ProfileRequirement, cell)
			}
			if strings.TrimSpace(cell.RequirementReason) == "" {
				return fmt.Errorf("cell %s is %s without a recorded reason", cell, cell.ProfileRequirement)
			}
			continue
		}
		catalogBound++
		switch assigned[cell.String()] {
		case 1:
		case 0:
			return fmt.Errorf("catalog-bound cell %s is not assigned to any profile", cell)
		default:
			return fmt.Errorf("catalog-bound cell %s is assigned to %d profiles", cell, assigned[cell.String()])
		}
	}
	if len(assigned) != catalogBound {
		return fmt.Errorf("profile registry assigns %d cells, the contract declares %d catalog-bound cells",
			len(assigned), catalogBound)
	}
	return nil
}

// LookupCellProfile returns the profile that owns one workload cell.
func LookupCellProfile(registry Registry, cell WorkloadCell) (Profile, error) {
	for _, profile := range registry.Profiles {
		if contains(profile.Cells, cell.String()) {
			return profile, nil
		}
	}
	return Profile{}, fmt.Errorf("workload cell %s has no profile", cell)
}

// LookupProfileByID returns one registered profile.
func LookupProfileByID(registry Registry, profileID string) (Profile, error) {
	for _, profile := range registry.Profiles {
		if profile.ID == profileID {
			return profile, nil
		}
	}
	return Profile{}, fmt.Errorf("profile %q is not registered", profileID)
}

// RequireSameProfile proves that every arm and repetition of one cell, and every
// cell that must share an environment, resolves to the same profile identity.
func RequireSameProfile(registry Registry, cells ...WorkloadCell) error {
	if len(cells) < 2 {
		return errors.New("profile equality requires at least two cells")
	}
	first, err := LookupCellProfile(registry, cells[0])
	if err != nil {
		return err
	}
	for _, cell := range cells[1:] {
		other, err := LookupCellProfile(registry, cell)
		if err != nil {
			return err
		}
		if other.ID != first.ID || other.Closure.SHA256 != first.Closure.SHA256 ||
			other.CatalogSHA256 != first.CatalogSHA256 {
			return fmt.Errorf("%s and %s resolve to different profiles (%s vs %s)",
				cells[0], cell, first.ID, other.ID)
		}
	}
	return nil
}

func sortReasons(reasons []UnresolvedReason) {
	sort.Slice(reasons, func(left, right int) bool {
		if reasons[left].State != reasons[right].State {
			return reasons[left].State < reasons[right].State
		}
		if reasons[left].Code != reasons[right].Code {
			return reasons[left].Code < reasons[right].Code
		}
		return reasons[left].Subject < reasons[right].Subject
	})
}

func lookupProduct(source *catalog.Catalog, name string) (catalog.Product, bool) {
	if source == nil {
		return catalog.Product{}, false
	}
	for _, product := range source.Products {
		if product.Name == name {
			return product, true
		}
	}
	return catalog.Product{}, false
}

func lookupPublication(source *catalog.Catalog, name string) (catalog.SnapshotPublication, bool) {
	if source == nil {
		return catalog.SnapshotPublication{}, false
	}
	for _, publication := range source.SnapshotPublications {
		if publication.Name == name {
			return publication, true
		}
	}
	return catalog.SnapshotPublication{}, false
}

func hasScope(source *catalog.Catalog, name string) bool {
	for _, scope := range source.Scopes {
		if scope.Name == name {
			return true
		}
	}
	return false
}

func hasSource(source *catalog.Catalog, name string) bool {
	for _, entry := range source.Sources {
		if entry.Name == name {
			return true
		}
	}
	return false
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
