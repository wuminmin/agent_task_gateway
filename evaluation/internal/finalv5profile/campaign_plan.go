package finalv5profile

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/internal/concurrencyfixture"
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
	ContractRelease string          `json:"contract_release"`
	Deployments     []PlannedDeploy `json:"deployments"`
	// NonProfileCells are source-controlled workload cells whose implementation
	// does not acquire a Catalog-backed Gateway deployment.  They are planned,
	// executed, and finalized independently; assigning one to an arbitrary
	// profile would fabricate a deployment relationship.
	NonProfileCells         []string                         `json:"non_profile_cells"`
	NonProfileCampaigns     []PlannedNonProfileCampaign      `json:"non_profile_campaigns"`
	Coverage                map[string][]string              `json:"coverage"`
	PreregisteredAggregates []CampaignPreregisteredAggregate `json:"preregistered_aggregates"`
}

// PlannedNonProfileCampaign describes one homogeneous deployment-free runner
// invocation. FreshExecutions is the publication-level repetition count. The
// process/warmup/sample members are copied from the frozen replicate contract,
// so Compiler's five processes are not collapsed into one process merely
// because the top-level campaign still has three fresh executions.
type PlannedNonProfileCampaign struct {
	ID                     string   `json:"id"`
	ExperimentID           string   `json:"experiment_id"`
	ProtocolProfile        string   `json:"protocol_profile"`
	ExecutionModel         string   `json:"execution_model"`
	FreshExecutions        int      `json:"fresh_executions"`
	ProcessReplicates      int      `json:"process_replicates"`
	WarmupsPerCell         int      `json:"warmups_per_cell_per_process"`
	MeasuredSamplesPerCell int      `json:"measured_samples_per_cell_per_process"`
	StateInheritance       bool     `json:"state_inheritance"`
	ProfileBinding         string   `json:"profile_binding"`
	Cells                  []string `json:"cells"`
}

type CampaignPreregisteredAggregate struct {
	SourcePath         string `json:"source_path"`
	RetainedPath       string `json:"retained_path"`
	SourceSHA256       string `json:"source_sha256"`
	ProfileAlias       string `json:"profile_alias"`
	Cell               string `json:"cell"`
	Rounds             int    `json:"rounds"`
	SuccessesRequired  int    `json:"successes_required"`
	SuccessStatus      string `json:"success_status"`
	MissStatus         string `json:"miss_status"`
	MissErrorCode      string `json:"miss_error_code"`
	RoundIdentityScope string `json:"round_identity_scope"`
	RetainAllRounds    bool   `json:"retain_all_rounds"`
	EarlyStop          bool   `json:"early_stop"`
}

func (plan *CampaignPlan) AttachPreregistration(contract concurrencyfixture.Preregistration, digest string) error {
	if plan == nil || len(plan.PreregisteredAggregates) != 0 {
		return errors.New("campaign plan already carries a preregistration")
	}
	if err := contract.Validate(); err != nil || digest != concurrencyfixture.PreregistrationSHA256 {
		return errors.New("campaign preregistration is invalid or has an unapproved digest")
	}
	owners := 0
	for _, deployment := range plan.Deployments {
		if deployment.Alias != contract.ProfileAlias {
			continue
		}
		for _, cell := range deployment.Cells {
			if cell == contract.Cell {
				owners++
			}
		}
	}
	if owners != 1 {
		return fmt.Errorf("preregistered cell owners=%d, want exactly 1", owners)
	}
	plan.PreregisteredAggregates = []CampaignPreregisteredAggregate{{
		SourcePath:   concurrencyfixture.PreregistrationSourcePath,
		RetainedPath: "source/concurrency-preregistration-v1.json", SourceSHA256: digest,
		ProfileAlias: contract.ProfileAlias, Cell: contract.Cell, Rounds: contract.Rounds,
		SuccessesRequired: contract.SuccessesRequired, SuccessStatus: contract.SuccessStatus,
		MissStatus: contract.MissStatus, MissErrorCode: contract.MissErrorCode,
		RoundIdentityScope: contract.RoundIdentityScope, RetainAllRounds: contract.RetainAllRounds,
		EarlyStop: contract.EarlyStop,
	}}
	return nil
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
func BuildCampaignPlan(registry Registry, required []string, nonProfile map[string]bool) (CampaignPlan, error) {
	plan := CampaignPlan{ContractRelease: registry.ContractRelease, Coverage: map[string][]string{}}
	assigned := map[string]string{}
	for _, profile := range registry.Profiles {
		measured := make([]string, 0, len(profile.Cells))
		for _, cell := range profile.Cells {
			if nonProfile[cell] {
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
		case nonProfile[cell]:
			plan.NonProfileCells = append(plan.NonProfileCells, cell)
		case assigned[cell] == "":
			missing = append(missing, cell)
		default:
			experiment := strings.SplitN(cell, "/", 2)[0]
			plan.Coverage[experiment] = append(plan.Coverage[experiment], cell)
		}
	}
	sort.Strings(plan.NonProfileCells)
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
