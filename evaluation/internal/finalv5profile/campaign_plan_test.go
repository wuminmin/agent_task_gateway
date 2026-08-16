package finalv5profile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func repositoryRegistry(t *testing.T) Registry {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "profiles", "registry.json"))
	if err != nil {
		t.Fatalf("read profile registry: %v", err)
	}
	var registry Registry
	if err := json.Unmarshal(payload, &registry); err != nil {
		t.Fatalf("decode profile registry: %v", err)
	}
	return registry
}

// requiredCells collects every cell the registry assigns, which is what a
// campaign must measure. The plan is then checked against that same set, so
// this test proves the partition property rather than a hand-copied list.
func requiredCells(registry Registry) []string {
	var cells []string
	for _, profile := range registry.Profiles {
		cells = append(cells, profile.Cells...)
	}
	return cells
}

func TestCampaignPlanPartitionsEveryCellAcrossProfiles(t *testing.T) {
	registry := repositoryRegistry(t)
	required := requiredCells(registry)
	if len(required) == 0 {
		t.Fatal("the profile registry assigns no cells")
	}
	plan, err := BuildCampaignPlan(registry, required, nil)
	if err != nil {
		t.Fatalf("build campaign plan: %v", err)
	}
	// Every planned cell appears once and only once. A cell carried by two
	// profiles would be measured under two Catalogs; one carried by none would
	// disappear from a campaign that still reported success.
	seen := map[string]string{}
	for _, deploy := range plan.Deployments {
		if deploy.Alias == "" || deploy.ProfileID == "" {
			t.Fatalf("planned deployment has no identity: %+v", deploy)
		}
		for _, cell := range deploy.Cells {
			if previous, repeated := seen[cell]; repeated {
				t.Fatalf("cell %s is planned under both %s and %s", cell, previous, deploy.Alias)
			}
			seen[cell] = deploy.Alias
		}
	}
	if len(seen) != len(required) {
		t.Fatalf("plan covers %d cells, the registry assigns %d", len(seen), len(required))
	}
	// The Artifact conflict is the reason this plan exists: its cells must land
	// on the result-heavy profile and nowhere else.
	for cell, alias := range seen {
		if strings.HasPrefix(cell, "artifact/") && alias != "result-heavy" {
			t.Fatalf("artifact cell %s was planned under %s", cell, alias)
		}
	}
}

func TestCampaignPlanRefusesACellNoProfileCarries(t *testing.T) {
	registry := repositoryRegistry(t)
	required := append(requiredCells(registry), "baseline/S9/nowhere/novel")
	if _, err := BuildCampaignPlan(registry, required, nil); err == nil {
		t.Fatal("a cell carried by no profile was planned without error")
	}
}

func TestCampaignPlanKeepsUnreadyProfilesVisible(t *testing.T) {
	registry := repositoryRegistry(t)
	plan, err := BuildCampaignPlan(registry, requiredCells(registry), nil)
	if err != nil {
		t.Fatalf("build campaign plan: %v", err)
	}
	if len(plan.Deployments) == 0 {
		t.Fatal("plan contains no deployments")
	}
	// Nothing may be dropped for being unready. A launcher must be able to see
	// that the matrix is incomplete rather than infer it from a short run.
	for _, deploy := range plan.Deployments {
		if !deploy.Ready && len(deploy.NotReadyFor) == 0 {
			t.Fatalf("profile %s is unready without a stated reason", deploy.Alias)
		}
	}
	if plan.ReadyDeployments() > len(plan.Deployments) {
		t.Fatal("more deployments are ready than are planned")
	}
}

func TestCampaignPlanSeparatesKernelOnlyCells(t *testing.T) {
	registry := repositoryRegistry(t)
	required := append(requiredCells(registry), "scale/outcome-merkle/10k-x1-o0/merkle_control")
	kernel := map[string]bool{"scale/outcome-merkle/10k-x1-o0/merkle_control": true}
	plan, err := BuildCampaignPlan(registry, required, kernel)
	if err != nil {
		t.Fatalf("build campaign plan: %v", err)
	}
	if len(plan.KernelOnlyCells) != 1 {
		t.Fatalf("kernel-only cells = %v, want exactly the declared one", plan.KernelOnlyCells)
	}
	for _, deploy := range plan.Deployments {
		for _, cell := range deploy.Cells {
			if kernel[cell] {
				t.Fatalf("kernel-only cell %s was assigned a deployment", cell)
			}
		}
	}
}
