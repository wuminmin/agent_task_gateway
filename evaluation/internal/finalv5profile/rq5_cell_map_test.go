package finalv5profile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRQ5WorkloadCellMapExactlyCoversFrozenRegistryAndWorkloads(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", "..", ".."))
	mapping, _, err := LoadRQ5WorkloadCellMap(filepath.Join(root, RQ5WorkloadCellMapSourcePath))
	if err != nil {
		t.Fatal(err)
	}
	campaign := rq5RegistryCellsForTest(t, filepath.Join(root, "config/profiles/registry.json"))
	experiment := rq5WorkloadCellsForTest(t,
		filepath.Join(root, "evaluation/final-v5-wsl2/protocol/workloads-v1.yaml"))
	if err := mapping.RequireExactCoverage(campaign, experiment); err != nil {
		t.Fatal(err)
	}
	translated, err := mapping.TranslateCampaignCells(campaign)
	if err != nil {
		t.Fatal(err)
	}
	if !equalSortedStrings(translated, experiment) {
		t.Fatalf("translated cells = %v, want %v", translated, experiment)
	}
	reversed, err := mapping.TranslateExperimentCells(experiment)
	if err != nil {
		t.Fatal(err)
	}
	if !equalSortedStrings(reversed, campaign) {
		t.Fatalf("reverse-translated cells = %v, want %v", reversed, campaign)
	}
}

func TestRQ5WorkloadCellMapRejectsUnknownDuplicateAndPartialCoordinates(t *testing.T) {
	valid := RQ5WorkloadCellMap{SchemaVersion: 1, Record: RQ5WorkloadCellMapRecord, Mappings: []RQ5WorkloadCellMapping{
		{CampaignCell: "rq5/campaign/single/build", ExperimentCell: "rq5/experiment/345000/build"},
		{CampaignCell: "rq5/campaign/single/retained", ExperimentCell: "rq5/experiment/345000/retained"},
	}}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := valid.TranslateCampaignCells([]string{"rq5/campaign/single/unknown"}); err == nil {
		t.Fatal("unknown campaign coordinate was accepted")
	}
	if _, err := valid.TranslateExperimentCells([]string{"rq5/experiment/345000/unknown"}); err == nil {
		t.Fatal("unknown experiment coordinate was accepted")
	}
	if _, err := valid.TranslateCampaignCells([]string{
		"rq5/campaign/single/build", "rq5/campaign/single/build",
	}); err == nil {
		t.Fatal("duplicate selected coordinate was accepted")
	}
	if err := valid.RequireExactCoverage(
		[]string{"rq5/campaign/single/build"},
		[]string{"rq5/experiment/345000/build", "rq5/experiment/345000/retained"}); err == nil {
		t.Fatal("partial frozen coverage was accepted")
	}

	for name, mutate := range map[string]func(*RQ5WorkloadCellMap){
		"duplicate campaign": func(mapping *RQ5WorkloadCellMap) {
			mapping.Mappings[1].CampaignCell = mapping.Mappings[0].CampaignCell
		},
		"duplicate experiment": func(mapping *RQ5WorkloadCellMap) {
			mapping.Mappings[1].ExperimentCell = mapping.Mappings[0].ExperimentCell
		},
	} {
		t.Run(name, func(t *testing.T) {
			mapping := valid
			mapping.Mappings = append([]RQ5WorkloadCellMapping(nil), valid.Mappings...)
			mutate(&mapping)
			if err := mapping.Validate(); err == nil {
				t.Fatal("duplicate mapping was accepted")
			}
		})
	}
	unknown := `{"schema_version":1,"record":"taskgate-rq5-workload-cell-map-v1","mappings":[],"unknown":true}`
	if _, err := DecodeRQ5WorkloadCellMap([]byte(unknown)); err == nil {
		t.Fatal("unknown mapping member was accepted")
	}
}

func rq5RegistryCellsForTest(t *testing.T, path string) []string {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var registry Registry
	if err := json.Unmarshal(payload, &registry); err != nil {
		t.Fatal(err)
	}
	var cells []string
	for _, profile := range registry.Profiles {
		for _, cell := range profile.Cells {
			if strings.HasPrefix(cell, "rq5/") {
				cells = append(cells, cell)
			}
		}
	}
	return cells
}

func rq5WorkloadCellsForTest(t *testing.T, path string) []string {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Profiles map[string]struct {
			Workloads []struct {
				ID     string   `yaml:"id"`
				Scales []string `yaml:"scales"`
				Modes  []string `yaml:"modes"`
			} `yaml:"workloads"`
		} `yaml:"profiles"`
	}
	if err := yaml.Unmarshal(payload, &manifest); err != nil {
		t.Fatal(err)
	}
	var cells []string
	for _, workload := range manifest.Profiles["rq5"].Workloads {
		for _, scale := range workload.Scales {
			for _, mode := range workload.Modes {
				cells = append(cells, strings.Join([]string{"rq5", workload.ID, scale, mode}, "/"))
			}
		}
	}
	return cells
}
