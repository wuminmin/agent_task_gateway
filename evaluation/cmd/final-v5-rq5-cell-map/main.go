// Command final-v5-rq5-cell-map translates the frozen campaign coordinates
// selected for RQ5 into the distinct frozen coordinates accepted by the RQ5
// experiment runner. It also emits a retained record of both forms and of the
// exact source-controlled mapping bytes used for the translation.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
)

type workloadManifest struct {
	SchemaVersion int `yaml:"schema_version"`
	Profiles      map[string]struct {
		Workloads []struct {
			ID     string   `yaml:"id"`
			Scales []string `yaml:"scales"`
			Modes  []string `yaml:"modes"`
		} `yaml:"workloads"`
	} `yaml:"profiles"`
}

func main() {
	root := flag.String("root", ".", "repository root")
	mapPath := flag.String("map", finalv5profile.RQ5WorkloadCellMapSourcePath, "source-controlled RQ5 cell map")
	registryPath := flag.String("registry", "config/profiles/registry.json", "frozen profile registry")
	workloadsPath := flag.String("workloads", "evaluation/final-v5-wsl2/protocol/workloads-v1.yaml", "frozen workload manifest")
	selectedPath := flag.String("campaign-selected", "", "strict JSON array of selected campaign coordinates")
	translatedPath := flag.String("experiment-selected-out", "", "create-exclusive translated coordinate output")
	evidencePath := flag.String("evidence-out", "", "create-exclusive translation evidence output")
	retainedMapPath := flag.String("retained-map-path", "", "campaign-root-relative retained mapping path")
	flag.Parse()
	if flag.NArg() != 0 || *selectedPath == "" || *translatedPath == "" || *evidencePath == "" || *retainedMapPath == "" {
		fmt.Fprintln(os.Stderr, "RQ5 map requires -campaign-selected, -experiment-selected-out, -evidence-out, and -retained-map-path")
		os.Exit(2)
	}
	if err := run(*root, *mapPath, *registryPath, *workloadsPath, *selectedPath,
		*translatedPath, *evidencePath, *retainedMapPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(root, mapPath, registryPath, workloadsPath, selectedPath, translatedPath, evidencePath,
	retainedMapPath string) error {
	mapping, mappingSHA, err := finalv5profile.LoadRQ5WorkloadCellMap(filepath.Join(root, mapPath))
	if err != nil {
		return err
	}
	campaignUniverse, err := rq5CampaignCells(filepath.Join(root, registryPath))
	if err != nil {
		return err
	}
	experimentUniverse, err := rq5ExperimentCells(filepath.Join(root, workloadsPath))
	if err != nil {
		return err
	}
	if err := mapping.RequireExactCoverage(campaignUniverse, experimentUniverse); err != nil {
		return err
	}
	selected, err := readStrictStringArray(selectedPath)
	if err != nil {
		return err
	}
	evidence, err := finalv5profile.NewRQ5CellTranslationEvidence(mapping, mappingSHA, retainedMapPath, selected)
	if err != nil {
		return err
	}
	if err := writeExclusiveJSON(translatedPath, evidence.ExperimentCells); err != nil {
		return err
	}
	if err := writeExclusiveJSON(evidencePath, evidence); err != nil {
		_ = os.Remove(translatedPath)
		return err
	}
	return nil
}

func rq5CampaignCells(path string) ([]string, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read profile registry: %w", err)
	}
	var registry finalv5profile.Registry
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&registry); err != nil {
		return nil, fmt.Errorf("decode profile registry: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("profile registry has trailing JSON")
	}
	var result []string
	for _, profile := range registry.Profiles {
		for _, cell := range profile.Cells {
			if strings.HasPrefix(cell, "rq5/") {
				result = append(result, cell)
			}
		}
	}
	sort.Strings(result)
	return result, nil
}

func rq5ExperimentCells(path string) ([]string, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read workload manifest: %w", err)
	}
	var manifest workloadManifest
	if err := yaml.Unmarshal(payload, &manifest); err != nil {
		return nil, fmt.Errorf("decode workload manifest: %w", err)
	}
	profile, present := manifest.Profiles["rq5"]
	if manifest.SchemaVersion != 2 || !present {
		return nil, errors.New("workload manifest omits the frozen RQ5 profile")
	}
	var result []string
	for _, workload := range profile.Workloads {
		for _, scale := range workload.Scales {
			for _, mode := range workload.Modes {
				result = append(result, strings.Join([]string{"rq5", workload.ID, scale, mode}, "/"))
			}
		}
	}
	sort.Strings(result)
	return result, nil
}

func readStrictStringArray(path string) ([]string, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var result []string
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("selected cells have trailing JSON")
	}
	return result, nil
}

func writeExclusiveJSON(path string, value any) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}
