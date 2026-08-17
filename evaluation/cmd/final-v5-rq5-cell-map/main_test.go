package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
)

func TestRunTranslatesOnlyAfterExactFrozenCoverageValidation(t *testing.T) {
	repo := filepath.Clean(filepath.Join("..", "..", ".."))
	directory := t.TempDir()
	campaignPath := filepath.Join(directory, "campaign.json")
	translatedPath := filepath.Join(directory, "experiment.json")
	evidencePath := filepath.Join(directory, "evidence.json")
	campaign := []string{
		"rq5/online-transition-v1/single/build",
		"rq5/online-transition-v1/single/retained",
	}
	payload, err := json.Marshal(campaign)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(campaignPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(repo, finalv5profile.RQ5WorkloadCellMapSourcePath, "config/profiles/registry.json",
		"evaluation/final-v5-wsl2/protocol/workloads-v1.yaml", campaignPath, translatedPath,
		evidencePath, "source/rq5-workload-cell-map-v1.json"); err != nil {
		t.Fatal(err)
	}
	translated, err := readStrictStringArray(translatedPath)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"rq5/daily-publication-v5/345000/build_verify_activate",
		"rq5/daily-publication-v5/345000/retained_route",
	}
	if len(translated) != len(want) || translated[0] != want[0] || translated[1] != want[1] {
		t.Fatalf("translated cells = %v, want %v", translated, want)
	}
	var evidence finalv5profile.RQ5CellTranslationEvidence
	evidenceBytes, err := os.ReadFile(evidencePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(evidenceBytes, &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.MappingSourcePath != finalv5profile.RQ5WorkloadCellMapSourcePath ||
		evidence.MappingSHA256 == "" || len(evidence.CampaignCells) != 2 || len(evidence.ExperimentCells) != 2 {
		t.Fatalf("translation evidence = %+v", evidence)
	}
}
