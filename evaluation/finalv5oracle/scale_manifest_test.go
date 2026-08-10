package finalv5oracle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	jsonschema "github.com/google/jsonschema-go/jsonschema"
)

func TestExposureScaleManifestSourceBindings(t *testing.T) {
	root := filepath.Join("..", "final-v5-wsl2")
	tests := map[string]string{
		"sql/datasets/benchmark-v1-generate.sql":              ExposureScaleDatasetSpecSHA256,
		"catalog/benchmark-contract-v1.yaml":                  ExposureScaleCatalogSpecSHA256,
		"contracts/result-normalization-v1.json":              ExposureScaleNormalizationSpecSHA256,
		"sql/contracts/scale-dependency-candidate-bdg.sql":    ExposureScaleCandidateBDGQuerySHA256,
		"sql/contracts/scale-dependency-candidate-direct.sql": ExposureScaleCandidateDirectQuerySHA256,
		"sql/contracts/scale-dependency-history-bdg.sql":      ExposureScaleHistoryBDGQuerySHA256,
		"sql/contracts/scale-dependency-history-direct.sql":   ExposureScaleHistoryDirectQuerySHA256,
	}
	for relative, want := range tests {
		value, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(value)
		if got := hex.EncodeToString(digest[:]); got != want {
			t.Fatalf("%s SHA-256 = %s, want %s", relative, got, want)
		}
	}
	if got := ExposureScaleQuerySpecSHA256(); got != ExposureScaleCombinedQuerySpecSHA256 {
		t.Fatalf("four-query Scale Spec SHA-256 = %q, want %s", got, ExposureScaleCombinedQuerySpecSHA256)
	}
}

func TestExposureScaleManifestCellsMatchTheReviewedContract(t *testing.T) {
	value, err := os.ReadFile(filepath.Join("..", "final-v5-wsl2", "contracts", "scale-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		Cells []struct {
			Workload string `json:"workload"`
			Scale    string `json:"scale"`
			Mode     string `json:"mode"`
			Expected struct {
				CandidateRows int64  `json:"retained_candidate_rows"`
				ExistingRows  int64  `json:"retained_existing_rows"`
				Candidate     int64  `json:"candidate_dependency_cardinality"`
				Existing      int64  `json:"existing_dependency_cardinality"`
				Overlap       int64  `json:"overlap_dependency_cardinality"`
				Novel         int64  `json:"novel_dependency_cardinality"`
				Union         int64  `json:"union_dependency_cardinality"`
				Release       int64  `json:"release_candidate_cardinality"`
				Outcome       int64  `json:"outcome_candidate_cardinality"`
				DigestStatus  string `json:"digest_generation_status"`
				ReviewStatus  string `json:"digest_review_status"`
			} `json:"expected"`
			Query struct {
				CandidateTemplate string `json:"candidate_template"`
				HistoryTemplate   string `json:"history_template"`
				Parameters        struct {
					CandidateMax int64 `json:"candidate_member_max"`
					HistoryLower int64 `json:"history_member_lower_exclusive"`
					HistoryUpper int64 `json:"history_member_upper_inclusive"`
					Overlap      int   `json:"overlap_percent"`
				} `json:"parameters"`
				CandidateInterval       string `json:"candidate_interval"`
				HistoryInterval         string `json:"history_interval"`
				DistinctQueryIdentities bool   `json:"distinct_query_identities"`
				TotalOrderRequired      bool   `json:"total_order_required"`
				ResultOrdering          string `json:"result_ordering"`
			} `json:"query"`
			Direct struct {
				Active            bool   `json:"active"`
				Role              string `json:"role"`
				CandidateTemplate string `json:"candidate_template"`
				HistoryTemplate   string `json:"history_template"`
				CompleteDrain     bool   `json:"complete_drain"`
			} `json:"direct"`
			BDG struct {
				Active                  bool     `json:"active"`
				Entrypoint              string   `json:"entrypoint"`
				CandidateTemplate       string   `json:"candidate_template"`
				HistoryTemplate         string   `json:"history_template"`
				Sequence                []string `json:"sequence"`
				CompletePipelineThrough string   `json:"complete_pipeline_through"`
			} `json:"bdg"`
			Oracle struct {
				ManifestPath string `json:"oracle_manifest_path"`
			} `json:"oracle"`
		} `json:"cells"`
	}
	if err := json.Unmarshal(value, &contract); err != nil {
		t.Fatal(err)
	}
	wantPaths := make(map[string]bool, 24)
	for _, cell := range ExposureScaleDependencyCells() {
		for _, mode := range []string{ExposureScaleModeNovel, ExposureScaleModeSemanticReplay} {
			relative, err := ExposureScaleDependencyManifestPath(cell.Scale, mode)
			if err != nil {
				t.Fatal(err)
			}
			wantPaths["oracle-manifests/"+relative] = true
		}
	}
	seen := make(map[string]bool, 24)
	for _, cell := range contract.Cells {
		if cell.Workload != "dependency-e2e" {
			continue
		}
		fixed, err := ParseExposureScaleDependencyCell(cell.Scale)
		if err != nil {
			t.Fatal(err)
		}
		if cell.Mode != ExposureScaleModeNovel && cell.Mode != ExposureScaleModeSemanticReplay {
			t.Fatalf("contract dependency mode %q is not fixed", cell.Mode)
		}
		if !wantPaths[cell.Oracle.ManifestPath] || seen[cell.Oracle.ManifestPath] {
			t.Fatalf("contract dependency manifest path %q is missing or duplicated", cell.Oracle.ManifestPath)
		}
		seen[cell.Oracle.ManifestPath] = true
		rows, overlapRows := fixed.CandidateFacts/ExposureScaleFactsPerRow, fixed.OverlapFacts/ExposureScaleFactsPerRow
		lower, upper := rows-overlapRows, 2*rows-overlapRows
		if cell.Expected.CandidateRows != rows || cell.Expected.ExistingRows != rows ||
			cell.Expected.Candidate != fixed.CandidateFacts || cell.Expected.Existing != fixed.CandidateFacts ||
			cell.Expected.Overlap != fixed.OverlapFacts || cell.Expected.Novel != fixed.CandidateFacts-fixed.OverlapFacts ||
			cell.Expected.Union != 2*fixed.CandidateFacts-fixed.OverlapFacts ||
			cell.Expected.Release != 1 || cell.Expected.Outcome != 5 ||
			cell.Expected.DigestStatus != "NOT_GENERATED" || cell.Expected.ReviewStatus != "NOT_APPROVED" {
			t.Fatalf("contract cell %s/%s differs from fixed algebra: %+v", cell.Scale, cell.Mode, cell.Expected)
		}
		if cell.Query.Parameters.CandidateMax != rows || cell.Query.Parameters.HistoryLower != lower ||
			cell.Query.Parameters.HistoryUpper != upper || cell.Query.Parameters.Overlap != fixed.OverlapPercent ||
			cell.Query.CandidateInterval != fmt.Sprintf("(0,%d]", rows) ||
			cell.Query.HistoryInterval != fmt.Sprintf("(%d,%d]", lower, upper) ||
			!cell.Query.DistinctQueryIdentities || !cell.Query.TotalOrderRequired || cell.Query.ResultOrdering != "query_order_v1" {
			t.Fatalf("contract cell %s/%s differs from the fixed query interval: %+v", cell.Scale, cell.Mode, cell.Query)
		}
		const candidateBDG = "sql/contracts/scale-dependency-candidate-bdg.sql"
		const historyBDG = "sql/contracts/scale-dependency-history-bdg.sql"
		const candidateDirect = "sql/contracts/scale-dependency-candidate-direct.sql"
		const historyDirect = "sql/contracts/scale-dependency-history-direct.sql"
		wantSequence := []string{"unmeasured_history_prefill", "measured_candidate"}
		if cell.Mode == ExposureScaleModeSemanticReplay {
			wantSequence = []string{"unmeasured_history_prefill", "unmeasured_candidate", "measured_identical_candidate"}
		}
		if cell.Query.CandidateTemplate != candidateBDG || cell.Query.HistoryTemplate != historyBDG ||
			cell.BDG.CandidateTemplate != candidateBDG || cell.BDG.HistoryTemplate != historyBDG ||
			cell.Direct.CandidateTemplate != candidateDirect || cell.Direct.HistoryTemplate != historyDirect ||
			cell.Direct.Active || cell.Direct.Role != "independent PostgreSQL result oracle only" || !cell.Direct.CompleteDrain ||
			!cell.BDG.Active || cell.BDG.Entrypoint != "query" || !reflect.DeepEqual(cell.BDG.Sequence, wantSequence) ||
			cell.BDG.CompletePipelineThrough != "AVAILABLE_AND_COMPOSITE_VERIFY" {
			t.Fatalf("contract cell %s/%s changed a fixed query template: query=%+v bdg=%+v direct=%+v",
				cell.Scale, cell.Mode, cell.Query, cell.BDG, cell.Direct)
		}
	}
	if !reflect.DeepEqual(seen, wantPaths) {
		t.Fatalf("contract dependency path set has %d entries; expected exact 24", len(seen))
	}
}

func TestExposureScaleManifestDoesNotClaimUnfrozenOutcomeIdentity(t *testing.T) {
	cell, err := ParseExposureScaleDependencyCell("10k-overlap-50")
	if err != nil {
		t.Fatal(err)
	}
	report, err := GenerateExposureScaleDependency(ExposureScaleDependencyRequest{
		CandidateFacts: cell.CandidateFacts, ExistingFacts: cell.CandidateFacts, OverlapFacts: cell.OverlapFacts,
		SetOptions: StreamSetOptions{MaxInMemoryMembers: 1024, CaptureMembers: 8, TempDir: t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	semantic, err := buildExposureScaleCandidateSemantics(cell.CandidateFacts, report.CandidateWitnessCommitment,
		StreamSetOptions{MaxInMemoryMembers: 1024, CaptureMembers: 8, TempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	manifest := exposureScaleManifest(cell, ExposureScaleModeNovel, FrozenExposureScaleManifestSpecHashes(), report, semantic)
	if manifest.Expected.OutcomeCandidateCardinality != nil || manifest.Expected.OutcomeCandidateSetSHA256 != "" {
		t.Fatalf("C1 manifest claimed an outcome identity before publication/Catalog/scope freeze: %+v", manifest.Expected)
	}
}

func TestExposureScaleFormalSemanticFixedVectors(t *testing.T) {
	type vector struct {
		result, release, dependency string
	}
	vectors := map[string]vector{
		"10k-overlap-0": {
			result:     "9528745fee7030eb82173d2ba6fa7ad5750da1dee0396a372eac08049ea2ee46",
			release:    "b2d2feb8a38b0a36450c75ed0ce3a998991d97a1abe060500a13a8f3b639c0cc",
			dependency: "d0f6452e4e475a91aa850a7a63a1e6108c72dafcde717805aedc566beb0ecf54",
		},
		"100k-overlap-0": {
			result:     "0ec0a2a22e7e185f12257f7f45060f67c0b3754d4d11f7acff860f47ea63fa0c",
			release:    "7301d8129f3e2f14c8d1126b872aaf5bf1d842205ca5ffaf08502d181f607410",
			dependency: "e2e4dfc1f7763f8351d8d0bfa73f70b1474fc3f0f8abf9edb9c6ea987cdecf99",
		},
		"1035000-overlap-0": {
			result:     "48f7de9160702299adc2cb00311d9d23c378dcd30c6100e34a143346abecdfe1",
			release:    "1a24cf7d957aeadb78a9d73d779662b2fc0c48a570abc674da817903a1452a79",
			dependency: "5d63478f9799ccd4635efded18e17bbb391dd753cdc9e9c429cb668d3c36c09b",
		},
	}
	const schema = "56f599290f1abf6595d635262b973b8eb26db02e04f8b7aaebcc582df7cbe4e2"
	for scale, want := range vectors {
		value, err := os.ReadFile(filepath.Join("..", "final-v5-wsl2", "oracle-manifests", "scale",
			"dependency-e2e", scale, "novel.json"))
		if err != nil {
			t.Fatal(err)
		}
		manifest, err := DecodeManifest(value)
		if err != nil {
			t.Fatal(err)
		}
		if manifest.Expected.NormalizedSchemaSHA256 != schema ||
			manifest.Expected.CanonicalResultSHA256 != want.result ||
			manifest.Expected.ReleaseCandidateSetSHA256 != want.release ||
			manifest.Expected.DependencyCandidateSetSHA256 != want.dependency {
			t.Fatalf("formal Scale semantic vector %s moved: %+v", scale, manifest.Expected)
		}
	}
}

func TestTrackedExposureScaleSemanticManifestsRegenerateAndValidate(t *testing.T) {
	manifestRoot := filepath.Join("..", "final-v5-wsl2", "oracle-manifests")
	validateSchema := oracleManifestSchemaValidator(t)
	values := readTrackedScaleManifestSet(t, manifestRoot)
	artifacts, err := VerifyExposureScaleDependencyManifestSet(values, StreamSetOptions{
		MaxInMemoryMembers: 64 * 1024, CaptureMembers: 8, TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 24 {
		t.Fatalf("verified %d Scale semantic manifests; expected 24", len(artifacts))
	}
	seenPaths, seenDigests := make(map[string]bool, 24), make(map[string]bool, 24)
	for _, artifact := range artifacts {
		value := values[artifact.RelativePath]
		var instance any
		if err := json.Unmarshal(value, &instance); err != nil {
			t.Fatal(err)
		}
		if err := validateSchema(instance); err != nil {
			t.Fatalf("schema rejected %s: %v", artifact.RelativePath, err)
		}
		if seenPaths[artifact.RelativePath] || seenDigests[artifact.SHA256] {
			t.Fatalf("duplicate manifest path or digest: %+v", artifact)
		}
		seenPaths[artifact.RelativePath], seenDigests[artifact.SHA256] = true, true
		manifest := artifact.Manifest
		cell, err := ParseExposureScaleDependencyCell(manifest.Scale)
		if err != nil {
			t.Fatal(err)
		}
		wantOverlap := cell.CandidateFacts * int64(cell.OverlapPercent) / 100
		if manifest.Expected.RowCount == nil || *manifest.Expected.RowCount != 1 ||
			manifest.Expected.ColumnCount == nil || *manifest.Expected.ColumnCount != 1 ||
			manifest.Expected.ReleaseCandidateCardinality == nil || *manifest.Expected.ReleaseCandidateCardinality != 1 ||
			manifest.Expected.DependencyCandidateCardinality == nil || *manifest.Expected.DependencyCandidateCardinality != cell.CandidateFacts ||
			manifest.Expected.ExistingCardinality == nil || *manifest.Expected.ExistingCardinality != cell.CandidateFacts ||
			manifest.Expected.OverlapCardinality == nil || *manifest.Expected.OverlapCardinality != wantOverlap ||
			manifest.Expected.NovelCardinality == nil || *manifest.Expected.NovelCardinality != cell.CandidateFacts-wantOverlap ||
			manifest.Expected.UnionCardinality == nil || *manifest.Expected.UnionCardinality != 2*cell.CandidateFacts-wantOverlap {
			t.Fatalf("%s has inconsistent semantic expectations: %+v", artifact.RelativePath, manifest.Expected)
		}
		if manifest.Expected.OutcomeCandidateCardinality != nil || manifest.Expected.OutcomeCandidateSetSHA256 != "" {
			t.Fatalf("%s froze an outcome before its publication inputs: %+v", artifact.RelativePath, manifest.Expected)
		}
	}

	o0 := manifestByPath(t, artifacts, "scale/dependency-e2e/1035000-overlap-0/novel.json")
	if o0.Expected.ExistingCardinality == nil || *o0.Expected.ExistingCardinality != 1_035_000 ||
		o0.Expected.UnionCardinality == nil || *o0.Expected.UnionCardinality != 2_070_000 {
		t.Fatalf("large o0 did not retain a full existing set and 2N union: %+v", o0.Expected)
	}
	o100 := manifestByPath(t, artifacts, "scale/dependency-e2e/1035000-overlap-100/novel.json")
	if o100.Expected.DependencyCandidateSetSHA256 == o100.Expected.ExistingSetSHA256 ||
		o100.Expected.DependencyCandidateSetSHA256 == o100.Expected.UnionSetSHA256 ||
		o100.Expected.ExistingSetSHA256 == o100.Expected.UnionSetSHA256 {
		t.Fatalf("o100 candidate/existing/union role domains collapsed: %+v", o100.Expected)
	}
}

func TestExposureScaleManifestVerifierRejectsSemanticMutations(t *testing.T) {
	cell, err := ParseExposureScaleDependencyCell("10k-overlap-50")
	if err != nil {
		t.Fatal(err)
	}
	report, err := GenerateExposureScaleDependency(ExposureScaleDependencyRequest{
		CandidateFacts: cell.CandidateFacts, ExistingFacts: cell.CandidateFacts, OverlapFacts: cell.OverlapFacts,
		SetOptions: StreamSetOptions{MaxInMemoryMembers: 1024, CaptureMembers: 8, TempDir: t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	semantic, err := buildExposureScaleCandidateSemantics(cell.CandidateFacts, report.CandidateWitnessCommitment,
		StreamSetOptions{MaxInMemoryMembers: 1024, CaptureMembers: 8, TempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	base := exposureScaleManifest(cell, ExposureScaleModeNovel, FrozenExposureScaleManifestSpecHashes(), report, semantic)
	mutations := map[string]func(*OracleManifest){
		"mode":       func(value *OracleManifest) { value.Mode = "replay" },
		"spec":       func(value *OracleManifest) { value.DatasetSpecSHA256 = strings.Repeat("a", 64) },
		"dependency": func(value *OracleManifest) { value.Expected.DependencyCandidateSetSHA256 = strings.Repeat("b", 64) },
		"existing":   func(value *OracleManifest) { value.Expected.ExistingSetSHA256 = strings.Repeat("c", 64) },
		"release":    func(value *OracleManifest) { value.Expected.ReleaseCandidateSetSHA256 = strings.Repeat("d", 64) },
		"generator":  func(value *OracleManifest) { value.Generation.GeneratorVersion += "-changed" },
		"command":    func(value *OracleManifest) { value.Generation.Command += " --changed yes" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := base
			mutate(&changed)
			if reflect.DeepEqual(changed, base) {
				t.Fatal("mutation retained the original manifest")
			}
			if err := VerifyExposureScaleDependencyManifest(changed, StreamSetOptions{
				MaxInMemoryMembers: 1024, CaptureMembers: 8, TempDir: t.TempDir(),
			}); err == nil {
				t.Fatal("semantic verifier accepted a mutated Scale manifest")
			}
		})
	}
}

func TestExposureScaleManifestSchemaRejectsOpenOrIncompleteCells(t *testing.T) {
	value, err := os.ReadFile(filepath.Join("..", "final-v5-wsl2", "oracle-manifests", "scale",
		"dependency-e2e", "10k-overlap-50", "novel.json"))
	if err != nil {
		t.Fatal(err)
	}
	var base map[string]any
	if err := json.Unmarshal(value, &base); err != nil {
		t.Fatal(err)
	}
	validate := oracleManifestSchemaValidator(t)
	mutations := map[string]func(map[string]any){
		"thirteenth scale": func(instance map[string]any) { instance["scale"] = "1m-overlap-25" },
		"open mode":        func(instance map[string]any) { instance["mode"] = "replay" },
		"missing union": func(instance map[string]any) {
			delete(instance["expected"].(map[string]any), "union_set_sha256")
		},
		"premature outcome": func(instance map[string]any) {
			expected := instance["expected"].(map[string]any)
			expected["outcome_candidate_cardinality"] = float64(5)
			expected["outcome_candidate_set_sha256"] = strings.Repeat("e", 64)
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			encoded, err := json.Marshal(base)
			if err != nil {
				t.Fatal(err)
			}
			var changed map[string]any
			if err := json.Unmarshal(encoded, &changed); err != nil {
				t.Fatal(err)
			}
			mutate(changed)
			if err := validate(changed); err == nil {
				t.Fatal("JSON Schema accepted an open or incomplete Scale cell")
			}
		})
	}
}

func oracleManifestSchemaValidator(t *testing.T) func(any) error {
	t.Helper()
	value, err := os.ReadFile(filepath.Join("..", "final-v5-wsl2", "schema", "oracle-manifest-v1.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(value, &schema); err != nil {
		t.Fatal(err)
	}
	schema.ID = "https://taskgate.local/schema/oracle-manifest-v1"
	resolved, err := schema.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
	if err != nil {
		t.Fatal(err)
	}
	return resolved.Validate
}

func readTrackedScaleManifestSet(t *testing.T, manifestRoot string) map[string][]byte {
	t.Helper()
	values := make(map[string][]byte, 24)
	root := filepath.Join(manifestRoot, "scale")
	err := filepath.WalkDir(root, func(current string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(manifestRoot, current)
		if err != nil {
			return err
		}
		value, err := os.ReadFile(current)
		if err != nil {
			return err
		}
		values[filepath.ToSlash(relative)] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return values
}

func manifestByPath(t *testing.T, artifacts []ExposureScaleManifestArtifact, relative string) OracleManifest {
	t.Helper()
	for _, artifact := range artifacts {
		if artifact.RelativePath == relative {
			return artifact.Manifest
		}
	}
	t.Fatalf("manifest %s was not generated", relative)
	return OracleManifest{}
}
