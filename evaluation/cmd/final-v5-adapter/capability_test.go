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
	if len(baselineImplementedPublicationCells) != 0 {
		t.Fatalf("formal baseline registry contains %d cells without a complete implementation", len(baselineImplementedPublicationCells))
	}
}

func TestEveryFormalBaselineCellFailsClosedUntilImplemented(t *testing.T) {
	adapter := &realAdapter{}
	for _, cell := range baselinePublicationRequirements {
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
	}
}

func TestIncompleteScaleAndArtifactProfilesCannotEnableCapabilities(t *testing.T) {
	for _, experimentID := range []string{"scale", "artifact"} {
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

// This test parses the real Catalog and exhausts every product subset that can
// legally resolve to a task policy. It prevents an adapter constructor or a
// syntactically valid private binding from hiding the absence of an executable
// formal task route.
func TestFrozenCatalogCannotBackCompleteScaleOrArtifactProfiles(t *testing.T) {
	frozen, err := catalog.Load("../../../config/catalog.yaml")
	if err != nil {
		t.Fatal(err)
	}
	policies := legalCatalogTaskPolicies(t, frozen)

	largeInfluenceRoutes := 0
	for _, policy := range policies {
		if policy.Budget.MaxInfluenceFacts < 1_035_000 {
			continue
		}
		largeInfluenceRoutes++
		// Even the zero-overlap formal cell performs novel followed by a
		// semantic replay on the same approved task. A one-query grant cannot
		// execute that pair; nonzero-overlap cells additionally need history.
		if policy.Budget.MaxQueries >= 2 {
			t.Fatalf("Catalog now has a large-influence two-query route %q; implement and review the complete scale profile before changing its gate", policy.BudgetProfile)
		}
	}
	if largeInfluenceRoutes == 0 {
		t.Fatal("Catalog evidence premise changed: no route reaches the frozen 1,035,000-Fact scale")
	}

	maxRows, maxApprovedColumns := int64(0), 0
	for _, policy := range policies {
		if policy.Budget.MaxRows > maxRows {
			maxRows = policy.Budget.MaxRows
		}
		columns := 0
		for _, product := range policy.Products {
			columns += len(product.Fields)
		}
		if columns > maxApprovedColumns {
			maxApprovedColumns = columns
		}
	}
	if maxRows >= 10_000 {
		t.Fatalf("Catalog now grants %d rows; implement and review every result-heavy row scale before changing its gate", maxRows)
	}
	if maxApprovedColumns >= 16 {
		t.Fatalf("Catalog now permits a %d-column task; implement and review every x16 result-heavy cell before changing its gate", maxApprovedColumns)
	}
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
