package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"taskbound.local/agent-data-gateway/evaluation/internal/concurrencyfixture"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
)

func main() {
	experimentID := flag.String("experiment", "", "profile campaign experiment identity")
	selectedPath := flag.String("selected-cells", "", "strict JSON array of assigned experiment/cell identities")
	inputPath := flag.String("input", "", "profile campaign JSONL output")
	campaignClass := flag.String("campaign-class", "", "expected runner-owned campaign class")
	samplesPerCell := flag.Int("samples-per-cell", 1, "exact retained sample count per selected cell")
	retainedInput := flag.Bool("retained-sample-input", false,
		"offline audit only: wrap immutable pre-fix flat Sample JSONL in memory")
	campaignSelectedPath := flag.String("campaign-selected-cells", "",
		"RQ5 only: strict JSON array of the untranslated campaign coordinates")
	rq5MapPath := flag.String("rq5-cell-map", "", "RQ5 only: exact source-controlled coordinate map")
	preregistrationPath := flag.String("preregistration", "", "retained fixed-N concurrency preregistration")
	preregistrationSHA256 := flag.String("preregistration-sha256", "", "exact retained preregistration SHA-256")
	flag.Parse()

	if *experimentID == "" || *selectedPath == "" || *inputPath == "" || *campaignClass == "" {
		fmt.Fprintln(os.Stderr, "-experiment, -selected-cells, -input, and -campaign-class are required")
		os.Exit(2)
	}
	if *campaignClass != "pilot" && *campaignClass != "publication" {
		fail(fmt.Errorf("profile campaign launcher gate requires campaign_class pilot or publication"))
	}
	selectedBytes, err := os.ReadFile(*selectedPath)
	if err != nil {
		fail(err)
	}
	var selected []string
	if err := experiment.StrictJSON(selectedBytes, &selected); err != nil {
		fail(fmt.Errorf("decode selected cells: %w", err))
	}
	if *experimentID == "rq5" {
		if *campaignSelectedPath == "" || *rq5MapPath == "" {
			fail(fmt.Errorf("RQ5 launcher gate requires -campaign-selected-cells and -rq5-cell-map"))
		}
		campaignSelectedBytes, err := os.ReadFile(*campaignSelectedPath)
		if err != nil {
			fail(err)
		}
		var campaignSelected []string
		if err := experiment.StrictJSON(campaignSelectedBytes, &campaignSelected); err != nil {
			fail(fmt.Errorf("decode RQ5 campaign-selected cells: %w", err))
		}
		if err := validateRQ5LauncherSelection(*rq5MapPath, campaignSelected, selected); err != nil {
			fail(err)
		}
	} else if *campaignSelectedPath != "" || *rq5MapPath != "" {
		fail(fmt.Errorf("RQ5 coordinate mapping flags are invalid for experiment %s", *experimentID))
	}

	var records []experiment.ProfileCampaignSampleV1
	if *retainedInput {
		if *campaignClass != "pilot" {
			fail(fmt.Errorf("offline retained-sample wrapping is pilot-only"))
		}
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
	var preregistration *concurrencyfixture.Preregistration
	if *preregistrationPath != "" || *preregistrationSHA256 != "" {
		if *preregistrationPath == "" || *preregistrationSHA256 == "" {
			fail(fmt.Errorf("preregistration path and SHA-256 must be supplied together"))
		}
		loaded, digest, err := concurrencyfixture.LoadPreregistration(*preregistrationPath)
		if err != nil {
			fail(err)
		}
		if digest != *preregistrationSHA256 {
			fail(fmt.Errorf("retained preregistration SHA-256 differs from the launcher anchor"))
		}
		preregistration = &loaded
	}
	var gateErr error
	if preregistration != nil {
		if *campaignClass != "pilot" {
			fail(fmt.Errorf("publication launcher cannot attach the pilot preregistration"))
		}
		gateErr = experiment.ValidateProfileCampaignExperimentGateWithPreregistration(
			*experimentID, selected, records, *samplesPerCell, preregistration, *preregistrationSHA256)
	} else {
		gateErr = experiment.ValidateProfileCampaignExperimentGateForClass(
			*campaignClass, *experimentID, selected, records, *samplesPerCell)
	}
	if gateErr != nil {
		fail(gateErr)
	}
	result := map[string]any{
		"schema_version": 1,
		"status":         "pass",
		"experiment_id":  *experimentID,
		"campaign_class": *campaignClass,
		"samples":        len(records),
		"selected_cells": len(selected),
	}
	inputBytes, err := os.ReadFile(*inputPath)
	if err != nil {
		fail(err)
	}
	inputDigest := sha256.Sum256(inputBytes)
	result["input_sha256"] = hex.EncodeToString(inputDigest[:])
	if preregistration != nil {
		passes, misses := 0, 0
		for _, record := range records {
			if record.Sample.ExperimentID+"/"+record.Sample.CellID != preregistration.Cell {
				continue
			}
			observedPass, err := experiment.ValidatePreregisteredConcurrencyRound(record.Sample)
			if err != nil {
				fail(err)
			}
			if observedPass {
				passes++
			} else {
				misses++
			}
		}
		result["preregistration_sha256"] = *preregistrationSHA256
		result["preregistered_round_passes"] = passes
		result["preregistered_round_misses"] = misses
	}
	_ = json.NewEncoder(os.Stdout).Encode(result)
}

func validateRQ5LauncherSelection(mapPath string, campaignSelected, experimentSelected []string) error {
	mapping, _, err := finalv5profile.LoadRQ5WorkloadCellMap(mapPath)
	if err != nil {
		return err
	}
	translated, err := mapping.TranslateCampaignCells(campaignSelected)
	if err != nil {
		return err
	}
	if !sameStrings(translated, experimentSelected) {
		return fmt.Errorf("RQ5 runner-selected cells do not match the explicit campaign coordinate translation")
	}
	return nil
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	counts := make(map[string]int, len(left))
	for _, value := range left {
		counts[value]++
	}
	for _, value := range right {
		counts[value]--
	}
	for _, count := range counts {
		if count != 0 {
			return false
		}
	}
	return true
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
