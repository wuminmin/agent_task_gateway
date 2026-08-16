package experiment

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func selectedRLSConfig() Config {
	return Config{SchemaVersion: 1, CampaignClass: "pilot", PilotKind: "real_system",
		CampaignID: "p30-selection", ExperimentID: "rls", SubmissionCommit: "0123456789012345678901234567890123456789",
		Deployments: 1, Samples: 1, RandomSeed: 20260817, FreshRootPerSample: true,
		Workloads: []Workload{
			{ID: "adaptive-100-v1", Scales: []string{"100-queries"}, Modes: []string{"rls", "unlimited", "bounded"}},
			{ID: "policy-denied-control", Scales: []string{"single"}, Modes: []string{"rls", "unlimited", "bounded"}},
		}}
}

func TestSelectedCellsRunOnlyTheProfilePartition(t *testing.T) {
	config := selectedRLSConfig()
	selected := map[string]bool{
		"rls/adaptive-100-v1/100-queries/bounded":  true,
		"rls/policy-denied-control/single/bounded": true,
	}
	position := 0
	operations := buildOperationsSelected(config, "deployment-01", 1, 1, &position, nil, selected)
	if len(operations) != 2 {
		t.Fatalf("selected operations = %d, want 2", len(operations))
	}
	got := make([]string, 0, len(operations))
	for index, operation := range operations {
		got = append(got, operation.ExperimentID+"/"+operation.CellID)
		if operation.OrderPosition != index+1 || operation.PairedSystemOrder != "bounded" || operation.Mode != "bounded" {
			t.Fatalf("selected operation retained an undispatched peer: %+v", operation)
		}
	}
	want := []string{"rls/adaptive-100-v1/100-queries/bounded", "rls/policy-denied-control/single/bounded"}
	if !reflect.DeepEqual(got, want) && !reflect.DeepEqual(got, []string{want[1], want[0]}) {
		t.Fatalf("selected identities = %v, want exactly %v", got, want)
	}
}

func TestSelectedCellsFileFailsClosed(t *testing.T) {
	config := selectedRLSConfig()
	for name, payload := range map[string]string{
		"empty":            `[]`,
		"duplicate":        `["rls/adaptive-100-v1/100-queries/bounded","rls/adaptive-100-v1/100-queries/bounded"]`,
		"other experiment": `["baseline/S1/SF1/direct"]`,
		"unknown":          `["rls/adaptive-100-v1/100-queries/not-a-mode"]`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cells.json")
			if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadSelectedCells(path, "rls", config); err == nil {
				t.Fatal("invalid selected-cells file was accepted")
			}
		})
	}
}
