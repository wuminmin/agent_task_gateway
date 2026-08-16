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
		{experimentID: "scale", profiles: []string{"scale", "scale-extreme"}, required: scalePublicationRequirements, cellCount: 62},
		{experimentID: "artifact", profiles: []string{"artifact"}, required: artifactPublicationRequirements, cellCount: 6},
		{experimentID: "rls", profiles: []string{"rls"}, required: rlsPublicationRequirements, cellCount: 6},
		{experimentID: "attack", profiles: []string{"attack"}, required: attackPublicationRequirements, cellCount: 15},
		{experimentID: "provsql", profiles: []string{"provsql"}, required: provSQLPublicationRequirements, cellCount: 9},
		{experimentID: "compiler", profiles: []string{"compiler"}, required: compilerPublicationRequirements, cellCount: 11},
		{experimentID: "concurrency", profiles: []string{"concurrency"}, required: concurrencyPublicationRequirements, cellCount: 9},
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
	for _, experimentID := range []string{"rls", "attack", "provsql", "compiler", "concurrency", "rq5"} {
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

func TestBaselinePilotCannotEnableFormalCapability(t *testing.T) {
	if publicationCoverageGates["baseline"].complete() {
		t.Fatal("S1/tiny Pilot was treated as complete S1--S6 publication coverage")
	}
	if implementedCapabilities()["baseline"] {
		t.Fatal("baseline formal capability was advertised before all publication cells were implemented")
	}
	// S1 and S2 gained a real execution path on 2026-08-16. Every implemented
	// cell must be one of theirs, and 20 of 58 is still incomplete, so the two
	// assertions above continue to hold. A cell registered from any other
	// workload would mean the resolver accepted something Execute cannot run.
	if len(baselineImplementedPublicationCells) != 20 {
		t.Fatalf("formal baseline registry contains %d cells, want S1 and S2's 20",
			len(baselineImplementedPublicationCells))
	}
	for _, cell := range baselineImplementedPublicationCells {
		if cell.WorkloadID != "S1" && cell.WorkloadID != "S2" {
			t.Fatalf("baseline registered %s/%s/%s, which has no execution path",
				cell.WorkloadID, cell.Scale, cell.Mode)
		}
	}
}

// TestEveryUnimplementedBaselineCellFailsClosed holds the resolver to the
// workloads that really have an execution path. S1 and S2 must resolve, and
// every other frozen cell must still be refused by name rather than attempted
// with a substituted query.
func TestEveryUnimplementedBaselineCellFailsClosed(t *testing.T) {
	adapter := &realAdapter{}
	implemented := map[publicationCell]bool{}
	for _, cell := range baselineImplementedPublicationCells {
		implemented[cell] = true
	}
	refused := 0
	for _, cell := range baselinePublicationRequirements {
		if implemented[cell] {
			if _, err := resolveBaselineExecutionCell(experiment.AdapterOperation{
				ExperimentID: "baseline", WorkloadID: cell.WorkloadID,
				Scale: cell.Scale, Mode: cell.Mode,
			}); err != nil {
				t.Fatalf("registered cell %+v does not resolve: %v", cell, err)
			}
			continue
		}
		operation := experiment.AdapterOperation{
			ExperimentID: "baseline",
			WorkloadID:   cell.WorkloadID,
			Scale:        cell.Scale,
			Mode:         cell.Mode,
		}
		sample := adapter.Execute(t.Context(), operation)
		if sample.Status != "invalid" || sample.ErrorCode != "unsupported_source_controlled_baseline_cell" {
			t.Fatalf("formal cell %+v returned status=%q code=%q", cell, sample.Status, sample.ErrorCode)
		}
		refused++
	}
	if refused != len(baselinePublicationRequirements)-20 {
		t.Fatalf("%d unimplemented cells failed closed, want %d",
			refused, len(baselinePublicationRequirements)-20)
	}
}

// Artifact left this guard on 2026-08-16 by completing its profile, so the
// guard now covers Scale alone. Baseline keeps its own fail-closed test above.
func TestIncompleteScaleProfileCannotEnableCapabilities(t *testing.T) {
	for _, experimentID := range []string{"scale"} {
		coverage := publicationCoverageGates[experimentID]
		if coverage.complete() {
			t.Fatalf("%s partial formal profile was reported complete", experimentID)
		}
		if implementedCapabilities()[experimentID] {
			t.Fatalf("%s capability was advertised without a routable full matrix", experimentID)
		}
		if len(coverage.implemented) != 0 {
			t.Fatalf("%s registry contains %d cells before real full-profile support", experimentID, len(coverage.implemented))
		}
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
func TestFrozenCatalogBacksTheReviewedScaleRouteWithoutAdvertisingIt(t *testing.T) {
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
		// The reviewed benchmark ceiling also grants many queries, and it is now
		// reachable both from result-heavy alone and from the frozen ProvSQL
		// relations Baseline S1/S2 share with the nonce-join workload. The Scale
		// route must instead bind the exposure-scale Product alone to its narrow
		// aggregate-result budget. The wide-grant check further down is what
		// holds that shared ceiling to exactly those reviewed Products.
		if policy.Budget.MaxQueries < 2 || onlyResultHeavy(policy) || onlyFrozenProvSQLRelations(policy) {
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
		if budget.MaxQueries != 8 || budget.MaxRows != 16 || budget.MaxDBTime.String() != "30m0s" ||
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
	if implementedCapabilities()["scale"] {
		t.Fatal("Scale capability was advertised when only its reviewed Catalog route is available")
	}
	if implemented := len(publicationCoverageGates["scale"].implemented); implemented != 0 {
		t.Fatalf("Scale coverage advertises %d implemented cells before real full-profile support", implemented)
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
		// A grant is result-heavy shaped when its own ceilings can release a
		// result-heavy sized answer. Summing columns across an arbitrary Product
		// union is not that property and stopped being a usable proxy once the
		// frozen ProvSQL relations left their scoped route: mixed sets such as
		// expense_detail plus provsql_orders became resolvable, and their union
		// exposes seventeen columns while the high-sensitivity route still grants
		// a hundred rows and five hundred release Facts. The reviewed Catalog
		// candidate resolves those same mixed sets, so they are legal and narrow,
		// not a widening.
		wide := policy.Budget.MaxRows >= 10_000 || policy.Budget.MaxReleaseFacts >= 100_000
		// The Scale route is reviewed and asserted exactly above: it releases at
		// most sixteen rows and is checked there against its whole narrow budget.
		if !wide || (len(policy.Products) == 1 && policy.Products[0].Name == "final_v5_exposure_scale") {
			continue
		}
		if onlyResultHeavy(policy) {
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
			continue
		}
		// Baseline S1/SF10 releases the whole 50,000-row frozen orders relation
		// and S2 joins it to lineitem, so a second wide ceiling is now reachable
		// from the three frozen ProvSQL relations. A Product may belong to at
		// most one scoped route and routes match an exact set, so those closures
		// cannot be given a route of their own while ProvSQL's nonce-join keeps
		// one; they share the default low route instead. The widening is bounded
		// to exactly those three reviewed relations, and every other product set
		// must still fail this check.
		if !onlyFrozenProvSQLRelations(policy) {
			t.Fatalf("a wide ceiling resolved for %v; only final_v5_result_heavy alone or the frozen ProvSQL relations may reach one",
				productNames(policy))
		}
		if policy.BudgetProfile != "final-v5-baseline-low-v1" {
			t.Fatalf("the Baseline closure resolved budget %q, want the Baseline ceiling", policy.BudgetProfile)
		}
		if policy.Budget.MaxReleaseFacts < 50_000*2 {
			t.Fatalf("the Baseline closure grants %d release Facts; S1/SF10 releases %d",
				policy.Budget.MaxReleaseFacts, 50_000*2)
		}
		// One Baseline Task settles a cell's novel query and its three replays,
		// and max_rows is a cumulative per-Task ledger, so the ceiling has to
		// cover four S1/SF10 results rather than one. The first targeted run
		// failed exactly here: under the 100,000-row result-heavy ceiling,
		// S1/SF10's third governed query was refused and no other cell was.
		if policy.Budget.MaxRows < 4*50_000 {
			t.Fatalf("the Baseline closure grants %d rows; one S1/SF10 Task settles %d",
				policy.Budget.MaxRows, 4*50_000)
		}
	}
}

// onlyFrozenProvSQLRelations reports whether every Product in the policy is one
// of the three frozen benchmark relations Baseline S1/S2 and the ProvSQL
// nonce-join draw from. It is deliberately a closed membership test: a new
// Product added to the default low route would not satisfy it.
func onlyFrozenProvSQLRelations(policy catalog.TaskPolicy) bool {
	frozen := map[string]bool{"provsql_orders": true, "provsql_lineitem": true, "provsql_nonce": true}
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
