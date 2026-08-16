package finalv5profile

import (
	"fmt"
	"sort"
	"strings"
)

// CampaignPlan is the deployment plan for one formal campaign replicate under
// the per-profile split: one Catalog-bound deployment per profile that carries
// measured cells, plus the cells that need no deployment at all.
//
// It exists because a campaign that brings one deployment up on the master
// Catalog and runs nine experiments against it cannot serve the Artifact arm,
// which verifies that the Gateway signing its Receipts was serving the
// result-heavy profile Catalog. One deployment cannot be two Catalogs, so the
// campaign becomes several deployments and the plan is what says which.
type CampaignPlan struct {
	ContractRelease string              `json:"contract_release"`
	Deployments     []PlannedDeploy     `json:"deployments"`
	KernelOnlyCells []string            `json:"kernel_only_cells"`
	Coverage        map[string][]string `json:"coverage"`
}

// PlannedDeploy is one profile-bound deployment and the cells it runs.
type PlannedDeploy struct {
	ProfileID     string   `json:"profile_id"`
	Alias         string   `json:"alias"`
	CatalogPath   string   `json:"catalog_path"`
	CatalogSHA256 string   `json:"catalog_sha256"`
	Experiments   []string `json:"experiments"`
	Cells         []string `json:"cells"`
	// Ready reports whether this profile may be deployed today. A profile whose
	// activation smoke has not passed is planned and reported, never silently
	// dropped: a campaign that quietly skipped it would produce a partial
	// matrix that still looked complete.
	Ready       bool     `json:"ready"`
	NotReadyFor []string `json:"not_ready_for,omitempty"`
}

// BuildCampaignPlan derives the plan from the profile registry and the complete
// set of cells the campaign must measure. Every required cell must be carried
// by exactly one profile or declared kernel-only; anything else is an error
// rather than a warning, because a cell assigned twice would be measured twice
// under different Catalogs and a cell assigned to none would vanish.
func BuildCampaignPlan(registry Registry, required []string, kernelOnly map[string]bool) (CampaignPlan, error) {
	plan := CampaignPlan{ContractRelease: registry.ContractRelease, Coverage: map[string][]string{}}
	assigned := map[string]string{}
	for _, profile := range registry.Profiles {
		measured := make([]string, 0, len(profile.Cells))
		for _, cell := range profile.Cells {
			if kernelOnly[cell] {
				continue
			}
			measured = append(measured, cell)
		}
		if len(measured) == 0 {
			continue
		}
		sort.Strings(measured)
		for _, cell := range measured {
			if owner, taken := assigned[cell]; taken {
				return CampaignPlan{}, fmt.Errorf("cell %s is carried by both %s and %s", cell, owner, profile.Alias)
			}
			assigned[cell] = profile.Alias
		}
		deploy := PlannedDeploy{
			ProfileID: profile.ID, Alias: profile.Alias,
			CatalogPath: profile.CatalogPath, CatalogSHA256: profile.CatalogSHA256,
			Experiments: append([]string(nil), profile.Experiments...),
			Cells:       measured,
			Ready:       profile.Status.ActivationSmokePassed && profile.CatalogPath != "" && profile.CatalogSHA256 != "",
		}
		if !deploy.Ready {
			deploy.NotReadyFor = notReadyReasons(profile)
		}
		plan.Deployments = append(plan.Deployments, deploy)
	}
	sort.Slice(plan.Deployments, func(left, right int) bool {
		return plan.Deployments[left].Alias < plan.Deployments[right].Alias
	})

	var missing []string
	for _, cell := range required {
		switch {
		case kernelOnly[cell]:
			plan.KernelOnlyCells = append(plan.KernelOnlyCells, cell)
		case assigned[cell] == "":
			missing = append(missing, cell)
		default:
			experiment := strings.SplitN(cell, "/", 2)[0]
			plan.Coverage[experiment] = append(plan.Coverage[experiment], cell)
		}
	}
	sort.Strings(plan.KernelOnlyCells)
	for experiment := range plan.Coverage {
		sort.Strings(plan.Coverage[experiment])
	}
	if len(missing) != 0 {
		sort.Strings(missing)
		shown := missing
		if len(shown) > 5 {
			shown = shown[:5]
		}
		return CampaignPlan{}, fmt.Errorf("%d required cells are carried by no profile, for example %v",
			len(missing), shown)
	}
	return plan, nil
}

func notReadyReasons(profile Profile) []string {
	reasons := make([]string, 0, len(profile.Status.UnresolvedReasons))
	for _, reason := range profile.Status.UnresolvedReasons {
		reasons = append(reasons, reason.Code)
	}
	if profile.CatalogPath == "" {
		reasons = append(reasons, "no_generated_profile_catalog")
	}
	sort.Strings(reasons)
	return reasons
}

// ReadyDeployments reports how many planned deployments may run today, which is
// the number a campaign launcher must compare against the plan's length before
// claiming it executed the whole matrix.
func (plan CampaignPlan) ReadyDeployments() int {
	ready := 0
	for _, deploy := range plan.Deployments {
		if deploy.Ready {
			ready++
		}
	}
	return ready
}
