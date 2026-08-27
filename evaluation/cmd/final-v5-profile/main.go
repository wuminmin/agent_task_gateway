// Command final-v5-profile derives the Final-V5 deployment profiles from the
// workload cells and emits one deterministic Catalog per profile.
//
// A profile is the canonical minimal transitive Product closure a single
// Catalog-bound Gateway instance activates. Profiles are generated, never
// hand-trimmed: nothing here filters a Catalog at runtime or deletes a file.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"taskbound.local/agent-data-gateway/evaluation/finalv5contracts"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/domain"
)

const (
	// masterCatalogPath is the reviewed, contract-indexed Catalog candidate. It
	// names every Product the campaign will ever need, so a closure derived from
	// it exists even for a workload whose Product is not live yet.
	masterCatalogPath = "evaluation/final-v5-wsl2/catalog/benchmark-contract-v1.yaml"
	// liveCatalogPath is what a deployment actually runs, with generated digests.
	liveCatalogPath  = "config/catalog.yaml"
	declarationPath  = "config/profiles/workloads-v1.yaml"
	registryPath     = "config/profiles/registry.json"
	hotMeasurePath   = "config/profiles/hot-artifacts.json"
	coveragePath     = "evaluation/final-v5-wsl2/profiles/profile-coverage-v1.2.json"
	intersectionPath = "evaluation/final-v5-wsl2/profiles/product-intersection-v1.json"
	attestationPath  = "config/profiles/schema-attestations-v1.json"
	// activationSupportPath is the per-profile activation support manifest. It is
	// the only source of activation_supported; there is no global override.
	activationSupportPath = "config/profiles/activation-support-v1.json"
	profileDirectory      = "config/profiles"
)

// activationImplementationAvailable is a property of the harness, not of any
// profile: an activator, a per-profile artifact materializer, a Gateway
// activation diagnostic, a profile-bound restart and the C12/C14/C15 mechanisms
// all exist. Whether a *particular* profile has actually been activated is a
// separate, per-profile question answered by the activation support manifest
// (config/profiles/activation-support-v1.json), never by this constant.
const activationImplementationAvailable = true

// declaration is the source-controlled input for experiments that have no
// machine contract. Baseline, Scale and Artifact Product sets always come from
// the Contract Index instead.
type declaration struct {
	SchemaVersion   int                           `yaml:"schema_version"`
	RegistryVersion string                        `yaml:"registry_version"`
	Experiments     map[string][]declaredWorkload `yaml:"experiments"`
	Aliases         map[string]string             `yaml:"aliases"`
}

type declaredWorkload struct {
	WorkloadID string   `yaml:"workload_id"`
	Scales     []string `yaml:"scales"`
	Modes      []string `yaml:"modes"`
	Products   []string `yaml:"products"`
}

func main() {
	root := flag.String("root", ".", "repository root")
	verifyOnly := flag.Bool("verify", false, "verify the generated profiles instead of rewriting them")
	printClosures := flag.Bool("print-closures", false, "print every derived closure digest for alias review")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "positional arguments are not accepted")
		os.Exit(2)
	}
	if *printClosures {
		if err := printDerivedClosures(*root); err != nil {
			fmt.Fprintln(os.Stderr, "final-v5-profile:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(*root, *verifyOnly); err != nil {
		fmt.Fprintln(os.Stderr, "final-v5-profile:", err)
		os.Exit(1)
	}
}

func run(root string, verifyOnly bool) error {
	master, err := catalog.Load(filepath.Join(root, masterCatalogPath))
	if err != nil {
		return fmt.Errorf("load master Catalog candidate: %w", err)
	}
	live, err := catalog.Load(filepath.Join(root, liveCatalogPath))
	if err != nil {
		return fmt.Errorf("load live Catalog: %w", err)
	}
	cells, declared, err := workloadCells(root)
	if err != nil {
		return err
	}
	hot, err := loadHotArtifacts(root)
	if err != nil {
		return err
	}
	attestations, err := loadAttestations(root)
	if err != nil {
		return err
	}
	support, err := loadActivationSupport(root)
	if err != nil {
		return err
	}
	profileCatalogs, err := loadProfileCatalogs(root, declared.Aliases)
	if err != nil {
		return err
	}
	// Build leaves activation_supported false for every profile. It is applied
	// below, once each profile Catalog has been materialized and its digest is
	// known, so a claim can be rejected when it names a different Catalog.
	profiles, err := finalv5profile.Build(finalv5profile.BuildInput{Master: master, Live: live,
		ProfileCatalogs: profileCatalogs, Cells: cells, Aliases: declared.Aliases, Hot: hot,
		ActivationSupported: false})
	if err != nil {
		return err
	}
	for index := range profiles {
		deploymentCatalog := live
		// Once a profile Catalog exists, keep projecting that independently
		// activatable declaration. Adding the same closure to the master Catalog
		// must not silently rewrite the already activated profile Catalog from
		// unrelated shared master metadata and invalidate its activation evidence.
		// Build still derives materializability and live-route status from the
		// master Catalog first; this choice only preserves generated profile bytes.
		//
		// The preservation is scoped to what it protects. A profile that has
		// never been activated has no activation evidence to invalidate, and
		// freezing its bytes only preserves a projection of a Catalog that has
		// since moved -- which is how the analytics-orders profile came to carry
		// a five-hundred-row ceiling its own workload cells cannot run under.
		// Such a profile is reprojected from the live Catalog.
		if profileCatalog, found := profileCatalogs[profiles[index].Closure.SHA256]; found {
			if activated, known := support[profiles[index].ID]; known && activated.ActivationSmokePassed {
				// Preserve the independently activated profile declaration, but refresh
				// budgets for route identities that still exist verbatim in the live
				// Catalog. Activation protects unrelated Catalog bytes; it must not
				// freeze an obsolete cumulative budget after the live route was raised
				// to cover another experiment sharing the same Product closure.
				deploymentCatalog = refreshMatchingRouteBudgets(profileCatalog, live)
			} else {
				// A never-activated profile is reprojected from the live Catalog,
				// but a reviewed profile-only route (the deliberately narrow
				// three-Product ProvSQL route, which the live Catalog does not
				// carry) is part of the profile declaration, not stale
				// projection. A release freeze resets every activation, so
				// without this the freeze itself would silently drop the route.
				deploymentCatalog = carryProfileOnlyRoutes(live, profileCatalog)
			}
		}
		if err := materializeProfile(root, deploymentCatalog, &profiles[index], hot, attestations,
			verifyOnly); err != nil {
			return err
		}
	}
	finalv5profile.ApplyActivationSupport(profiles, support)
	registry := finalv5profile.Registry{SchemaVersion: 1, RegistryVersion: finalv5profile.RegistryVersion,
		ContractRelease: contractRelease(), MaxHotLimitBytes: finalv5profile.MaxHotBytesPerInstance,
		HotLimitScope: finalv5profile.HotLimitScope, Profiles: profiles}
	if err := finalv5profile.ValidateRegistry(registry, cells); err != nil {
		return err
	}
	if err := writeCanonicalJSON(filepath.Join(root, registryPath), registry, verifyOnly); err != nil {
		return err
	}
	if err := writeCanonicalJSON(filepath.Join(root, coveragePath), buildCoverage(registry, cells), verifyOnly); err != nil {
		return err
	}
	return writeCanonicalJSON(filepath.Join(root, intersectionPath), buildIntersection(registry), verifyOnly)
}

// refreshMatchingRouteBudgets uses the live Catalog as the budget derivation
// input only when a profile route has the same sensitivity, exact Product set,
// approval mode, approver and budget-profile name. A profile-only route (for
// example the deliberately narrow ProvSQL three-Product route) is left intact.
// This keeps activation preservation from becoming a stale-budget cache without
// allowing a live default route to replace a reviewed profile-scoped route.
func refreshMatchingRouteBudgets(profileCatalog, live *catalog.Catalog) *catalog.Catalog {
	refreshed := *profileCatalog
	refreshed.BudgetProfiles = append([]catalog.BudgetProfile(nil), profileCatalog.BudgetProfiles...)
	for _, route := range profileCatalog.ApprovalRoutes {
		if !matchingRouteExists(live, route) {
			continue
		}
		liveBudget, found := live.LookupBudgetProfile(route.BudgetProfile)
		if !found {
			continue
		}
		for index := range refreshed.BudgetProfiles {
			if refreshed.BudgetProfiles[index].Name == route.BudgetProfile {
				refreshed.BudgetProfiles[index] = liveBudget
				break
			}
		}
	}
	return &refreshed
}

func matchingRouteExists(document *catalog.Catalog, want catalog.ApprovalRoute) bool {
	for _, candidate := range document.ApprovalRoutes {
		if candidate.Sensitivity == want.Sensitivity && candidate.Mode == want.Mode &&
			candidate.Approver == want.Approver && candidate.BudgetProfile == want.BudgetProfile &&
			sameStrings(candidate.Products, want.Products) {
			return true
		}
	}
	return false
}

// carryProfileOnlyRoutes returns the live Catalog extended with every
// product-scoped route of the profile Catalog whose exact Product set has no
// route in the live Catalog, together with the budget profile that route names.
// A live route for the same Product set always wins, so a reviewed live change
// is never overridden by a stale profile projection.
func carryProfileOnlyRoutes(live, profileCatalog *catalog.Catalog) *catalog.Catalog {
	extended := *live
	extended.ApprovalRoutes = append([]catalog.ApprovalRoute(nil), live.ApprovalRoutes...)
	extended.BudgetProfiles = append([]catalog.BudgetProfile(nil), live.BudgetProfiles...)
	for _, route := range profileCatalog.ApprovalRoutes {
		if len(route.Products) == 0 || productRouteExists(live, route.Products) {
			continue
		}
		extended.ApprovalRoutes = append(extended.ApprovalRoutes, route)
		if _, found := extended.LookupBudgetProfile(route.BudgetProfile); found {
			continue
		}
		if budget, found := profileCatalog.LookupBudgetProfile(route.BudgetProfile); found {
			extended.BudgetProfiles = append(extended.BudgetProfiles, budget)
		}
	}
	return &extended
}

func productRouteExists(document *catalog.Catalog, products []string) bool {
	for _, candidate := range document.ApprovalRoutes {
		if len(candidate.Products) > 0 && sameStrings(candidate.Products, products) {
			return true
		}
	}
	return false
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	left = append([]string(nil), left...)
	right = append([]string(nil), right...)
	sort.Strings(left)
	sort.Strings(right)
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// loadProfileCatalogs discovers source-controlled per-profile Catalogs by the
// reviewed alias map. A Catalog is keyed by the closure it actually declares,
// not merely by its filename, so an alias/file mismatch cannot authorize a
// different Product set. Missing files are legitimate for unresolved profiles
// and for the initial bootstrap from the default live Catalog.
func loadProfileCatalogs(root string, aliases map[string]string) (map[string]*catalog.Catalog, error) {
	loaded := make(map[string]*catalog.Catalog)
	for closureSHA, alias := range aliases {
		path := filepath.Join(root, profileDirectory, alias+".catalog.yaml")
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return nil, fmt.Errorf("inspect profile Catalog %q: %w", alias, err)
		}
		document, err := catalog.Load(path)
		if err != nil {
			return nil, fmt.Errorf("load profile Catalog %q: %w", alias, err)
		}
		products := make([]string, 0, len(document.Products))
		for _, product := range document.Products {
			products = append(products, product.Name)
		}
		closure, reasons, err := finalv5profile.ComputeClosure(document, products)
		if err != nil {
			return nil, fmt.Errorf("profile Catalog %q closure: %w", alias, err)
		}
		if len(reasons) != 0 {
			return nil, fmt.Errorf("profile Catalog %q has an incomplete closure: %+v", alias, reasons)
		}
		if closure.SHA256 != closureSHA {
			return nil, fmt.Errorf("profile Catalog %q declares closure %s, reviewed alias maps to %s",
				alias, closure.SHA256, closureSHA)
		}
		loaded[closureSHA] = document
	}
	return loaded, nil
}

// coverageReport is the source-controlled cell completeness audit. Every count
// is derived from the registry; nothing here is hand written.
type coverageReport struct {
	SchemaVersion    int    `json:"schema_version"`
	ContractsVersion string `json:"contracts_version"`
	RegistryVersion  string `json:"registry_version"`
	// CoverageScope names the denominator the mapping counts apply to. The
	// profile mapping invariant is about Catalog-bound cells; control-only and
	// exempt cells are counted separately rather than omitted.
	CoverageScope            string            `json:"coverage_scope"`
	AllFormalCells           int               `json:"all_formal_cells"`
	CatalogBoundCells        int               `json:"catalog_bound_cells"`
	ControlOnlyCells         int               `json:"control_only_cells"`
	ProfileExemptCells       int               `json:"catalog_profile_exempt_cells"`
	MappedCatalogBoundCells  int               `json:"mapped_catalog_bound_cells"`
	MissingCatalogBoundCells int               `json:"missing_catalog_bound_cells"`
	DuplicateCatalogBound    int               `json:"duplicate_catalog_bound_cells"`
	RoutableCells            int               `json:"routable_cells"`
	TargetedRunEligibleCells int               `json:"targeted_run_eligible_cells"`
	UnresolvedCells          int               `json:"unresolved_cells"`
	DuplicateCellIDs         []string          `json:"duplicate_cell_ids"`
	MissingCellIDs           []string          `json:"missing_cell_ids"`
	ControlOnlyCellIDs       []string          `json:"control_only_cell_ids"`
	Profiles                 []coverageProfile `json:"profiles"`
}

type coverageProfile struct {
	ProfileID           string                            `json:"profile_id"`
	Alias               string                            `json:"alias"`
	ClosureSHA256       string                            `json:"closure_sha256"`
	Routable            bool                              `json:"routable"`
	TargetedRunEligible bool                              `json:"targeted_run_eligible"`
	Status              finalv5profile.ProfileStatus      `json:"status"`
	UnresolvedReason    []finalv5profile.UnresolvedReason `json:"unresolved_reason"`
	CatalogSHA256       string                            `json:"catalog_sha256"`
	TotalHotBytes       int64                             `json:"total_hot_bytes"`
	Cells               []string                          `json:"cells"`
}

func buildCoverage(registry finalv5profile.Registry, cells []finalv5profile.WorkloadCell) coverageReport {
	report := coverageReport{SchemaVersion: 1, ContractsVersion: registry.ContractRelease,
		RegistryVersion: registry.RegistryVersion, CoverageScope: "catalog_bound_workload_cells",
		AllFormalCells: len(cells), DuplicateCellIDs: []string{}, MissingCellIDs: []string{},
		ControlOnlyCellIDs: []string{}}
	for _, cell := range cells {
		switch cell.ProfileRequirement {
		case finalv5profile.RequirementCatalogBound:
			report.CatalogBoundCells++
		case finalv5profile.RequirementControlOnly:
			report.ControlOnlyCells++
			report.ControlOnlyCellIDs = append(report.ControlOnlyCellIDs, cell.String())
		case finalv5profile.RequirementProfileExempt:
			report.ProfileExemptCells++
		}
	}
	sort.Strings(report.ControlOnlyCellIDs)
	seen := map[string]int{}
	for _, profile := range registry.Profiles {
		entry := coverageProfile{ProfileID: profile.ID, Alias: profile.Alias,
			ClosureSHA256: profile.Closure.SHA256, Routable: profile.Routable,
			TargetedRunEligible: profile.TargetedRunEligible, Status: profile.Status,
			UnresolvedReason: profile.Status.UnresolvedReasons, CatalogSHA256: profile.CatalogSHA256,
			TotalHotBytes: profile.TotalHotBytes, Cells: profile.Cells}
		report.Profiles = append(report.Profiles, entry)
		report.MappedCatalogBoundCells += len(profile.Cells)
		if profile.Routable {
			report.RoutableCells += len(profile.Cells)
		} else {
			report.UnresolvedCells += len(profile.Cells)
		}
		if profile.TargetedRunEligible {
			report.TargetedRunEligibleCells += len(profile.Cells)
		}
		for _, cell := range profile.Cells {
			seen[cell]++
		}
	}
	for _, cell := range sortedKeys(seen) {
		if seen[cell] > 1 {
			report.DuplicateCellIDs = append(report.DuplicateCellIDs, cell)
		}
	}
	for _, cell := range cells {
		if cell.ProfileRequirement == finalv5profile.RequirementCatalogBound && seen[cell.String()] == 0 {
			report.MissingCellIDs = append(report.MissingCellIDs, cell.String())
		}
	}
	sort.Strings(report.MissingCellIDs)
	report.DuplicateCatalogBound = len(report.DuplicateCellIDs)
	report.MissingCatalogBoundCells = len(report.MissingCellIDs)
	return report
}

// intersectionReport answers, from the registry alone, whether any two
// deployment-ready profiles share a Product. A shared Product is what makes a
// live same-query cross-profile test constructible; without one, that test is
// not applicable rather than merely unfinished.
type intersectionReport struct {
	SchemaVersion    int                `json:"schema_version"`
	Record           string             `json:"record"`
	ContractsVersion string             `json:"contracts_version"`
	RegistryVersion  string             `json:"registry_version"`
	ProfileCount     int                `json:"profile_count"`
	PairCount        int                `json:"pair_count"`
	OverlappingPairs int                `json:"overlapping_pair_count"`
	Pairs            []intersectionPair `json:"pairs"`
}

type intersectionPair struct {
	LeftProfileID               string   `json:"left_profile_id"`
	RightProfileID              string   `json:"right_profile_id"`
	LeftAlias                   string   `json:"left_alias"`
	RightAlias                  string   `json:"right_alias"`
	LeftProducts                []string `json:"left_products"`
	RightProducts               []string `json:"right_products"`
	Intersection                []string `json:"intersection"`
	IntersectionCount           int      `json:"intersection_count"`
	SameQueryLiveTestApplicable bool     `json:"same_query_live_test_applicable"`
}

func buildIntersection(registry finalv5profile.Registry) intersectionReport {
	var live []finalv5profile.Profile
	for _, profile := range registry.Profiles {
		if profile.Status.ClosureComplete && profile.Status.CatalogMaterializable &&
			profile.Status.LiveRouteAvailable {
			live = append(live, profile)
		}
	}
	sort.Slice(live, func(a, b int) bool { return live[a].ID < live[b].ID })
	report := intersectionReport{SchemaVersion: 1,
		Record: "taskgate-final-v5-product-intersection-v1", ContractsVersion: registry.ContractRelease,
		RegistryVersion: registry.RegistryVersion, ProfileCount: len(live), Pairs: []intersectionPair{}}
	for left := 0; left < len(live); left++ {
		for right := left + 1; right < len(live); right++ {
			shared := intersect(live[left].Closure.Products, live[right].Closure.Products)
			pair := intersectionPair{LeftProfileID: live[left].ID, RightProfileID: live[right].ID,
				LeftAlias: live[left].Alias, RightAlias: live[right].Alias,
				LeftProducts: live[left].Closure.Products, RightProducts: live[right].Closure.Products,
				Intersection: shared, IntersectionCount: len(shared),
				SameQueryLiveTestApplicable: len(shared) > 0}
			if pair.SameQueryLiveTestApplicable {
				report.OverlappingPairs++
			}
			report.Pairs = append(report.Pairs, pair)
		}
	}
	report.PairCount = len(report.Pairs)
	return report
}

func intersect(left, right []string) []string {
	present := map[string]bool{}
	for _, value := range right {
		present[value] = true
	}
	shared := []string{}
	for _, value := range left {
		if present[value] {
			shared = append(shared, value)
		}
	}
	sort.Strings(shared)
	return shared
}

func writeCanonicalJSON(path string, value any, verifyOnly bool) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if verifyOnly {
		return compareBytes(path, encoded)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, encoded, 0o644)
}

// printDerivedClosures reports every distinct closure so a reviewer can assign
// its alias. Aliases are review output, not an input the tool may invent.
func printDerivedClosures(root string) error {
	master, err := catalog.Load(filepath.Join(root, masterCatalogPath))
	if err != nil {
		return err
	}
	live, err := catalog.Load(filepath.Join(root, liveCatalogPath))
	if err != nil {
		return err
	}
	hot, err := loadHotArtifacts(root)
	if err != nil {
		return err
	}
	cells, _, err := workloadCells(root)
	if err != nil {
		return err
	}
	type group struct {
		closure finalv5profile.Closure
		status  finalv5profile.ProfileStatus
		cells   []string
	}
	groups := map[string]*group{}
	for _, cell := range cells {
		closure, reasons, err := finalv5profile.ComputeClosure(master, cell.Products)
		if err != nil {
			return err
		}
		entry, present := groups[closure.SHA256]
		if !present {
			// Closure review output. Activation support is a per-profile
			// evidence question that this listing deliberately does not answer.
			status, _ := finalv5profile.EvaluateStatus(closure, reasons, live, hot, false)
			entry = &group{closure: closure, status: status}
			groups[closure.SHA256] = entry
		}
		entry.cells = append(entry.cells, cell.String())
	}
	for _, digest := range sortedKeys(groups) {
		entry := groups[digest]
		fmt.Printf("%s  cells=%d closure=%t materializable=%t route=%t activation=%t\n  products=%v\n  publications=%v\n  example=%s\n",
			digest, len(entry.cells), entry.status.ClosureComplete, entry.status.CatalogMaterializable,
			entry.status.LiveRouteAvailable, entry.status.ActivationSupported,
			entry.closure.Products, entry.closure.Publications, entry.cells[0])
	}
	return nil
}

func contractRelease() string {
	runtime, err := finalv5contracts.LoadRuntime()
	if err != nil {
		return ""
	}
	return runtime.ContractRelease()
}

// workloadCells unions the contract-derived cells with the declared cells of
// the experiments that have no machine contract.
func workloadCells(root string) ([]finalv5profile.WorkloadCell, declaration, error) {
	var declared declaration
	value, err := os.ReadFile(filepath.Join(root, declarationPath))
	if err != nil {
		return nil, declared, fmt.Errorf("read profile workload declaration: %w", err)
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(value)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&declared); err != nil {
		return nil, declared, fmt.Errorf("decode profile workload declaration: %w", err)
	}
	if declared.SchemaVersion != 1 || declared.RegistryVersion != finalv5profile.RegistryVersion {
		return nil, declared, errors.New("profile workload declaration header is invalid")
	}
	runtime, err := finalv5contracts.LoadRuntime()
	if err != nil {
		return nil, declared, fmt.Errorf("contract bridge: %w", err)
	}
	contractCells, err := runtime.ContractWorkloadCells()
	if err != nil {
		return nil, declared, err
	}
	cells := make([]finalv5profile.WorkloadCell, 0, len(contractCells))
	for _, contractCell := range contractCells {
		cell := finalv5profile.WorkloadCell{
			ExperimentID: contractCell.Identity.ExperimentID, WorkloadID: contractCell.Identity.WorkloadID,
			Scale: contractCell.Identity.Scale, Mode: contractCell.Identity.Mode,
			Products: contractCell.Products, ProfileRequirement: finalv5profile.RequirementCatalogBound}
		if len(contractCell.Products) == 0 {
			// A cell that requests no Catalog Product never binds a Gateway
			// Catalog. It is classified rather than dropped, so the coverage
			// denominator stays honest.
			cell.ProfileRequirement = finalv5profile.RequirementControlOnly
			cell.RequirementReason = "the contract cell requests no Catalog Product; it runs on Control-side or kernel-only paths"
		}
		cells = append(cells, cell)
	}
	for _, experiment := range sortedKeys(declared.Experiments) {
		for _, workload := range declared.Experiments[experiment] {
			if len(workload.Products) == 0 || len(workload.Scales) == 0 || len(workload.Modes) == 0 {
				return nil, declared, fmt.Errorf("declared workload %s/%s is incomplete", experiment, workload.WorkloadID)
			}
			for _, scale := range workload.Scales {
				for _, mode := range workload.Modes {
					cells = append(cells, finalv5profile.WorkloadCell{ExperimentID: experiment,
						WorkloadID: workload.WorkloadID, Scale: scale, Mode: mode,
						Products: workload.Products, ProfileRequirement: finalv5profile.RequirementCatalogBound})
				}
			}
		}
	}
	return cells, declared, nil
}

// materializeProfile writes the deterministic minimal Catalog of one profile
// and records its live digest and HOT artifact sizes.
// loadAttestations reads the reviewed per-profile schema attestations. The file
// is optional only so a first bootstrap can generate profile Catalogs before a
// live deployment exists to attest them; a test requires every materializable
// profile to be attested in the committed tree.
// loadActivationSupport reads the per-profile activation support manifest. An
// absent manifest is not an error and is not an escape hatch: it yields an empty
// support set, so every profile stays unsupported until a live smoke is recorded.
func loadActivationSupport(root string) (map[string]finalv5profile.ProfileActivationSupport, error) {
	value, err := os.ReadFile(filepath.Join(root, activationSupportPath))
	if os.IsNotExist(err) {
		return map[string]finalv5profile.ProfileActivationSupport{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read activation support manifest: %w", err)
	}
	support, err := finalv5profile.DecodeActivationSupport(value)
	if err != nil {
		return nil, err
	}
	if support.ContractRelease != contractRelease() {
		return nil, fmt.Errorf("activation support manifest pins contract %s, this tree is %s",
			support.ContractRelease, contractRelease())
	}
	return support.SupportedProfiles()
}

func loadAttestations(root string) (finalv5profile.SchemaAttestationRegistry, error) {
	var registry finalv5profile.SchemaAttestationRegistry
	value, err := os.ReadFile(filepath.Join(root, attestationPath))
	if os.IsNotExist(err) {
		return registry, nil
	}
	if err != nil {
		return registry, err
	}
	if err := json.Unmarshal(value, &registry); err != nil {
		return registry, err
	}
	return registry, registry.Validate()
}

func materializeProfile(root string, full *catalog.Catalog, profile *finalv5profile.Profile,
	hot map[string]finalv5profile.HotArtifact,
	attestations finalv5profile.SchemaAttestationRegistry, verifyOnly bool) error {
	if !profile.Status.CatalogMaterializable {
		// A profile whose closure the live Catalog cannot publish yet stays a
		// first-class registry entry, but it must not get a generated Catalog:
		// emitting one would claim a deployment can activate it.
		profile.CatalogPath, profile.CatalogSHA256 = "", ""
		profile.HotArtifacts, profile.TotalHotBytes = nil, 0
		profile.WithinHotLimitBytes = false
		return nil
	}
	document := projectCatalog(full, profile.Closure)
	// A profile Catalog declares only its own closure, so it must carry the
	// attestation of exactly those reporting views rather than the full
	// Catalog's, which covers every Product.
	if attestation, found := attestations.Lookup(profile.ID); found {
		for index := range document.Sources {
			document.Sources[index].SchemaDigest = attestation.SchemaDigest
		}
	}
	encoded, err := yaml.Marshal(document)
	if err != nil {
		return err
	}
	header := fmt.Sprintf(""+
		"# GENERATED by evaluation/cmd/final-v5-profile. Do not edit by hand.\n"+
		"#\n"+
		"# Final-V5 deployment profile %q.\n"+
		"# profile_id:     %s\n"+
		"# closure_sha256: %s\n"+
		"#\n"+
		"# This is the canonical minimal transitive Product closure required by\n"+
		"# the workload cells assigned to this profile. One Catalog-bound Gateway\n"+
		"# instance activates exactly this closure and nothing else.\n",
		profile.Alias, profile.ID, profile.Closure.SHA256)
	encoded = append([]byte(header), encoded...)
	path := filepath.Join(root, profile.CatalogPath)
	if verifyOnly {
		if err := compareBytes(path, encoded); err != nil {
			return err
		}
	} else if err := os.WriteFile(path, encoded, 0o644); err != nil {
		return err
	}
	loaded, err := catalog.Load(path)
	if err != nil {
		return fmt.Errorf("generated profile Catalog %q is not startup-safe: %w", profile.Alias, err)
	}
	profile.CatalogSHA256 = loaded.SHA256
	profile.HotArtifacts = nil
	profile.TotalHotBytes = 0
	for _, publication := range profile.Closure.Publications {
		artifact, measured := hot[publication]
		if !measured {
			return fmt.Errorf("no observed HOT artifact size for Publication %q", publication)
		}
		artifact.Publication = publication
		profile.HotArtifacts = append(profile.HotArtifacts, artifact)
		profile.TotalHotBytes += artifact.Bytes
	}
	profile.WithinHotLimitBytes = profile.TotalHotBytes <= profile.MaxHotLimitBytes
	if !profile.WithinHotLimitBytes {
		return fmt.Errorf("profile %q activates %d HOT bytes against the %d byte %s",
			profile.Alias, profile.TotalHotBytes, profile.MaxHotLimitBytes, finalv5profile.HotLimitScope)
	}
	return nil
}

// projectCatalog keeps only the closure members, in the full Catalog's own
// order, so a generated profile Catalog is a projection rather than a rewrite.
func projectCatalog(full *catalog.Catalog, closure finalv5profile.Closure) *catalog.Catalog {
	document := &catalog.Catalog{CatalogVersion: full.CatalogVersion}
	for _, source := range full.Sources {
		if contains(closure.Sources, source.Name) {
			document.Sources = append(document.Sources, source)
		}
	}
	for _, publication := range full.SnapshotPublications {
		if contains(closure.Publications, publication.Name) {
			document.SnapshotPublications = append(document.SnapshotPublications, publication)
		}
	}
	for _, scope := range full.Scopes {
		if contains(closure.Scopes, scope.Name) {
			document.Scopes = append(document.Scopes, scope)
		}
	}
	for _, product := range full.Products {
		if contains(closure.Products, product.Name) {
			document.Products = append(document.Products, product)
		}
	}
	// A startup-safe Catalog needs a sensitivity default route for every
	// sensitivity its Products carry, in addition to the exact product-scoped
	// route the closure resolves through. Both are transitive dependencies.
	sensitivities := map[domain.Sensitivity]bool{}
	for _, product := range document.Products {
		sensitivities[product.Sensitivity] = true
	}
	var budgets []string
	for _, route := range full.ApprovalRoutes {
		keep := false
		switch {
		case len(route.Products) == 0:
			keep = sensitivities[route.Sensitivity]
		default:
			keep = true
			for _, product := range route.Products {
				if !contains(closure.Products, product) {
					keep = false
					break
				}
			}
		}
		if !keep {
			continue
		}
		document.ApprovalRoutes = append(document.ApprovalRoutes, route)
		if !contains(budgets, route.BudgetProfile) {
			budgets = append(budgets, route.BudgetProfile)
		}
	}
	for _, profile := range full.BudgetProfiles {
		if contains(budgets, profile.Name) {
			document.BudgetProfiles = append(document.BudgetProfiles, profile)
		}
	}
	return document
}

func loadHotArtifacts(root string) (map[string]finalv5profile.HotArtifact, error) {
	value, err := os.ReadFile(filepath.Join(root, hotMeasurePath))
	if err != nil {
		return nil, fmt.Errorf("read observed HOT artifact sizes: %w", err)
	}
	var document struct {
		SchemaVersion int                                   `json:"schema_version"`
		Observed      map[string]finalv5profile.HotArtifact `json:"observed"`
	}
	if err := json.Unmarshal(value, &document); err != nil {
		return nil, fmt.Errorf("decode observed HOT artifact sizes: %w", err)
	}
	if document.SchemaVersion != 1 || len(document.Observed) == 0 {
		return nil, errors.New("observed HOT artifact record is invalid")
	}
	return document.Observed, nil
}

func compareBytes(path string, want []byte) error {
	actual, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if string(actual) != string(want) {
		return fmt.Errorf("%s differs from its deterministic regeneration", path)
	}
	return nil
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
