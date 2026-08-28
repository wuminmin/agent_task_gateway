package main

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/internal/catalog"
)

type capabilityManifestWorkload struct {
	ID     string   `yaml:"id"`
	Scales []string `yaml:"scales"`
	Modes  []string `yaml:"modes"`
}

type capabilityManifestProfile struct {
	Workloads []capabilityManifestWorkload `yaml:"workloads"`
}

func loadProtocolPublicationProfile(t *testing.T, names ...string) []publicationCell {
	t.Helper()
	var manifest struct {
		Profiles map[string]capabilityManifestProfile `yaml:"profiles"`
	}
	raw, err := os.ReadFile("../../final-v5-wsl2/protocol/workloads-v1.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	var cells []publicationCell
	for _, name := range names {
		profile, ok := manifest.Profiles[name]
		if !ok {
			t.Fatalf("frozen protocol has no %s profile", name)
		}
		workloads := make([]publicationWorkload, 0, len(profile.Workloads))
		for _, workload := range profile.Workloads {
			workloads = append(workloads, publicationWorkload{ID: workload.ID, Scales: workload.Scales, Modes: workload.Modes})
		}
		cells = append(cells, expandPublicationWorkloads(workloads)...)
	}
	return cells
}

func TestEveryPublicationRequirementMatchesFrozenProtocol(t *testing.T) {
	tests := []struct {
		experimentID string
		profiles     []string
		required     []publicationCell
		cellCount    int
	}{
		{experimentID: "baseline", profiles: []string{"baseline"}, required: baselinePublicationRequirements, cellCount: 58},
		// scale-extreme left the frozen protocol in v1.11 (172-cell scope, 2026-08-27).
		{experimentID: "scale", profiles: []string{"scale"}, required: scalePublicationRequirements, cellCount: 60},
		{experimentID: "artifact", profiles: []string{"artifact"}, required: artifactPublicationRequirements, cellCount: 6},
		{experimentID: "rls", profiles: []string{"rls"}, required: rlsPublicationRequirements, cellCount: 6},
		{experimentID: "attack", profiles: []string{"attack"}, required: attackPublicationRequirements, cellCount: 15},
		{experimentID: "provsql", profiles: []string{"provsql"}, required: provSQLPublicationRequirements, cellCount: 9},
		{experimentID: "compiler", profiles: []string{"compiler"}, required: compilerPublicationRequirements, cellCount: 11},
		{experimentID: "concurrency", profiles: []string{"concurrency"}, required: concurrencyPublicationRequirements, cellCount: 5},
		{experimentID: "rq5", profiles: []string{"rq5"}, required: rq5PublicationRequirements, cellCount: 2},
	}
	for _, test := range tests {
		t.Run(test.experimentID, func(t *testing.T) {
			want := loadProtocolPublicationProfile(t, test.profiles...)
			if len(want) != test.cellCount {
				t.Fatalf("frozen profile contains %d cells, want %d", len(want), test.cellCount)
			}
			assertSamePublicationCells(t, test.required, want)
		})
	}
}

func TestEveryExperimentHasFailClosedPublicationCoverageGate(t *testing.T) {
	if len(publicationCoverageGates) != len(experimentIDs) {
		t.Fatalf("coverage gate count = %d, want %d", len(publicationCoverageGates), len(experimentIDs))
	}
	for _, experimentID := range experimentIDs {
		if _, gated := publicationCoverageGates[experimentID]; !gated {
			t.Fatalf("experiment %q has no publication coverage gate", experimentID)
		}
	}
	if publicationCoverageGateSatisfied("future-experiment-without-a-gate") {
		t.Fatal("an experiment without an exact profile gate was accepted")
	}
}

func TestTrueCapabilitiesDeriveEveryCellFromRealAcceptedDefinitions(t *testing.T) {
	for _, experimentID := range []string{"scale", "rls", "attack", "provsql", "compiler", "concurrency", "rq5"} {
		t.Run(experimentID, func(t *testing.T) {
			coverage := publicationCoverageGates[experimentID]
			if !coverage.complete() {
				t.Fatalf("real publication coverage is incomplete: %d/%d", len(coverage.implemented), len(coverage.required))
			}
			assertSamePublicationCells(t, coverage.implemented, coverage.required)
			for _, cell := range coverage.required {
				if !realPublicationCellImplemented(experimentID, cell) {
					t.Fatalf("protocol cell is not accepted by the real handler or fixture: %+v", cell)
				}
			}
			if realPublicationCellImplemented(experimentID, publicationCell{
				WorkloadID: "not-preregistered", Scale: "not-preregistered", Mode: "not-preregistered",
			}) {
				t.Fatal("an unsupported cell was reported as implemented")
			}
		})
	}
}

func TestMissingGateOrFutureCellDisablesCapability(t *testing.T) {
	const experimentID = "rq5"
	original := publicationCoverageGates[experimentID]
	t.Cleanup(func() { publicationCoverageGates[experimentID] = original })

	delete(publicationCoverageGates, experimentID)
	if implementedCapabilities()[experimentID] {
		t.Fatal("capability stayed true after its exact profile gate was removed")
	}

	missing := original
	missing.implemented = append([]publicationCell(nil), original.implemented[:len(original.implemented)-1]...)
	publicationCoverageGates[experimentID] = missing
	if implementedCapabilities()[experimentID] {
		t.Fatal("capability stayed true after a real implemented cell was removed")
	}

	future := original
	future.required = append(append([]publicationCell(nil), original.required...), publicationCell{
		WorkloadID: "daily-publication-v6", Scale: "690000", Mode: "build_verify_activate",
	})
	publicationCoverageGates[experimentID] = future
	if implementedCapabilities()[experimentID] {
		t.Fatal("capability stayed true after the protocol gained an unimplemented future cell")
	}
}

func TestBaselineRetainedRunEnablesCapability(t *testing.T) {
	// Resolution alone must never satisfy the gate. Every frozen cell resolves,
	// but author-approved retained-run evidence is independently required.
	if len(baselineImplementedPublicationCells) != len(baselinePublicationRequirements) {
		t.Fatalf("%d of %d frozen Baseline cells resolve; source parseability is necessary before applying retained-run evidence",
			len(baselineImplementedPublicationCells), len(baselinePublicationRequirements))
	}
	if !baselineRealSystemValidated {
		t.Fatal("baselineRealSystemValidated is false after author approval of the retained 58-cell run")
	}
	if !publicationCoverageGates["baseline"].complete() {
		t.Fatal("Baseline coverage stayed incomplete after the retained run covered every frozen cell")
	}
	if !implementedCapabilities()["baseline"] {
		t.Fatal("baseline capability stayed false after resolution and retained-run evidence both completed")
	}
}

// Every Baseline cell credited by the retained run must still resolve through
// the source-controlled execution path; the evidence bit cannot substitute a
// query for a frozen cell that the Adapter no longer implements.
func TestEveryValidatedBaselineCellResolves(t *testing.T) {
	assertSamePublicationCells(t, baselineImplementedPublicationCells, baselinePublicationRequirements)
	for _, cell := range baselinePublicationRequirements {
		if _, err := resolveBaselineExecutionCell(experiment.AdapterOperation{
			ExperimentID: "baseline", WorkloadID: cell.WorkloadID,
			Scale: cell.Scale, Mode: cell.Mode,
		}); err != nil {
			t.Fatalf("validated cell %+v does not resolve: %v", cell, err)
		}
	}
}

func TestScaleRetainedRunsEnableCompleteCapability(t *testing.T) {
	coverage := publicationCoverageGates["scale"]
	if !scaleRealSystemValidated {
		t.Fatal("scaleRealSystemValidated is false after author approval of two retained 24-cell runs")
	}
	if !coverage.complete() {
		t.Fatalf("Scale profile is incomplete after retained-run approval: %d implemented of %d required",
			len(coverage.implemented), len(coverage.required))
	}
	assertSamePublicationCells(t, coverage.implemented, scalePublicationRequirements)

	dependencyCells := 0
	for _, cell := range coverage.implemented {
		if cell.WorkloadID == "dependency-e2e" {
			dependencyCells++
		}
		if !realPublicationCellImplemented("scale", cell) {
			t.Fatalf("validated Scale cell no longer resolves through the real dispatch predicate: %+v", cell)
		}
	}
	if dependencyCells != 24 {
		t.Fatalf("Scale registry contains %d dependency-e2e cells, want 24", dependencyCells)
	}
	if len(coverage.implemented) != 62 {
		t.Fatalf("Scale registry contains %d cells, want the complete 62-cell profile", len(coverage.implemented))
	}
	if !implementedCapabilities()["scale"] {
		t.Fatal("Scale capability stayed false after source resolution and retained-run evidence both completed")
	}
}

// TestScaleCellsMatchTheDispatchTheyClaim pins the registration to the
// same predicate scale.go dispatches on. A cell advertised here that Execute
// would refuse is worse than one left unadvertised.
func TestScaleCellsMatchTheDispatchTheyClaim(t *testing.T) {
	adapter := &scaleAdapter{}
	for _, cell := range publicationCoverageGates["scale"].implemented {
		sample := adapter.Execute(t.Context(), experiment.AdapterOperation{
			ExperimentID: "scale", WorkloadID: cell.WorkloadID, Scale: cell.Scale, Mode: cell.Mode,
		})
		// The cell must get past dispatch. It still fails on the deployment
		// binding, which is environment rather than implementation, so the one
		// code it must never return is the dispatch refusal.
		if sample.ErrorCode == "unsupported_frozen_scale_cell" || sample.ErrorCode == "unsupported_frozen_scale_workload" {
			t.Fatalf("registered scale cell %s/%s/%s is refused by dispatch",
				cell.WorkloadID, cell.Scale, cell.Mode)
		}
	}
	refused := adapter.Execute(t.Context(), experiment.AdapterOperation{
		ExperimentID: "scale", WorkloadID: "outcome-merkle", Scale: "not-frozen", Mode: "merkle_control",
	})
	if refused.ErrorCode != "unsupported_frozen_scale_cell" {
		t.Fatalf("an unfrozen scale was accepted by dispatch: %q", refused.ErrorCode)
	}
}

// The complement of the guard above: Artifact may advertise true only while its
// implemented set covers every preregistered cell exactly. A cell added to the
// requirements without an implementation must pull the capability back down.
func TestArtifactCapabilityRequiresItsCompleteProfile(t *testing.T) {
	coverage := publicationCoverageGates["artifact"]
	if !coverage.complete() {
		t.Fatalf("artifact profile is incomplete: %d implemented of %d required",
			len(coverage.implemented), len(coverage.required))
	}
	if len(coverage.implemented) != len(artifactPublicationRequirements) {
		t.Fatalf("artifact implemented %d cells, required %d",
			len(coverage.implemented), len(artifactPublicationRequirements))
	}
	if !implementedCapabilities()["artifact"] {
		t.Fatal("artifact capability is false despite a complete profile")
	}
}

// This test parses the real Catalog and exhausts every product subset that can
// legally resolve to a task policy. It prevents an adapter constructor or a
// syntactically valid private binding from hiding the absence of an executable
// formal task route.
func TestFrozenCatalogBacksTheReviewedScaleRouteAndCapability(t *testing.T) {
	frozen, err := catalog.Load("../../../config/catalog.yaml")
	if err != nil {
		t.Fatal(err)
	}
	policies := legalCatalogTaskPolicies(t, frozen)

	largeInfluenceRoutes := 0
	reviewedScaleRoutes := 0
	for _, policy := range policies {
		if policy.Budget.MaxInfluenceFacts < 1_035_000 {
			continue
		}
		largeInfluenceRoutes++
		// Even the zero-overlap formal cell performs novel followed by a
		// semantic replay on the same approved task. A one-query grant cannot
		// execute that pair; nonzero-overlap cells additionally need history.
		// The reviewed result-heavy route also grants many queries; the Scale
		// route must instead bind the exposure-scale Product alone to its narrow
		// aggregate-result budget.
		// The reviewed shape puts every benchmark low Product without a scoped
		// route of its own on one default ceiling, so result-heavy is no longer
		// the only wide route. Scale still binds its own Product to its own
		// route, which is what this block checks; the default-low closures are
		// checked by the wide-grant block further down.
		if policy.Budget.MaxQueries < 2 || onlyResultHeavy(policy) || onlyDefaultLowBenchmarkProducts(policy) {
			continue
		}
		reviewedScaleRoutes++
		if policy.BudgetProfile != "final-v5-exposure-scale-v1" {
			t.Fatalf("Scale route uses budget profile %q, want final-v5-exposure-scale-v1", policy.BudgetProfile)
		}
		if len(policy.Products) != 1 || policy.Products[0].Name != "final_v5_exposure_scale" {
			t.Fatalf("reviewed Scale route resolved for %d products; it must bind final_v5_exposure_scale alone", len(policy.Products))
		}
		budget := policy.Budget
		// max_rows moved from 16 to 200,000 when Baseline S3 joined this Product:
		// S3/45k-225k returns 45,000 rows on each of four governed queries of one
		// Task, and a Product may hold at most one scoped route. Every other
		// member of this budget is still asserted byte for byte.
		if budget.MaxQueries != 8 || budget.MaxRows != 200_000 || budget.MaxDBTime.String() != "30m0s" ||
			budget.PerQueryTimeout.String() != "30m0s" || budget.TaskTTL.String() != "2h0m0s" ||
			budget.MaxReleaseFacts != 1_250_000 || budget.MaxInfluenceFacts != 2_500_000 ||
			budget.MaxOutcomeFacts != 128 || budget.ExposureProfileVersion != "taskgate-exposure-v5" {
			t.Fatalf("Scale route resolved the wrong narrow budget: %+v", budget)
		}
		footprint := budget.PredicateFootprint
		if footprint == nil || footprint.Version != "taskgate-predicate-footprint-v1" ||
			footprint.MaxRawLiteralsPerQuery != 64 || footprint.MaxUniqueAtomsPerQuery != 16 ||
			footprint.MaxAtomPayloadBytes != 4096 || footprint.MaxTotalAtomPayloadBytes != 65536 {
			t.Fatalf("Scale route resolved the wrong predicate footprint: %+v", footprint)
		}
	}
	if largeInfluenceRoutes == 0 {
		t.Fatal("Catalog evidence premise changed: no route reaches the frozen 1,035,000-Fact scale")
	}
	if reviewedScaleRoutes != 1 {
		t.Fatalf("Catalog resolves %d reviewed large-influence two-query Scale routes, want 1", reviewedScaleRoutes)
	}
	scaleProductPublished := false
	for _, product := range frozen.Products {
		if product.Name == "final_v5_exposure_scale" {
			scaleProductPublished = true
			break
		}
	}
	if !scaleProductPublished {
		t.Fatal("Catalog does not publish the reviewed final_v5_exposure_scale Product")
	}
	if !scaleRealSystemValidated {
		t.Fatal("Scale retained-run evidence gate is false after author approval")
	}
	if !implementedCapabilities()["scale"] {
		t.Fatal("Scale capability stayed false despite its reviewed route and retained-run evidence")
	}
	// The route is necessary but not sufficient: the 24 governed cells enter
	// the registry only because the independent retained-run gate is also true.
	dependencyCells := 0
	for _, cell := range publicationCoverageGates["scale"].implemented {
		if cell.WorkloadID == "dependency-e2e" {
			dependencyCells++
		}
	}
	if dependencyCells != 24 {
		t.Fatalf("Scale advertises %d dependency-e2e cells, want the complete retained 24-cell set", dependencyCells)
	}

	// The reviewed benchmark route deliberately reaches the frozen 100,000-row,
	// 16-column result-heavy shape. Nothing else may: a wide grant reachable
	// from any other product set would let an unreviewed task execute a
	// result-heavy sized query.
	for _, policy := range policies {
		columns := 0
		for _, product := range policy.Products {
			columns += len(product.Fields)
		}
		// Width is judged from the budget itself. Summing columns across an
		// arbitrary Product union stopped being a usable proxy once mixed sets
		// became resolvable: a hundred-row detail grant is narrow no matter how
		// many columns the union exposes.
		wide := policy.Budget.MaxRows >= 10_000 || policy.Budget.MaxReleaseFacts >= 100_000
		if !wide {
			continue
		}
		switch {
		case onlyResultHeavy(policy):
			if policy.BudgetProfile != "final-v5-benchmark-low-v1" {
				t.Fatalf("result-heavy resolved budget %q, want the reviewed benchmark ceiling", policy.BudgetProfile)
			}
			if policy.Budget.MaxRows != 100_000 || columns != 16 {
				t.Fatalf("reviewed result-heavy route grants %d rows and %d columns; the frozen cells need exactly 100000 and 16",
					policy.Budget.MaxRows, columns)
			}
			if policy.Budget.MaxReleaseFacts < 100_000*16 {
				t.Fatalf("reviewed result-heavy route grants %d release Facts; the 100k x16 cell needs %d",
					policy.Budget.MaxReleaseFacts, 100_000*16)
			}
		case len(policy.Products) == 1 && policy.Products[0].Name == "final_v5_exposure_scale":
			// Scale shares its Product with Baseline S3, which returns 45,000
			// rows on each of four governed queries of one Task.
			if policy.BudgetProfile != "final-v5-exposure-scale-v1" || policy.Budget.MaxRows < 4*45_000 {
				t.Fatalf("Scale route = %q at %d rows; S3 settles %d on one Task",
					policy.BudgetProfile, policy.Budget.MaxRows, 4*45_000)
			}
		case onlyDefaultLowBenchmarkProducts(policy):
			// Baseline S1/S2/S5 and the ProvSQL nonce-join share the default low
			// ceiling. S1/SF10 settles four 50,000-row results on one Task.
			if policy.BudgetProfile != "final-v5-baseline-low-v1" || policy.Budget.MaxRows < 4*50_000 {
				t.Fatalf("default low route = %q at %d rows; S1/SF10 settles %d on one Task",
					policy.BudgetProfile, policy.Budget.MaxRows, 4*50_000)
			}
		default:
			t.Fatalf("a wide ceiling resolved for %v; only result-heavy, exposure-scale or the frozen default-low benchmark Products may reach one",
				productNames(policy))
		}
	}
}

// onlyDefaultLowBenchmarkProducts reports whether every Product in the policy is
// one the reviewed shape leaves on the default low route. It is a closed
// membership test on purpose: a new Product added there would not satisfy it.
// expense_summary is a member because this Catalog deliberately does not scope
// it -- scoping would make it unusable alongside expense_detail.
func onlyDefaultLowBenchmarkProducts(policy catalog.TaskPolicy) bool {
	frozen := map[string]bool{
		"provsql_orders": true, "provsql_lineitem": true, "provsql_nonce": true,
		"final_v5_analytics_depth4": true, "expense_summary": true,
	}
	if len(policy.Products) == 0 {
		return false
	}
	for _, product := range policy.Products {
		if !frozen[product.Name] {
			return false
		}
	}
	return true
}

func productNames(policy catalog.TaskPolicy) []string {
	names := make([]string, 0, len(policy.Products))
	for _, product := range policy.Products {
		names = append(names, product.Name)
	}
	return names
}

func onlyResultHeavy(policy catalog.TaskPolicy) bool {
	return len(policy.Products) == 1 && policy.Products[0].Name == "final_v5_result_heavy"
}

func legalCatalogTaskPolicies(t *testing.T, frozen *catalog.Catalog) []catalog.TaskPolicy {
	t.Helper()
	if len(frozen.Products) == 0 || len(frozen.Products) > 20 {
		t.Fatalf("unexpected Catalog product count %d", len(frozen.Products))
	}
	policies := make([]catalog.TaskPolicy, 0)
	for selected := 1; selected < 1<<len(frozen.Products); selected++ {
		names := make([]string, 0, len(frozen.Products))
		for index, product := range frozen.Products {
			if selected&(1<<index) != 0 {
				names = append(names, product.Name)
			}
		}
		policy, err := frozen.ResolveTaskPolicy(names)
		if err == nil {
			policies = append(policies, policy)
		}
	}
	if len(policies) == 0 {
		t.Fatal("frozen Catalog resolves no legal task policy")
	}
	return policies
}

func TestPublicationCoverageRejectsDuplicatesAndOutOfProfileCells(t *testing.T) {
	one := publicationCell{WorkloadID: "S1", Scale: "SF1", Mode: "direct"}
	two := publicationCell{WorkloadID: "S1", Scale: "SF1", Mode: "novel"}
	tests := []publicationProfileCoverage{
		{required: []publicationCell{one, one}, implemented: []publicationCell{one, one}},
		{required: []publicationCell{one}, implemented: []publicationCell{two}},
		{required: []publicationCell{one, two}, implemented: []publicationCell{one}},
	}
	for index, coverage := range tests {
		if coverage.complete() {
			t.Fatalf("invalid coverage case %d was accepted", index)
		}
	}
	if !(publicationProfileCoverage{required: []publicationCell{one, two}, implemented: []publicationCell{two, one}}).complete() {
		t.Fatal("complete exact coverage was rejected")
	}
}

func assertSamePublicationCells(t *testing.T, got, want []publicationCell) {
	t.Helper()
	toSet := func(cells []publicationCell) map[publicationCell]int {
		result := make(map[publicationCell]int, len(cells))
		for _, cell := range cells {
			result[cell]++
		}
		return result
	}
	gotSet, wantSet := toSet(got), toSet(want)
	if len(gotSet) != len(wantSet) {
		t.Fatalf("capability registry has %d unique cells, protocol has %d", len(gotSet), len(wantSet))
	}
	for cell, count := range wantSet {
		if gotSet[cell] != count {
			t.Fatalf("capability registry count for %+v = %d, want %d", cell, gotSet[cell], count)
		}
	}
}
