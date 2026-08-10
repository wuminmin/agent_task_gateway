package finalv5oracle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	ManifestSchemaVersion   = 1
	ManifestOracleVersion   = "taskgate-final-v5-independent-oracle-v1"
	ManifestContractVersion = "taskgate-final-v5-author-contract-v1"
)

// OracleManifest is a credential-free, deterministic statement of one
// evaluation cell's independently generated expectations.  Semantic set
// digests in this structure never stand in for production bitmap/radix
// digests or runtime Parquet/object hashes.
type OracleManifest struct {
	SchemaVersion           int                `json:"schema_version"`
	OracleVersion           string             `json:"oracle_version"`
	ContractVersion         string             `json:"contract_version"`
	ExperimentID            string             `json:"experiment_id"`
	WorkloadID              string             `json:"workload_id"`
	Scale                   string             `json:"scale"`
	Mode                    string             `json:"mode"`
	DatasetSpecSHA256       string             `json:"dataset_spec_sha256"`
	CatalogSpecSHA256       string             `json:"catalog_spec_sha256"`
	QuerySpecSHA256         string             `json:"query_spec_sha256"`
	NormalizationSpecSHA256 string             `json:"normalization_spec_sha256"`
	Expected                ManifestExpected   `json:"expected"`
	Generation              ManifestGeneration `json:"generation"`
}

type ManifestExpected struct {
	RowCount                       *int64 `json:"row_count,omitempty"`
	ColumnCount                    *int   `json:"column_count,omitempty"`
	NormalizedSchemaSHA256         string `json:"normalized_schema_sha256,omitempty"`
	CanonicalResultSHA256          string `json:"canonical_result_sha256,omitempty"`
	ReleaseCandidateCardinality    *int64 `json:"release_candidate_cardinality,omitempty"`
	DependencyCandidateCardinality *int64 `json:"dependency_candidate_cardinality,omitempty"`
	OutcomeCandidateCardinality    *int64 `json:"outcome_candidate_cardinality,omitempty"`
	ReleaseCandidateSetSHA256      string `json:"release_candidate_set_sha256,omitempty"`
	DependencyCandidateSetSHA256   string `json:"dependency_candidate_set_sha256,omitempty"`
	OutcomeCandidateSetSHA256      string `json:"outcome_candidate_set_sha256,omitempty"`
	ExistingCardinality            *int64 `json:"existing_cardinality,omitempty"`
	ExistingSetSHA256              string `json:"existing_set_sha256,omitempty"`
	OverlapCardinality             *int64 `json:"overlap_cardinality,omitempty"`
	OverlapSetSHA256               string `json:"overlap_set_sha256,omitempty"`
	NovelCardinality               *int64 `json:"novel_cardinality,omitempty"`
	NovelSetSHA256                 string `json:"novel_set_sha256,omitempty"`
	UnionCardinality               *int64 `json:"union_cardinality,omitempty"`
	UnionSetSHA256                 string `json:"union_set_sha256,omitempty"`
	ScheduleSHA256                 string `json:"schedule_sha256,omitempty"`
}

type ManifestGeneration struct {
	Seed             int64  `json:"seed"`
	GeneratorVersion string `json:"generator_version"`
	Command          string `json:"command"`
}

func Int64(value int64) *int64 { return &value }
func Int(value int) *int       { return &value }

// CanonicalManifest returns the one accepted JSON representation.  Go struct
// field order is intentional; HTML escaping and insignificant indentation are
// disabled, and a final LF is part of the source-controlled byte contract.
func CanonicalManifest(manifest OracleManifest) ([]byte, error) {
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(manifest); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func ManifestSHA256(manifest OracleManifest) (string, error) {
	value, err := CanonicalManifest(manifest)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:]), nil
}

// DecodeManifest accepts only canonical JSON with no duplicate/unknown keys.
// This prevents a reviewer and a runtime decoder from binding different bytes.
func DecodeManifest(value []byte) (OracleManifest, error) {
	var manifest OracleManifest
	if len(value) == 0 || !json.Valid(value) {
		return manifest, errors.New("oracle manifest is not valid JSON")
	}
	if err := rejectDuplicateJSON(value); err != nil {
		return manifest, err
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return manifest, errors.New("oracle manifest has trailing JSON")
	}
	canonical, err := CanonicalManifest(manifest)
	if err != nil {
		return manifest, err
	}
	if !bytes.Equal(value, canonical) {
		return manifest, errors.New("oracle manifest is not canonical JSON")
	}
	return manifest, nil
}

func (manifest OracleManifest) Validate() error {
	if manifest.SchemaVersion != ManifestSchemaVersion || manifest.OracleVersion != ManifestOracleVersion ||
		manifest.ContractVersion != ManifestContractVersion {
		return errors.New("oracle manifest version is unsupported")
	}
	for name, value := range map[string]string{
		"experiment_id": manifest.ExperimentID, "workload_id": manifest.WorkloadID,
		"scale": manifest.Scale, "mode": manifest.Mode,
		"generator_version": manifest.Generation.GeneratorVersion,
	} {
		if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\x00\r\n\t") {
			return fmt.Errorf("oracle manifest %s is not a canonical token", name)
		}
	}
	if strings.TrimSpace(manifest.Generation.Command) == "" || strings.ContainsAny(manifest.Generation.Command, "\x00\r\n") {
		return errors.New("oracle manifest generation command is invalid")
	}
	if err := validateManifestIdentity(manifest); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"dataset_spec_sha256": manifest.DatasetSpecSHA256, "catalog_spec_sha256": manifest.CatalogSpecSHA256,
		"query_spec_sha256": manifest.QuerySpecSHA256, "normalization_spec_sha256": manifest.NormalizationSpecSHA256,
	} {
		if !validSHA256(value) {
			return fmt.Errorf("oracle manifest %s is not SHA-256", name)
		}
	}
	if err := manifest.Expected.validate(); err != nil {
		return err
	}
	if err := validateExpectedForWorkload(manifest); err != nil {
		return err
	}
	return nil
}

func validateManifestIdentity(manifest OracleManifest) error {
	switch manifest.ExperimentID {
	case "baseline":
		if len(manifest.WorkloadID) != 2 || manifest.WorkloadID[0] != 'S' || manifest.WorkloadID[1] < '1' || manifest.WorkloadID[1] > '6' {
			return errors.New("baseline oracle manifest workload_id must be S1 through S6")
		}
	case "scale":
		if manifest.WorkloadID != "dependency-e2e" && manifest.WorkloadID != "outcome-merkle" {
			return errors.New("scale oracle manifest workload_id is unsupported")
		}
		if manifest.WorkloadID == "dependency-e2e" {
			if _, err := ParseExposureScaleDependencyCell(manifest.Scale); err != nil {
				return err
			}
			if manifest.Mode != ExposureScaleModeNovel && manifest.Mode != ExposureScaleModeSemanticReplay {
				return errors.New("scale dependency oracle manifest mode is unsupported")
			}
		}
	case "artifact":
		if manifest.WorkloadID != "result-heavy" {
			return errors.New("artifact oracle manifest workload_id must be result-heavy")
		}
	default:
		return errors.New("oracle manifest experiment_id is unsupported")
	}
	return nil
}

func (expected ManifestExpected) validate() error {
	for name, value := range map[string]*int64{
		"row_count": expected.RowCount, "release_candidate_cardinality": expected.ReleaseCandidateCardinality,
		"dependency_candidate_cardinality": expected.DependencyCandidateCardinality,
		"outcome_candidate_cardinality":    expected.OutcomeCandidateCardinality,
		"existing_cardinality":             expected.ExistingCardinality, "overlap_cardinality": expected.OverlapCardinality,
		"novel_cardinality": expected.NovelCardinality, "union_cardinality": expected.UnionCardinality,
	} {
		if value != nil && *value < 0 {
			return fmt.Errorf("oracle manifest %s is negative", name)
		}
	}
	if expected.ColumnCount != nil && *expected.ColumnCount <= 0 {
		return errors.New("oracle manifest column_count is not positive")
	}
	for name, value := range map[string]string{
		"normalized_schema_sha256":        expected.NormalizedSchemaSHA256,
		"canonical_result_sha256":         expected.CanonicalResultSHA256,
		"release_candidate_set_sha256":    expected.ReleaseCandidateSetSHA256,
		"dependency_candidate_set_sha256": expected.DependencyCandidateSetSHA256,
		"outcome_candidate_set_sha256":    expected.OutcomeCandidateSetSHA256,
		"existing_set_sha256":             expected.ExistingSetSHA256, "overlap_set_sha256": expected.OverlapSetSHA256,
		"novel_set_sha256": expected.NovelSetSHA256,
		"union_set_sha256": expected.UnionSetSHA256, "schedule_sha256": expected.ScheduleSHA256,
	} {
		if value != "" && !validSHA256(value) {
			return fmt.Errorf("oracle manifest %s is not SHA-256", name)
		}
	}
	if expected.RowCount != nil {
		if expected.ColumnCount == nil || expected.NormalizedSchemaSHA256 == "" || expected.CanonicalResultSHA256 == "" {
			return errors.New("logical result expectations are incomplete")
		}
	}
	if expected.DependencyCandidateCardinality != nil && expected.DependencyCandidateSetSHA256 == "" {
		return errors.New("dependency candidate expectation omits its semantic set digest")
	}
	for _, pair := range []struct {
		name        string
		cardinality *int64
		digest      string
	}{
		{"release candidate", expected.ReleaseCandidateCardinality, expected.ReleaseCandidateSetSHA256},
		{"dependency candidate", expected.DependencyCandidateCardinality, expected.DependencyCandidateSetSHA256},
		{"outcome candidate", expected.OutcomeCandidateCardinality, expected.OutcomeCandidateSetSHA256},
		{"existing", expected.ExistingCardinality, expected.ExistingSetSHA256},
		{"overlap", expected.OverlapCardinality, expected.OverlapSetSHA256},
		{"novel", expected.NovelCardinality, expected.NovelSetSHA256},
		{"union", expected.UnionCardinality, expected.UnionSetSHA256},
	} {
		if (pair.cardinality == nil) != (pair.digest == "") {
			return fmt.Errorf("oracle manifest %s cardinality and digest must appear together", pair.name)
		}
	}
	return nil
}

func validateExpectedForWorkload(manifest OracleManifest) error {
	expected := manifest.Expected
	requireLogical := func() error {
		if expected.RowCount == nil || expected.ColumnCount == nil || expected.NormalizedSchemaSHA256 == "" || expected.CanonicalResultSHA256 == "" {
			return errors.New("workload requires complete logical result expectations")
		}
		return nil
	}
	requireCandidate := func(name string, cardinality *int64, digest string) error {
		if cardinality == nil || digest == "" {
			return fmt.Errorf("workload requires %s candidate cardinality and digest", name)
		}
		return nil
	}
	requireAlgebra := func(candidate *int64) error {
		if candidate == nil || expected.ExistingCardinality == nil || expected.OverlapCardinality == nil ||
			expected.NovelCardinality == nil || expected.UnionCardinality == nil || expected.ExistingSetSHA256 == "" ||
			expected.OverlapSetSHA256 == "" || expected.NovelSetSHA256 == "" || expected.UnionSetSHA256 == "" {
			return errors.New("workload requires complete existing/overlap/novel/union set algebra")
		}
		if *expected.OverlapCardinality > *candidate || *expected.OverlapCardinality > *expected.ExistingCardinality ||
			*expected.NovelCardinality != *candidate-*expected.OverlapCardinality ||
			*expected.UnionCardinality != *expected.ExistingCardinality+*expected.NovelCardinality {
			return errors.New("oracle manifest set-algebra cardinalities are inconsistent")
		}
		return nil
	}

	switch {
	case manifest.ExperimentID == "baseline":
		if err := requireLogical(); err != nil {
			return err
		}
		if manifest.WorkloadID == "S3" {
			for _, input := range []struct {
				name        string
				cardinality *int64
				digest      string
			}{
				{"release", expected.ReleaseCandidateCardinality, expected.ReleaseCandidateSetSHA256},
				{"dependency", expected.DependencyCandidateCardinality, expected.DependencyCandidateSetSHA256},
				{"outcome", expected.OutcomeCandidateCardinality, expected.OutcomeCandidateSetSHA256},
			} {
				if err := requireCandidate(input.name, input.cardinality, input.digest); err != nil {
					return err
				}
			}
		}
	case manifest.ExperimentID == "artifact":
		return requireLogical()
	case manifest.ExperimentID == "scale" && manifest.WorkloadID == "dependency-e2e":
		if err := requireLogical(); err != nil {
			return err
		}
		// Outcome identity additionally binds the final publication digest,
		// exact Catalog bytes, and task scope. Those inputs remain explicitly
		// NOT_GENERATED/NOT_APPROVED, so decision 14 forbids C1 from emitting an
		// outcome digest. C1 closes the independently fixed release/dependency
		// material only.
		if expected.OutcomeCandidateCardinality != nil || expected.OutcomeCandidateSetSHA256 != "" {
			return errors.New("Scale dependency outcome identity requires frozen publication, Catalog, and scope inputs")
		}
		for _, input := range []struct {
			name        string
			cardinality *int64
			digest      string
		}{
			{"release", expected.ReleaseCandidateCardinality, expected.ReleaseCandidateSetSHA256},
			{"dependency", expected.DependencyCandidateCardinality, expected.DependencyCandidateSetSHA256},
		} {
			if err := requireCandidate(input.name, input.cardinality, input.digest); err != nil {
				return err
			}
		}
		return requireAlgebra(expected.DependencyCandidateCardinality)
	case manifest.ExperimentID == "scale" && manifest.WorkloadID == "outcome-merkle":
		if err := requireCandidate("outcome", expected.OutcomeCandidateCardinality, expected.OutcomeCandidateSetSHA256); err != nil {
			return err
		}
		if expected.ScheduleSHA256 == "" {
			return errors.New("outcome-merkle workload requires an exact overlap schedule digest")
		}
		return requireAlgebra(expected.OutcomeCandidateCardinality)
	}
	return nil
}

func rejectDuplicateJSON(value []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(value))
	var consume func() error
	consume = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, composite := token.(json.Delim)
		if !composite {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok || seen[key] {
					return errors.New("oracle manifest contains a duplicate object key")
				}
				seen[key] = true
				if err := consume(); err != nil {
					return err
				}
			}
			_, err := decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := consume(); err != nil {
					return err
				}
			}
			_, err := decoder.Token()
			return err
		default:
			return errors.New("oracle manifest contains an invalid delimiter")
		}
	}
	if err := consume(); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("oracle manifest has trailing JSON")
	}
	return nil
}
