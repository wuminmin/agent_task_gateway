package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
)

func TestPublicationPlanClosesProfileAndNonProfileDenominators(t *testing.T) {
	root := filepath.Clean("../../..")
	required, nonProfile, groups, err := publicationCells(root)
	if err != nil {
		t.Fatal(err)
	}
	registryBytes, err := os.ReadFile(filepath.Join(root, "config/profiles/registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	var registry finalv5profile.Registry
	if err := json.Unmarshal(registryBytes, &registry); err != nil {
		t.Fatal(err)
	}
	for _, profile := range registry.Profiles {
		required = append(required, profile.Cells...)
	}
	plan, err := finalv5profile.BuildCampaignPlan(registry, required, nonProfile)
	if err != nil {
		t.Fatal(err)
	}
	plan.NonProfileCampaigns = groups
	profileCells := 0
	for _, deployment := range plan.Deployments {
		profileCells += len(deployment.Cells)
	}
	if len(plan.Deployments) != 11 || profileCells != 125 || len(plan.NonProfileCells) != 47 {
		t.Fatalf("publication plan deployments/profile/non-profile = %d/%d/%d, want 11/125/47",
			len(plan.Deployments), profileCells, len(plan.NonProfileCells))
	}
	want := map[string]struct {
		cells, processes, warmups, samples int
	}{
		"scale-outcome-merkle": {36, 1, 5, 30},
		"scale-kernel-storage": {2, 1, 5, 30},
		"compiler":             {11, 5, 1, 100},
	}
	for _, group := range groups {
		expected, ok := want[group.ID]
		if !ok || len(group.Cells) != expected.cells || group.FreshExecutions != 3 ||
			group.ProcessReplicates != expected.processes || group.WarmupsPerCell != expected.warmups ||
			group.MeasuredSamplesPerCell != expected.samples || group.ExecutionModel != "deployment_free_process" ||
			group.ProfileBinding != "forbidden" || group.StateInheritance {
			t.Fatalf("non-profile group differs from frozen execution model: %+v", group)
		}
	}
}
