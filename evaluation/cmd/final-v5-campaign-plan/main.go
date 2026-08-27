// Command final-v5-campaign-plan derives the per-profile deployment plan for a
// formal campaign replicate.
//
// The formal runner brings one deployment up and runs nine experiments against
// it. That cannot serve the Artifact arm, which verifies that the Gateway
// signing its Receipts was serving the result-heavy profile Catalog, so the
// campaign becomes one deployment per profile. This command says which
// deployments those are, which cells each carries, and which profiles are not
// yet activatable -- it never activates or measures anything.
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

	"taskbound.local/agent-data-gateway/evaluation/internal/concurrencyfixture"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
)

func main() {
	root := flag.String("root", ".", "repository root")
	registryPath := flag.String("registry", "config/profiles/registry.json", "profile registry path")
	preregistrationPath := flag.String("preregistration", concurrencyfixture.PreregistrationSourcePath,
		"source-controlled fixed-N concurrency preregistration")
	requireReady := flag.Bool("require-ready", false,
		"exit non-zero unless every planned deployment is activatable today")
	campaignClass := flag.String("campaign-class", "pilot", "pilot or publication planning semantics")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "positional arguments are not accepted")
		os.Exit(2)
	}
	if err := run(*root, *registryPath, *preregistrationPath, *campaignClass, *requireReady); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(root, registryPath, preregistrationPath, campaignClass string, requireReady bool) error {
	if campaignClass != "pilot" && campaignClass != "publication" {
		return errors.New("campaign class must be pilot or publication")
	}
	payload, err := os.ReadFile(filepath.Join(root, registryPath))
	if err != nil {
		return fmt.Errorf("read profile registry: %w", err)
	}
	var registry finalv5profile.Registry
	if err := json.Unmarshal(payload, &registry); err != nil {
		return fmt.Errorf("decode profile registry: %w", err)
	}
	required, nonProfile, nonProfileCampaigns, err := publicationCells(root)
	if err != nil {
		return err
	}
	// Pilot planning retains the 125-cell profile matrix. Formal
	// publication planning adds the 47 deployment-free cells explicitly.
	if campaignClass == "pilot" {
		required = required[:0]
		for _, profile := range registry.Profiles {
			required = append(required, profile.Cells...)
		}
		nonProfile = nil
		nonProfileCampaigns = nil
	} else {
		for _, profile := range registry.Profiles {
			required = append(required, profile.Cells...)
		}
		if len(required) != 172 {
			return fmt.Errorf("publication denominator is %d cells, want 172", len(required))
		}
	}
	plan, err := finalv5profile.BuildCampaignPlan(registry, required, nonProfile)
	if err != nil {
		return err
	}
	plan.NonProfileCampaigns = nonProfileCampaigns
	if campaignClass == "pilot" {
		preregistration, preregistrationSHA256, err := concurrencyfixture.LoadPreregistration(
			filepath.Join(root, preregistrationPath))
		if err != nil {
			return fmt.Errorf("load concurrency preregistration: %w", err)
		}
		if err := plan.AttachPreregistration(preregistration, preregistrationSHA256); err != nil {
			return err
		}
	}
	encoded, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	if requireReady && plan.ReadyDeployments() != len(plan.Deployments) {
		return errors.New("the campaign plan contains deployments that are not activatable yet")
	}
	return nil
}

func publicationCells(root string) ([]string, map[string]bool, []finalv5profile.PlannedNonProfileCampaign, error) {
	type profile struct {
		Workloads []struct {
			ID     string   `yaml:"id"`
			Scales []string `yaml:"scales"`
			Modes  []string `yaml:"modes"`
		} `yaml:"workloads"`
	}
	var workloads struct {
		SchemaVersion int                `yaml:"schema_version"`
		Profiles      map[string]profile `yaml:"profiles"`
	}
	payload, err := os.ReadFile(filepath.Join(root, "evaluation/final-v5-wsl2/protocol/workloads-v1.yaml"))
	if err != nil || yaml.Unmarshal(payload, &workloads) != nil || workloads.SchemaVersion != 2 {
		return nil, nil, nil, errors.New("decode frozen publication workload profiles")
	}
	type replicate struct {
		Profiles        []string `yaml:"profiles"`
		Processes       int      `yaml:"process_replicates"`
		Warmups         int      `yaml:"warmups_per_cell_per_process"`
		MeasuredSamples int      `yaml:"measured_samples_per_cell_per_process"`
	}
	var protocol struct {
		Campaign struct {
			PublicationDeployments int                  `yaml:"publication_deployments"`
			Contracts              map[string]replicate `yaml:"replicate_contracts"`
		} `yaml:"campaign"`
	}
	protocolPayload, err := os.ReadFile(filepath.Join(root, "evaluation/final-v5-wsl2/protocol/protocol-v1.yaml"))
	if err != nil || yaml.Unmarshal(protocolPayload, &protocol) != nil || protocol.Campaign.PublicationDeployments != 3 {
		return nil, nil, nil, errors.New("decode frozen publication replicate contracts")
	}
	contracts := map[string]replicate{}
	for _, contract := range protocol.Campaign.Contracts {
		for _, name := range contract.Profiles {
			contracts[name] = contract
		}
	}
	type groupKey struct{ id, experiment, profile string }
	groups := map[groupKey][]string{}
	var required []string
	nonProfile := map[string]bool{}
	profileNames := make([]string, 0, len(workloads.Profiles))
	for name := range workloads.Profiles {
		profileNames = append(profileNames, name)
	}
	sort.Strings(profileNames)
	for _, profileName := range profileNames {
		experimentID := profileName
		if profileName == "scale-extreme" {
			experimentID = "scale"
		}
		for _, workload := range workloads.Profiles[profileName].Workloads {
			for _, scale := range workload.Scales {
				for _, mode := range workload.Modes {
					cell := strings.Join([]string{experimentID, workload.ID, scale, mode}, "/")
					key := groupKey{}
					switch {
					case profileName == "scale" && workload.ID == "outcome-merkle":
						key = groupKey{"scale-outcome-merkle", "scale", profileName}
					case profileName == "scale-extreme":
						key = groupKey{"scale-kernel-storage", "scale", profileName}
					case profileName == "compiler":
						key = groupKey{"compiler", "compiler", profileName}
					}
					if key.id != "" {
						required = append(required, cell)
						nonProfile[cell] = true
						groups[key] = append(groups[key], cell)
					}
				}
			}
		}
	}
	var campaigns []finalv5profile.PlannedNonProfileCampaign
	for key, cells := range groups {
		contract, ok := contracts[key.profile]
		if !ok || contract.Processes < 1 || contract.MeasuredSamples < 1 {
			return nil, nil, nil, fmt.Errorf("frozen profile %s lacks a valid replicate contract", key.profile)
		}
		sort.Strings(cells)
		campaigns = append(campaigns, finalv5profile.PlannedNonProfileCampaign{
			ID: key.id, ExperimentID: key.experiment, ProtocolProfile: key.profile,
			ExecutionModel: "deployment_free_process", FreshExecutions: 3,
			ProcessReplicates: contract.Processes, WarmupsPerCell: contract.Warmups,
			MeasuredSamplesPerCell: contract.MeasuredSamples, StateInheritance: false,
			ProfileBinding: "forbidden", Cells: cells,
		})
	}
	sort.Strings(required)
	sort.Slice(campaigns, func(i, j int) bool { return campaigns[i].ID < campaigns[j].ID })
	if len(required) != 47 || len(nonProfile) != 47 || len(campaigns) != 2 {
		return nil, nil, nil, fmt.Errorf("publication non-profile denominator is %d cells with %d groups/%d identities, want 47 and 2/47", len(required), len(campaigns), len(nonProfile))
	}
	return required, nonProfile, campaigns, nil
}
