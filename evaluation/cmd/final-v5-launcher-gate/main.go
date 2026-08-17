package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

func main() {
	experimentID := flag.String("experiment", "", "profile campaign experiment identity")
	selectedPath := flag.String("selected-cells", "", "strict JSON array of assigned experiment/cell identities")
	inputPath := flag.String("input", "", "profile campaign JSONL output")
	campaignClass := flag.String("campaign-class", "", "expected runner-owned campaign class")
	samplesPerCell := flag.Int("samples-per-cell", 1, "exact retained sample count per selected cell")
	retainedInput := flag.Bool("retained-sample-input", false,
		"offline audit only: wrap immutable pre-fix flat Sample JSONL in memory")
	flag.Parse()

	if *experimentID == "" || *selectedPath == "" || *inputPath == "" || *campaignClass == "" {
		fmt.Fprintln(os.Stderr, "-experiment, -selected-cells, -input, and -campaign-class are required")
		os.Exit(2)
	}
	if *campaignClass != "pilot" {
		fail(fmt.Errorf("profile campaign launcher gate requires campaign_class pilot"))
	}
	selectedBytes, err := os.ReadFile(*selectedPath)
	if err != nil {
		fail(err)
	}
	var selected []string
	if err := experiment.StrictJSON(selectedBytes, &selected); err != nil {
		fail(fmt.Errorf("decode selected cells: %w", err))
	}

	var records []experiment.ProfileCampaignSampleV1
	if *retainedInput {
		samples, err := experiment.ReadSamples([]string{*inputPath})
		if err != nil {
			fail(err)
		}
		records = experiment.WrapRetainedSamplesForProfileCampaignAudit(samples, *campaignClass)
	} else {
		var err error
		records, err = experiment.ReadProfileCampaignSamples(*inputPath)
		if err != nil {
			fail(err)
		}
	}
	if err := experiment.ValidateProfileCampaignExperimentGate(*experimentID, selected, records, *samplesPerCell); err != nil {
		fail(err)
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"schema_version": 1,
		"status":         "pass",
		"experiment_id":  *experimentID,
		"campaign_class": *campaignClass,
		"samples":        len(records),
		"selected_cells": len(selected),
	})
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
