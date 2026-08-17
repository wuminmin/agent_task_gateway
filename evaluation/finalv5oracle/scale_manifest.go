package finalv5oracle

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"path"
	"reflect"
	"strconv"
)

const (
	ExposureScaleManifestGeneratorVersion = "taskgate-final-v5-exposure-scale-semantic-manifest-v1"

	ExposureScaleDatasetSpecSHA256       = "2f67aa9ee55dd6eb2d7da4eaf1c8d50fd7e5e65f65492f7f1bb231d1c1f25027"
	ExposureScaleCatalogSpecSHA256       = "9d43f8206879423cae808b9d8ffa10e1bf57334019f5672a86d6f0c23b8d4772"
	ExposureScaleNormalizationSpecSHA256 = "9e78abdc2130b19cb414d520edeb6da4dfac0f481609efd244f22f6ba2b1e816"

	ExposureScaleCandidateBDGQuerySHA256    = "8ca770c7f91e039c927e25692514a18c2b5953e1f8645e0bc1556c500769fb1c"
	ExposureScaleCandidateDirectQuerySHA256 = "6c848e270ea32b887543a1145c737568c0f178cf5f8c165b057bfe061603808f"
	ExposureScaleHistoryBDGQuerySHA256      = "e15a2eeaca1bddf61550f01a04fbd88d36135f9e25f8315f1363738a31ea20fb"
	ExposureScaleHistoryDirectQuerySHA256   = "95e3513eb54cbf53ca8616ca1ba806077a98f1f1d55b6679546e3c45d4456a1c"
	ExposureScaleCombinedQuerySpecSHA256    = "aa36d0260315b648079d01e78a36b1ec87f3f371e150477d7f8ab47920d7a2b2"

	ExposureScaleModeNovel          = "novel"
	ExposureScaleModeSemanticReplay = "semantic_replay"

	exposureScaleQuerySpecDomain = "TASKGATE-FINAL-V5-EXPOSURE-SCALE-QUERY-SPEC-V1\x00"
)

type ExposureScaleManifestSpecHashes struct {
	Dataset       string `json:"dataset_spec_sha256"`
	Catalog       string `json:"catalog_spec_sha256"`
	Query         string `json:"query_spec_sha256"`
	Normalization string `json:"normalization_spec_sha256"`
}

type ExposureScaleDependencyCell struct {
	Scale          string `json:"scale"`
	CandidateFacts int64  `json:"candidate_facts"`
	OverlapPercent int    `json:"overlap_percent"`
	OverlapFacts   int64  `json:"overlap_facts"`
}

type ExposureScaleManifestArtifact struct {
	RelativePath string         `json:"path"`
	SHA256       string         `json:"sha256"`
	Manifest     OracleManifest `json:"-"`
}

var exposureScaleDependencyCells = []ExposureScaleDependencyCell{
	{Scale: "10k-overlap-0", CandidateFacts: DependencyScale10K, OverlapPercent: 0, OverlapFacts: 0},
	{Scale: "10k-overlap-50", CandidateFacts: DependencyScale10K, OverlapPercent: 50, OverlapFacts: 5_000},
	{Scale: "10k-overlap-90", CandidateFacts: DependencyScale10K, OverlapPercent: 90, OverlapFacts: 9_000},
	{Scale: "10k-overlap-100", CandidateFacts: DependencyScale10K, OverlapPercent: 100, OverlapFacts: 10_000},
	{Scale: "100k-overlap-0", CandidateFacts: DependencyScale100K, OverlapPercent: 0, OverlapFacts: 0},
	{Scale: "100k-overlap-50", CandidateFacts: DependencyScale100K, OverlapPercent: 50, OverlapFacts: 50_000},
	{Scale: "100k-overlap-90", CandidateFacts: DependencyScale100K, OverlapPercent: 90, OverlapFacts: 90_000},
	{Scale: "100k-overlap-100", CandidateFacts: DependencyScale100K, OverlapPercent: 100, OverlapFacts: 100_000},
	{Scale: "1035000-overlap-0", CandidateFacts: DependencyScale1035000, OverlapPercent: 0, OverlapFacts: 0},
	{Scale: "1035000-overlap-50", CandidateFacts: DependencyScale1035000, OverlapPercent: 50, OverlapFacts: 517_500},
	{Scale: "1035000-overlap-90", CandidateFacts: DependencyScale1035000, OverlapPercent: 90, OverlapFacts: 931_500},
	{Scale: "1035000-overlap-100", CandidateFacts: DependencyScale1035000, OverlapPercent: 100, OverlapFacts: 1_035_000},
}

func FrozenExposureScaleManifestSpecHashes() ExposureScaleManifestSpecHashes {
	return ExposureScaleManifestSpecHashes{
		Dataset: ExposureScaleDatasetSpecSHA256, Catalog: ExposureScaleCatalogSpecSHA256,
		Query: ExposureScaleQuerySpecSHA256(), Normalization: ExposureScaleNormalizationSpecSHA256,
	}
}

func (specs ExposureScaleManifestSpecHashes) Validate() error {
	want := FrozenExposureScaleManifestSpecHashes()
	if specs != want {
		return fmt.Errorf("exposure-scale manifest Spec hashes are %+v; expected the reviewed fixed binding %+v", specs, want)
	}
	return nil
}

// ExposureScaleQuerySpecSHA256 binds both BDG queries and both independent
// Direct result queries. The oracle never parses these SQL files; source-hash
// tests prove that the four reviewed byte strings still match these inputs.
func ExposureScaleQuerySpecSHA256() string {
	target := sha256.New()
	_, _ = target.Write([]byte(exposureScaleQuerySpecDomain))
	inputs := []struct{ name, digest string }{
		{"candidate-bdg", ExposureScaleCandidateBDGQuerySHA256},
		{"candidate-direct", ExposureScaleCandidateDirectQuerySHA256},
		{"history-bdg", ExposureScaleHistoryBDGQuerySHA256},
		{"history-direct", ExposureScaleHistoryDirectQuerySHA256},
	}
	writeUint64(target, uint64(len(inputs)))
	for _, input := range inputs {
		writeFramed(target, []byte(input.name))
		decoded, _ := hex.DecodeString(input.digest)
		writeFramed(target, decoded)
	}
	return hex.EncodeToString(target.Sum(nil))
}

func ExposureScaleDependencyCells() []ExposureScaleDependencyCell {
	return append([]ExposureScaleDependencyCell(nil), exposureScaleDependencyCells...)
}

// ExposureScaleHistoryResultSummary independently evaluates the fixed
// history sum for one of the twelve Scale cells. It consumes the same sole
// source-controlled Product row formula as Dataset agreement and FactSet
// generation; it neither executes SQL nor consumes a production result.
func ExposureScaleHistoryResultSummary(scale string) (ResultSummary, error) {
	cell, err := ParseExposureScaleDependencyCell(scale)
	if err != nil {
		return ResultSummary{}, err
	}
	m := cell.CandidateFacts / ExposureScaleFactsPerRow
	k := cell.OverlapFacts / ExposureScaleFactsPerRow
	lowerExclusive, upperInclusive := m-k, 2*m-k
	total := new(big.Rat)
	for rank := lowerExclusive + 1; rank <= upperInclusive; rank++ {
		row, rowErr := exposureScaleDatasetRow(rank - 1)
		if rowErr != nil {
			return ResultSummary{}, fmt.Errorf("history row %d: %w", rank, rowErr)
		}
		metric, ok := row[1].(string)
		if !ok {
			return ResultSummary{}, errors.New("exposure-scale metric formula did not return exact numeric text")
		}
		value, ok := new(big.Rat).SetString(metric)
		if !ok {
			return ResultSummary{}, errors.New("exposure-scale metric formula returned invalid exact numeric text")
		}
		total.Add(total, value)
	}
	return CanonicalResult([]ResultColumn{{Name: "history_total", Type: SQLNumeric}},
		[][]any{{total.FloatString(2)}})
}

func ParseExposureScaleDependencyCell(scale string) (ExposureScaleDependencyCell, error) {
	for _, cell := range exposureScaleDependencyCells {
		if cell.Scale == scale {
			return cell, nil
		}
	}
	return ExposureScaleDependencyCell{}, fmt.Errorf("exposure-scale dependency scale %q is not one of the 12 fixed cells", scale)
}

func ExposureScaleDependencyManifestPath(scale, mode string) (string, error) {
	if _, err := ParseExposureScaleDependencyCell(scale); err != nil {
		return "", err
	}
	if mode != ExposureScaleModeNovel && mode != ExposureScaleModeSemanticReplay {
		return "", fmt.Errorf("exposure-scale dependency mode %q is not fixed", mode)
	}
	return path.Join("scale", "dependency-e2e", scale, mode+".json"), nil
}

// GenerateExposureScaleDependencyManifests independently constructs the exact
// closed 12-by-2 semantic manifest set from the frozen typed Dataset formula.
// It does not accept SQL, production responses, Samples, or evidence bytes.
func GenerateExposureScaleDependencyManifests(specs ExposureScaleManifestSpecHashes,
	options StreamSetOptions) ([]ExposureScaleManifestArtifact, error) {
	if err := specs.Validate(); err != nil {
		return nil, err
	}
	result := make([]ExposureScaleManifestArtifact, 0, 24)
	semantics := make(map[int64]exposureScaleCandidateSemantics, 3)
	for _, cell := range exposureScaleDependencyCells {
		report, err := GenerateExposureScaleDependency(ExposureScaleDependencyRequest{
			CandidateFacts: cell.CandidateFacts, ExistingFacts: cell.CandidateFacts,
			OverlapFacts: cell.OverlapFacts, SetOptions: options,
		})
		if err != nil {
			return nil, fmt.Errorf("generate %s dependency oracle: %w", cell.Scale, err)
		}
		semantic, present := semantics[cell.CandidateFacts]
		if !present {
			semantic, err = buildExposureScaleCandidateSemantics(cell.CandidateFacts,
				report.CandidateWitnessCommitment, options)
			if err != nil {
				return nil, fmt.Errorf("generate %s candidate semantics: %w", cell.Scale, err)
			}
			semantics[cell.CandidateFacts] = semantic
		} else if semantic.CandidateWitnessCommitment != report.CandidateWitnessCommitment {
			return nil, errors.New("candidate witness changed across overlap cells at one formal scale")
		}
		for _, mode := range []string{ExposureScaleModeNovel, ExposureScaleModeSemanticReplay} {
			manifest := exposureScaleManifest(cell, mode, specs, report, semantic)
			relative, _ := ExposureScaleDependencyManifestPath(cell.Scale, mode)
			digest, err := ManifestSHA256(manifest)
			if err != nil {
				return nil, fmt.Errorf("canonicalize %s: %w", relative, err)
			}
			result = append(result, ExposureScaleManifestArtifact{RelativePath: relative, SHA256: digest, Manifest: manifest})
		}
	}
	if len(result) != 24 {
		return nil, fmt.Errorf("generated %d exposure-scale manifests; expected 24", len(result))
	}
	paths, digests := make(map[string]bool, len(result)), make(map[string]bool, len(result))
	for _, artifact := range result {
		if paths[artifact.RelativePath] || digests[artifact.SHA256] {
			return nil, errors.New("exposure-scale manifest paths and SHA-256 values must both be unique")
		}
		paths[artifact.RelativePath], digests[artifact.SHA256] = true, true
	}
	return result, nil
}

// VerifyExposureScaleDependencyManifest fully regenerates one Scale manifest
// and compares every member. Unsupported workloads fail closed.
func VerifyExposureScaleDependencyManifest(manifest OracleManifest, options StreamSetOptions) error {
	if manifest.ExperimentID != "scale" || manifest.WorkloadID != "dependency-e2e" {
		return errors.New("manifest is not a supported exposure-scale dependency manifest")
	}
	cell, err := ParseExposureScaleDependencyCell(manifest.Scale)
	if err != nil {
		return err
	}
	if manifest.Mode != ExposureScaleModeNovel && manifest.Mode != ExposureScaleModeSemanticReplay {
		return errors.New("exposure-scale dependency manifest mode is unsupported")
	}
	specs := ExposureScaleManifestSpecHashes{Dataset: manifest.DatasetSpecSHA256, Catalog: manifest.CatalogSpecSHA256,
		Query: manifest.QuerySpecSHA256, Normalization: manifest.NormalizationSpecSHA256}
	if err := specs.Validate(); err != nil {
		return err
	}
	report, err := GenerateExposureScaleDependency(ExposureScaleDependencyRequest{CandidateFacts: cell.CandidateFacts,
		ExistingFacts: cell.CandidateFacts, OverlapFacts: cell.OverlapFacts, SetOptions: options})
	if err != nil {
		return err
	}
	semantic, err := buildExposureScaleCandidateSemantics(cell.CandidateFacts, report.CandidateWitnessCommitment, options)
	if err != nil {
		return err
	}
	want := exposureScaleManifest(cell, manifest.Mode, specs, report, semantic)
	if !reflect.DeepEqual(manifest, want) {
		return errors.New("exposure-scale dependency manifest differs from independent regeneration")
	}
	return nil
}

// VerifyExposureScaleDependencyManifestSet rejects missing, duplicate, extra,
// non-canonical, or semantically changed files in the fixed 24-cell set.
func VerifyExposureScaleDependencyManifestSet(values map[string][]byte, options StreamSetOptions) ([]ExposureScaleManifestArtifact, error) {
	if len(values) != 24 {
		return nil, fmt.Errorf("exposure-scale manifest set has %d files; expected exactly 24", len(values))
	}
	want, err := GenerateExposureScaleDependencyManifests(FrozenExposureScaleManifestSpecHashes(), options)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(want))
	for _, artifact := range want {
		value, present := values[artifact.RelativePath]
		if !present {
			return nil, fmt.Errorf("exposure-scale manifest set omits %q", artifact.RelativePath)
		}
		manifest, err := DecodeManifest(value)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", artifact.RelativePath, err)
		}
		if !reflect.DeepEqual(manifest, artifact.Manifest) {
			return nil, fmt.Errorf("%s differs from independent regeneration", artifact.RelativePath)
		}
		seen[artifact.RelativePath] = true
	}
	for relative := range values {
		if !seen[relative] {
			return nil, fmt.Errorf("exposure-scale manifest set contains unexpected path %q", relative)
		}
	}
	return want, nil
}

func ExposureScaleManifestGenerationCommand(specs ExposureScaleManifestSpecHashes) string {
	return fmt.Sprintf("final-v5-oracle scale-manifests --dataset-spec-sha256 %s --catalog-spec-sha256 %s --query-spec-sha256 %s --normalization-spec-sha256 %s",
		specs.Dataset, specs.Catalog, specs.Query, specs.Normalization)
}

type exposureScaleCandidateSemantics struct {
	CandidateWitnessCommitment string
	Result                     ResultSummary
	Release                    StreamSetSummary
}

func buildExposureScaleCandidateSemantics(candidateFacts int64, witness string,
	options StreamSetOptions) (exposureScaleCandidateSemantics, error) {
	if !isFormalDependencyScale(candidateFacts) || !validSHA256(witness) {
		return exposureScaleCandidateSemantics{}, errors.New("candidate semantics require a formal Scale and exact witness")
	}
	rows := candidateFacts / ExposureScaleFactsPerRow
	result, err := CanonicalResult([]ResultColumn{{Name: "member_count", Type: SQLBigInt}}, [][]any{{rows}})
	if err != nil {
		return exposureScaleCandidateSemantics{}, err
	}
	outputRowKey, err := ComposeOracleCanonicalKeyV2("group-row", "global")
	if err != nil {
		return exposureScaleCandidateSemantics{}, err
	}
	releaseFact, err := BuildV2DerivedFact(V2DerivedInput{
		SnapshotBundle: []V2SnapshotBinding{{SourceNamespace: ExposureScaleSourceNamespace, Snapshot: ExposureScaleSnapshot}},
		OutputRowKey:   outputRowKey, NormalizedExpression: "count(*)", SQLType: "bigint",
		CanonicalValue: "i:" + strconv.FormatInt(rows, 10), WitnessCommitment: witness,
	})
	if err != nil {
		return exposureScaleCandidateSemantics{}, err
	}
	release, err := SummarizeSemanticSet("candidate", func(yield func(string) error) error {
		return yield(releaseFact.SHA256)
	}, options)
	if err != nil {
		return exposureScaleCandidateSemantics{}, err
	}
	if release.Cardinality != 1 {
		return exposureScaleCandidateSemantics{}, errors.New("candidate semantic generator violated release cardinality")
	}
	return exposureScaleCandidateSemantics{CandidateWitnessCommitment: witness, Result: result,
		Release: release}, nil
}

func exposureScaleManifest(cell ExposureScaleDependencyCell, mode string, specs ExposureScaleManifestSpecHashes,
	report DependencyOracleReport, semantic exposureScaleCandidateSemantics) OracleManifest {
	return OracleManifest{
		SchemaVersion: ManifestSchemaVersion, OracleVersion: ManifestOracleVersion,
		ContractVersion: ManifestContractVersion, ExperimentID: "scale", WorkloadID: "dependency-e2e",
		Scale: cell.Scale, Mode: mode, DatasetSpecSHA256: specs.Dataset, CatalogSpecSHA256: specs.Catalog,
		QuerySpecSHA256: specs.Query, NormalizationSpecSHA256: specs.Normalization,
		Expected: ManifestExpected{
			RowCount: Int64(semantic.Result.RowCount), ColumnCount: Int(semantic.Result.ColumnCount),
			NormalizedSchemaSHA256:         semantic.Result.NormalizedSchemaSHA256,
			CanonicalResultSHA256:          semantic.Result.CanonicalResultSHA256,
			ReleaseCandidateCardinality:    Int64(semantic.Release.Cardinality),
			ReleaseCandidateSetSHA256:      semantic.Release.SetSHA256,
			DependencyCandidateCardinality: Int64(report.Candidate.Cardinality),
			DependencyCandidateSetSHA256:   report.Candidate.SetSHA256,
			ExistingCardinality:            Int64(report.Existing.Cardinality), ExistingSetSHA256: report.Existing.SetSHA256,
			OverlapCardinality: Int64(report.Overlap.Cardinality), OverlapSetSHA256: report.Overlap.SetSHA256,
			NovelCardinality: Int64(report.Novel.Cardinality), NovelSetSHA256: report.Novel.SetSHA256,
			UnionCardinality: Int64(report.Union.Cardinality), UnionSetSHA256: report.Union.SetSHA256,
		},
		Generation: ManifestGeneration{Seed: BenchmarkDatasetGeneratorSeed,
			GeneratorVersion: ExposureScaleManifestGeneratorVersion,
			Command:          ExposureScaleManifestGenerationCommand(specs)},
	}
}
