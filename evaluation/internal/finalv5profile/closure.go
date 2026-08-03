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
const RegistryVersion = "taskgate-final-v5-workload-closure-profile-v1"

// MaxHotBytesPerInstance is the production HOT artifact ceiling of one
// Catalog-bound Gateway instance. Profiles are activated one at a time, so this
// is checked per profile and never as a sum across mutually exclusive profiles.
const MaxHotBytesPerInstance = int64(160 << 20)

// WorkloadCell is one preregistered cell together with the Products its Query
// Contract requests. Terminals lists the Products a semantic View product reads
// through, which the closure must also carry.
type WorkloadCell struct {
	ExperimentID string   `json:"experiment_id" yaml:"experiment_id"`
	WorkloadID   string   `json:"workload_id" yaml:"workload_id"`
	Scale        string   `json:"scale" yaml:"scale"`
	Mode         string   `json:"mode" yaml:"mode"`
	Products     []string `json:"products" yaml:"products"`
}

func (cell WorkloadCell) String() string {
	return strings.Join([]string{cell.ExperimentID, cell.WorkloadID, cell.Scale, cell.Mode}, "/")
}

// Closure is the canonical minimal transitive closure one profile activates.
type Closure struct {
	Products       []string `json:"products"`
	Publications   []string `json:"publications"`
	Sources        []string `json:"sources"`
	Scopes         []string `json:"scopes"`
	BudgetProfiles []string `json:"budget_profiles"`
	// Routable records whether the live Catalog can actually grant a task over
	// this closure. An unroutable closure is still a real profile; it simply
	// cannot be activated for a measured run.
	Routable bool   `json:"routable"`
	SHA256   string `json:"closure_sha256"`
}

// HotArtifact is one activated HOT artifact and its observed byte count.
type HotArtifact struct {
	Publication string `json:"publication"`
	SHA256      string `json:"hot_index_digest"`
	Bytes       int64  `json:"bytes"`
}

// Profile is one registry entry. ID is derived from ClosureSHA256, so two
// identical closures can never become two profiles and two different closures
// can never share an ID.
type Profile struct {
	ID                  string        `json:"profile_id"`
	Alias               string        `json:"alias"`
	Closure             Closure       `json:"closure"`
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

// UnresolvedCell is a preregistered cell whose Products the live Catalog does
// not publish yet. It is recorded rather than dropped: a silently missing cell
// would let an incomplete deployment look complete.
type UnresolvedCell struct {
	Cell     string   `json:"cell"`
	Products []string `json:"products"`
	Reason   string   `json:"reason"`
}

// Registry is the complete source-controlled profile set.
type Registry struct {
	SchemaVersion    int              `json:"schema_version"`
	RegistryVersion  string           `json:"registry_version"`
	ContractRelease  string           `json:"contract_release"`
	MaxHotLimitBytes int64            `json:"max_hot_limit_bytes"`
	HotLimitScope    string           `json:"max_hot_limit_scope"`
	Profiles         []Profile        `json:"profiles"`
	UnresolvedCells  []UnresolvedCell `json:"unresolved_cells"`
}

// HotLimitScope is the exact wording evidence and the paper must use. The
// ceiling is a property of one Catalog-bound Gateway instance, not a claim that
// one instance carries every enterprise data product at once.
const HotLimitScope = "per Catalog-bound Gateway instance HOT limit"

// ActivationPolicy is the operational contract every profile carries.
const ActivationPolicy = "restart a Catalog-bound Gateway on this profile Catalog; verify its SHA-256; " +
	"activate only this closure; isolate the previous profile's HOT cache, semantic cache, Publications and Task bindings; " +
	"record activation evidence; activation and restart are never part of a measured sample"

// ComputeClosure derives the canonical minimal transitive closure of a Product
// set against a full Catalog. It fails closed when a requested Product, one of
// its transitive dependencies, its Publication, its sidecar, its dictionary, or
// its approval route is absent.
func ComputeClosure(full *catalog.Catalog, requested []string) (Closure, error) {
	if full == nil {
		return Closure{}, errors.New("closure requires a Catalog")
	}
	if len(requested) == 0 {
		return Closure{}, errors.New("closure requires at least one Product")
	}
	products := map[string]catalog.Product{}
	pending := append([]string(nil), requested...)
	for len(pending) > 0 {
		name := pending[0]
		pending = pending[1:]
		if _, done := products[name]; done {
			continue
		}
		product, found := lookupProduct(full, name)
		if !found {
			return Closure{}, fmt.Errorf("Catalog does not publish Product %q", name)
		}
		products[name] = product
		// A semantic View product reads through terminal Products. Its public
		// scopes name them, so the closure follows those scopes rather than
		// trusting a hand-maintained dependency list.
		for _, terminal := range terminalProducts(full, product) {
			pending = append(pending, terminal)
		}
	}

	closure := Closure{}
	publications := map[string]bool{}
	sources := map[string]bool{}
	scopes := map[string]bool{}
	for name, product := range products {
		closure.Products = append(closure.Products, name)
		if strings.TrimSpace(product.Source) == "" {
			return Closure{}, fmt.Errorf("Product %q has no source", name)
		}
		sources[product.Source] = true
		for _, scope := range product.Scopes {
			if !hasScope(full, scope) {
				return Closure{}, fmt.Errorf("Catalog omits scope %q required by Product %q", scope, name)
			}
			scopes[scope] = true
		}
		if product.SnapshotPublication == "" {
			// A View product has no immutable Publication of its own; its
			// terminals carry the published rows. That is only legal when the
			// closure actually contains at least one terminal Publication.
			continue
		}
		publication, found := lookupPublication(full, product.SnapshotPublication)
		if !found {
			return Closure{}, fmt.Errorf("Catalog omits Publication %q required by Product %q",
				product.SnapshotPublication, name)
		}
		if err := validatePublicationClosure(publication, product); err != nil {
			return Closure{}, err
		}
		publications[publication.Name] = true
		sources[publication.Source] = true
	}
	for _, name := range sortedKeys(publications) {
		closure.Publications = append(closure.Publications, name)
	}
	for _, name := range sortedKeys(sources) {
		if !hasSource(full, name) {
			return Closure{}, fmt.Errorf("Catalog omits source %q", name)
		}
		closure.Sources = append(closure.Sources, name)
	}
	for _, name := range sortedKeys(scopes) {
		closure.Scopes = append(closure.Scopes, name)
	}
	sort.Strings(closure.Products)
	if len(closure.Publications) == 0 {
		return Closure{}, errors.New("closure activates no immutable Publication")
	}

	// A profile activates its whole closure, so the approval route is resolved
	// for the closure Product set rather than for one member at a time. A
	// closure with no route is recorded as unroutable instead of being hidden:
	// that is the real state of a workload whose Catalog route does not exist
	// yet, and activation refuses it.
	if policy, err := full.ResolveTaskPolicy(closure.Products); err == nil {
		closure.BudgetProfiles = []string{policy.BudgetProfile}
		closure.Routable = true
	}
	closure.SHA256 = closureDigest(closure)
	return closure, nil
}

// terminalProducts resolves the Products a View product reads through by
// matching its public scopes onto the Products that declare them natively.
func terminalProducts(full *catalog.Catalog, product catalog.Product) []string {
	if product.ViewContract == nil {
		return nil
	}
	terminals := map[string]bool{}
	for _, scope := range product.Scopes {
		field := strings.TrimSuffix(scope, "_partition_key")
		for _, candidate := range full.Products {
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

func validatePublicationClosure(publication catalog.SnapshotPublication, product catalog.Product) error {
	if publication.Snapshot != product.Snapshot {
		return fmt.Errorf("Publication %q snapshot differs from Product %q", publication.Name, product.Name)
	}
	if strings.TrimSpace(publication.OrdinalSidecar) == "" {
		return fmt.Errorf("Publication %q omits its ordinal sidecar", publication.Name)
	}
	for label, digest := range map[string]string{
		"sidecar": publication.SidecarDigest, "dictionary": publication.DictionaryDigest,
		"manifest": publication.ManifestDigest,
	} {
		if len(digest) != 64 || digest == strings.Repeat("0", 64) {
			return fmt.Errorf("Publication %q %s digest is absent or a fail-closed sentinel", publication.Name, label)
		}
	}
	return nil
}

// closureDigest is the canonical closure identity: a length-delimited,
// domain-separated encoding of the sorted closure members. Reordering,
// renaming, adding or dropping any member changes it.
func closureDigest(closure Closure) string {
	hash := sha256.New()
	hash.Write([]byte(RegistryVersion + "\x00"))
	fmt.Fprintf(hash, "%t\x00", closure.Routable)
	for _, group := range [][]string{closure.Products, closure.Publications, closure.Sources,
		closure.Scopes, closure.BudgetProfiles} {
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

// Build groups cells into profiles. Cells whose closures are identical are
// merged into one profile; no profile is ever created because two cells came
// from differently named experiments.
func Build(full *catalog.Catalog, cells []WorkloadCell, aliases map[string]string) ([]Profile, []UnresolvedCell, error) {
	if len(cells) == 0 {
		return nil, nil, errors.New("profile registry requires at least one workload cell")
	}
	byDigest := map[string]*Profile{}
	seenCells := map[string]bool{}
	var unresolved []UnresolvedCell
	for _, cell := range cells {
		if seenCells[cell.String()] {
			return nil, nil, fmt.Errorf("workload cell %s is declared twice", cell)
		}
		seenCells[cell.String()] = true
		closure, err := ComputeClosure(full, cell.Products)
		if err != nil {
			unresolved = append(unresolved, UnresolvedCell{Cell: cell.String(),
				Products: append([]string(nil), cell.Products...), Reason: err.Error()})
			continue
		}
		profile, present := byDigest[closure.SHA256]
		if !present {
			profile = &Profile{ID: ProfileID(closure.SHA256), Closure: closure,
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
		alias, named := aliases[digest]
		if !named || strings.TrimSpace(alias) == "" {
			return nil, nil, fmt.Errorf("closure %s has no reviewed alias", digest)
		}
		profile.Alias = alias
		profile.CatalogPath = "config/profiles/" + alias + ".catalog.yaml"
		profiles = append(profiles, *profile)
	}
	if err := validateAliases(profiles); err != nil {
		return nil, nil, err
	}
	sort.Slice(unresolved, func(left, right int) bool { return unresolved[left].Cell < unresolved[right].Cell })
	return profiles, unresolved, nil
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
		if profile.TotalHotBytes > profile.MaxHotLimitBytes || !profile.WithinHotLimitBytes {
			return fmt.Errorf("profile %q activates %d HOT bytes against the %d byte per-instance limit",
				profile.Alias, profile.TotalHotBytes, profile.MaxHotLimitBytes)
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
		for _, cell := range profile.Cells {
			assigned[cell]++
		}
	}
	if err := validateAliases(registry.Profiles); err != nil {
		return err
	}
	unresolved := map[string]bool{}
	for _, cell := range registry.UnresolvedCells {
		if strings.TrimSpace(cell.Reason) == "" {
			return fmt.Errorf("unresolved cell %s records no reason", cell.Cell)
		}
		if assigned[cell.Cell] > 0 {
			return fmt.Errorf("cell %s is both assigned and unresolved", cell.Cell)
		}
		unresolved[cell.Cell] = true
	}
	for _, cell := range cells {
		switch {
		case assigned[cell.String()] == 1:
		case assigned[cell.String()] > 1:
			return fmt.Errorf("workload cell %s is assigned to %d profiles", cell, assigned[cell.String()])
		case unresolved[cell.String()]:
		default:
			return fmt.Errorf("workload cell %s is neither assigned to a profile nor recorded as unresolved", cell)
		}
	}
	if len(assigned)+len(unresolved) != len(cells) {
		return fmt.Errorf("profile registry accounts for %d cells, the contract declares %d",
			len(assigned)+len(unresolved), len(cells))
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

func lookupProduct(full *catalog.Catalog, name string) (catalog.Product, bool) {
	for _, product := range full.Products {
		if product.Name == name {
			return product, true
		}
	}
	return catalog.Product{}, false
}

func lookupPublication(full *catalog.Catalog, name string) (catalog.SnapshotPublication, bool) {
	for _, publication := range full.SnapshotPublications {
		if publication.Name == name {
			return publication, true
		}
	}
	return catalog.SnapshotPublication{}, false
}

func hasScope(full *catalog.Catalog, name string) bool {
	for _, scope := range full.Scopes {
		if scope.Name == name {
			return true
		}
	}
	return false
}

func hasSource(full *catalog.Catalog, name string) bool {
	for _, source := range full.Sources {
		if source.Name == name {
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
