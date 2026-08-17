package finalv5profile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

const (
	RQ5WorkloadCellMapRecord     = "taskgate-rq5-workload-cell-map-v1"
	RQ5CellTranslationRecord     = "taskgate-rq5-cell-translation-v1"
	RQ5WorkloadCellMapSourcePath = "config/profiles/rq5-workload-cell-map-v1.json"
)

type RQ5WorkloadCellMapping struct {
	CampaignCell   string `json:"campaign_cell"`
	ExperimentCell string `json:"experiment_cell"`
}

type RQ5WorkloadCellMap struct {
	SchemaVersion int                      `json:"schema_version"`
	Record        string                   `json:"record"`
	Mappings      []RQ5WorkloadCellMapping `json:"mappings"`
}

type RQ5CellTranslationEvidence struct {
	SchemaVersion       int      `json:"schema_version"`
	Record              string   `json:"record"`
	MappingSourcePath   string   `json:"mapping_source_path"`
	MappingRetainedPath string   `json:"mapping_retained_path"`
	MappingSHA256       string   `json:"mapping_sha256"`
	CampaignCells       []string `json:"campaign_cells"`
	ExperimentCells     []string `json:"experiment_cells"`
}

func LoadRQ5WorkloadCellMap(path string) (RQ5WorkloadCellMap, string, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return RQ5WorkloadCellMap{}, "", fmt.Errorf("read RQ5 workload cell map: %w", err)
	}
	mapping, err := DecodeRQ5WorkloadCellMap(payload)
	if err != nil {
		return RQ5WorkloadCellMap{}, "", err
	}
	digest := sha256.Sum256(payload)
	return mapping, hex.EncodeToString(digest[:]), nil
}

func DecodeRQ5WorkloadCellMap(payload []byte) (RQ5WorkloadCellMap, error) {
	var mapping RQ5WorkloadCellMap
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&mapping); err != nil {
		return RQ5WorkloadCellMap{}, fmt.Errorf("decode RQ5 workload cell map: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return RQ5WorkloadCellMap{}, errors.New("RQ5 workload cell map has trailing JSON")
	}
	if err := mapping.Validate(); err != nil {
		return RQ5WorkloadCellMap{}, err
	}
	return mapping, nil
}

func (mapping RQ5WorkloadCellMap) Validate() error {
	if mapping.SchemaVersion != 1 || mapping.Record != RQ5WorkloadCellMapRecord || len(mapping.Mappings) == 0 {
		return errors.New("RQ5 workload cell map has an unknown or empty record")
	}
	campaign := make(map[string]bool, len(mapping.Mappings))
	experiment := make(map[string]bool, len(mapping.Mappings))
	for index, pair := range mapping.Mappings {
		if !validRQ5Coordinate(pair.CampaignCell) || !validRQ5Coordinate(pair.ExperimentCell) {
			return fmt.Errorf("RQ5 workload cell map entry %d has an invalid coordinate", index+1)
		}
		if campaign[pair.CampaignCell] {
			return fmt.Errorf("RQ5 campaign coordinate %q is duplicated", pair.CampaignCell)
		}
		if experiment[pair.ExperimentCell] {
			return fmt.Errorf("RQ5 experiment coordinate %q is duplicated", pair.ExperimentCell)
		}
		campaign[pair.CampaignCell] = true
		experiment[pair.ExperimentCell] = true
	}
	return nil
}

func (mapping RQ5WorkloadCellMap) RequireExactCoverage(campaignCells, experimentCells []string) error {
	wantCampaign, err := exactRQ5CellSet("campaign", campaignCells)
	if err != nil {
		return err
	}
	wantExperiment, err := exactRQ5CellSet("experiment", experimentCells)
	if err != nil {
		return err
	}
	gotCampaign := make(map[string]bool, len(mapping.Mappings))
	gotExperiment := make(map[string]bool, len(mapping.Mappings))
	for _, pair := range mapping.Mappings {
		gotCampaign[pair.CampaignCell] = true
		gotExperiment[pair.ExperimentCell] = true
	}
	if !sameStringSet(gotCampaign, wantCampaign) || !sameStringSet(gotExperiment, wantExperiment) {
		return errors.New("RQ5 workload cell map does not exactly cover both frozen coordinate sets")
	}
	return nil
}

func (mapping RQ5WorkloadCellMap) TranslateCampaignCells(cells []string) ([]string, error) {
	lookup := make(map[string]string, len(mapping.Mappings))
	for _, pair := range mapping.Mappings {
		lookup[pair.CampaignCell] = pair.ExperimentCell
	}
	return translateRQ5Cells("campaign", cells, lookup)
}

func (mapping RQ5WorkloadCellMap) TranslateExperimentCells(cells []string) ([]string, error) {
	lookup := make(map[string]string, len(mapping.Mappings))
	for _, pair := range mapping.Mappings {
		lookup[pair.ExperimentCell] = pair.CampaignCell
	}
	return translateRQ5Cells("experiment", cells, lookup)
}

func NewRQ5CellTranslationEvidence(mapping RQ5WorkloadCellMap, mappingSHA, retainedPath string,
	campaignCells []string) (RQ5CellTranslationEvidence, error) {
	if !validCampaignDigest(mappingSHA) || strings.TrimSpace(retainedPath) == "" {
		return RQ5CellTranslationEvidence{}, errors.New("RQ5 cell translation has invalid mapping provenance")
	}
	experimentCells, err := mapping.TranslateCampaignCells(campaignCells)
	if err != nil {
		return RQ5CellTranslationEvidence{}, err
	}
	return RQ5CellTranslationEvidence{
		SchemaVersion: 1, Record: RQ5CellTranslationRecord,
		MappingSourcePath: RQ5WorkloadCellMapSourcePath, MappingRetainedPath: retainedPath,
		MappingSHA256: mappingSHA, CampaignCells: sortedCopy(campaignCells),
		ExperimentCells: sortedCopy(experimentCells),
	}, nil
}

func (evidence RQ5CellTranslationEvidence) Validate(mapping RQ5WorkloadCellMap, mappingSHA string) error {
	if evidence.SchemaVersion != 1 || evidence.Record != RQ5CellTranslationRecord ||
		evidence.MappingSourcePath != RQ5WorkloadCellMapSourcePath ||
		strings.TrimSpace(evidence.MappingRetainedPath) == "" || evidence.MappingSHA256 != mappingSHA {
		return errors.New("RQ5 cell translation evidence has invalid mapping provenance")
	}
	translated, err := mapping.TranslateCampaignCells(evidence.CampaignCells)
	if err != nil {
		return err
	}
	if !equalSortedStrings(translated, evidence.ExperimentCells) {
		return errors.New("RQ5 cell translation evidence does not match the source-controlled map")
	}
	return nil
}

func translateRQ5Cells(kind string, cells []string, lookup map[string]string) ([]string, error) {
	if len(cells) == 0 {
		return nil, fmt.Errorf("RQ5 %s coordinate set is empty", kind)
	}
	seen := make(map[string]bool, len(cells))
	result := make([]string, 0, len(cells))
	for _, cell := range cells {
		if seen[cell] {
			return nil, fmt.Errorf("RQ5 %s coordinate %q is duplicated", kind, cell)
		}
		translated, present := lookup[cell]
		if !present {
			return nil, fmt.Errorf("unknown RQ5 %s coordinate %q", kind, cell)
		}
		seen[cell] = true
		result = append(result, translated)
	}
	sort.Strings(result)
	return result, nil
}

func exactRQ5CellSet(kind string, cells []string) (map[string]bool, error) {
	if len(cells) == 0 {
		return nil, fmt.Errorf("frozen RQ5 %s coordinate set is empty", kind)
	}
	result := make(map[string]bool, len(cells))
	for _, cell := range cells {
		if !validRQ5Coordinate(cell) || result[cell] {
			return nil, fmt.Errorf("frozen RQ5 %s coordinate set is invalid or duplicated", kind)
		}
		result[cell] = true
	}
	return result, nil
}

func validRQ5Coordinate(value string) bool {
	parts := strings.Split(value, "/")
	if len(parts) != 4 || parts[0] != "rq5" {
		return false
	}
	for _, part := range parts[1:] {
		if strings.TrimSpace(part) == "" || strings.TrimSpace(part) != part {
			return false
		}
	}
	return true
}

func sameStringSet(left, right map[string]bool) bool {
	if len(left) != len(right) {
		return false
	}
	for value := range left {
		if !right[value] {
			return false
		}
	}
	return true
}

func sortedCopy(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

func equalSortedStrings(left, right []string) bool {
	left = sortedCopy(left)
	right = sortedCopy(right)
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
