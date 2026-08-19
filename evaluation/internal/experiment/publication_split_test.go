package experiment

import (
	"fmt"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
)

func TestSplitPublicationPlanRequires129Plus38Plus11WithoutProfileFiction(t *testing.T) {
	plan := splitPublicationPlanFixture()
	if err := ValidateSplitPublicationPlan(plan); err != nil {
		t.Fatalf("valid split publication plan: %v", err)
	}

	mutations := []struct {
		name   string
		mutate func(*finalv5profile.CampaignPlan)
	}{
		{"profile denominator", func(value *finalv5profile.CampaignPlan) { value.Deployments[0].Cells = value.Deployments[0].Cells[1:] }},
		{"scale denominator", func(value *finalv5profile.CampaignPlan) {
			value.NonProfileCampaigns[0].Cells = value.NonProfileCampaigns[0].Cells[1:]
		}},
		{"compiler processes", func(value *finalv5profile.CampaignPlan) { value.NonProfileCampaigns[2].ProcessReplicates = 1 }},
		{"state inheritance", func(value *finalv5profile.CampaignPlan) { value.NonProfileCampaigns[0].StateInheritance = true }},
		{"profile binding fiction", func(value *finalv5profile.CampaignPlan) { value.NonProfileCampaigns[0].ProfileBinding = "required" }},
		{"pilot aggregate", func(value *finalv5profile.CampaignPlan) {
			value.PreregisteredAggregates = []finalv5profile.CampaignPreregisteredAggregate{{Cell: "concurrency/shared-root/50/natural_contention"}}
		}},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			mutated := splitPublicationPlanFixture()
			test.mutate(&mutated)
			if err := ValidateSplitPublicationPlan(mutated); err == nil {
				t.Fatal("mutated publication plan was accepted")
			}
		})
	}
}

func splitPublicationPlanFixture() finalv5profile.CampaignPlan {
	plan := finalv5profile.CampaignPlan{ContractRelease: "final-v5-contracts-v1.10"}
	for deployment := 0; deployment < 11; deployment++ {
		cells := 11
		if deployment < 8 {
			cells = 12
		}
		entry := finalv5profile.PlannedDeploy{Alias: fmt.Sprintf("profile-%02d", deployment), Ready: true}
		for cell := 0; cell < cells; cell++ {
			entry.Cells = append(entry.Cells, fmt.Sprintf("profile/%02d/%03d/mode", deployment, cell))
		}
		plan.Deployments = append(plan.Deployments, entry)
	}
	add := func(id, experiment string, count, processes, warmups, samples int) {
		profile := experiment
		if id == "scale-kernel-storage" {
			profile = "scale-extreme"
		}
		entry := finalv5profile.PlannedNonProfileCampaign{ID: id, ExperimentID: experiment,
			ProtocolProfile: profile,
			ExecutionModel:  "deployment_free_process", FreshExecutions: 3, ProcessReplicates: processes,
			WarmupsPerCell: warmups, MeasuredSamplesPerCell: samples, ProfileBinding: "forbidden"}
		for index := 0; index < count; index++ {
			cell := fmt.Sprintf("%s/%s/%03d/mode", experiment, id, index)
			entry.Cells = append(entry.Cells, cell)
			plan.NonProfileCells = append(plan.NonProfileCells, cell)
		}
		plan.NonProfileCampaigns = append(plan.NonProfileCampaigns, entry)
	}
	add("scale-outcome-merkle", "scale", 36, 1, 5, 30)
	add("scale-kernel-storage", "scale", 2, 1, 5, 30)
	add("compiler", "compiler", 11, 5, 1, 100)
	return plan
}
