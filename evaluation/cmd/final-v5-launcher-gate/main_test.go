package main

import (
	"path/filepath"
	"testing"
)

func TestRQ5LauncherGateUsesExplicitCoordinateMap(t *testing.T) {
	mapPath := filepath.Join("..", "..", "..", "config", "profiles", "rq5-workload-cell-map-v1.json")
	campaign := []string{"rq5/online-transition-v1/single/build"}
	experiment := []string{"rq5/daily-publication-v5/345000/build_verify_activate"}
	if err := validateRQ5LauncherSelection(mapPath, campaign, experiment); err != nil {
		t.Fatal(err)
	}
	if err := validateRQ5LauncherSelection(mapPath, campaign,
		[]string{"rq5/daily-publication-v5/345000/retained_route"}); err == nil {
		t.Fatal("launcher gate accepted a mismatched experiment coordinate")
	}
	if err := validateRQ5LauncherSelection(mapPath,
		[]string{"rq5/online-transition-v1/single/unknown"}, experiment); err == nil {
		t.Fatal("launcher gate accepted an unknown campaign coordinate")
	}
}
