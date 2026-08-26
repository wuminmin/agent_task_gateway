package finalv5oracle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestProvSQLManifestSourceBindings(t *testing.T) {
	root := filepath.Join("..", "final-v5-wsl2")
	tests := map[string]string{
		"sql/datasets/benchmark-v1-generate.sql":                                     ProvSQLDatasetSpecSHA256,
		"catalog/benchmark-contract-v1.yaml":                                         ProvSQLCatalogSpecSHA256,
		"contracts/result-normalization-v1.json":                                     ProvSQLNormalizationSpecSHA256,
		filepath.Join("..", "..", "config", "snapshots", "provsql-orders-v1.json"):   ProvSQLOrdersSnapshotSHA256,
		filepath.Join("..", "..", "config", "snapshots", "provsql-lineitem-v1.json"): ProvSQLLineitemSnapshotSHA256,
		filepath.Join("..", "..", "config", "snapshots", "provsql-nonce-v1.json"):    ProvSQLNonceSnapshotSHA256,
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
	if got := ProvSQLQuerySpecSHA256(); !validSHA256(got) || got != FrozenProvSQLManifestSpecHashes().Query {
		t.Fatalf("ProvSQL fixed Query Spec SHA-256 = %q", got)
	} else {
		t.Logf("provsql_query_spec_sha256=%s", got)
	}
}

func TestProvSQLNonceJoinCellsAreTheExactClosed105(t *testing.T) {
	cells := ProvSQLNonceJoinCells()
	if len(cells) != 105 {
		t.Fatalf("ProvSQL cells = %d, want 105", len(cells))
	}
	reviewed := []struct {
		scale string
		limit int64
		base  int64
	}{{"1k", 1_000, 0}, {"10k", 10_000, 300}, {"45k", 45_000, 600}}
	cursor := 0
	for _, scale := range reviewed {
		for iteration := 1; iteration <= 35; iteration++ {
			warmup := iteration <= 5
			phaseIteration := iteration
			nonce := scale.base + int64(iteration)
			if !warmup {
				phaseIteration = iteration - 5
				nonce = scale.base + 100 + int64(phaseIteration)
			}
			want := ProvSQLNonceJoinCell{Scale: scale.scale, Limit: scale.limit, Nonce: nonce,
				Warmup: warmup, Iteration: phaseIteration, BindingKey: ProvSQLBindingKey(scale.scale, nonce)}
			if cells[cursor] != want {
				t.Fatalf("ProvSQL reviewed cell %d = %+v, want %+v", cursor, cells[cursor], want)
			}
			cursor++
		}
	}
	paths, keys := make(map[string]bool, 105), make(map[string]bool, 105)
	phaseCounts := map[string][2]int{}
	for _, cell := range cells {
		if err := cell.validate(); err != nil {
			t.Fatalf("invalid cell %+v: %v", cell, err)
		}
		parsed, err := ParseProvSQLBindingKey(cell.BindingKey)
		if err != nil || parsed != cell {
			t.Fatalf("binding-key round trip = %+v, %v", parsed, err)
		}
		relative, err := ProvSQLNonceJoinManifestPath(cell.Scale, cell.Nonce)
		if err != nil || paths[relative] || keys[cell.BindingKey] {
			t.Fatalf("duplicate/invalid cell path %q key %q: %v", relative, cell.BindingKey, err)
		}
		paths[relative], keys[cell.BindingKey] = true, true
		counts := phaseCounts[cell.Scale]
		if cell.Warmup {
			counts[0]++
		} else {
			counts[1]++
		}
		phaseCounts[cell.Scale] = counts
	}
	for _, scale := range []string{"1k", "10k", "45k"} {
		if got := phaseCounts[scale]; got != [2]int{5, 30} {
			t.Fatalf("%s warmup/measured count = %v", scale, got)
		}
	}
	for _, invalid := range []string{"", "1k/0", "1k/6", "1k/100", "10k/301 ", "45k/731", "100k/1"} {
		if _, err := ParseProvSQLBindingKey(invalid); err == nil {
			t.Fatalf("invalid binding key %q was accepted", invalid)
		}
	}
}

func TestProvSQLManifestSingleCellRegeneratesAndRejectsMutation(t *testing.T) {
	cell, err := ParseProvSQLBindingKey("1k/1")
	if err != nil {
		t.Fatal(err)
	}
	options := StreamSetOptions{MaxInMemoryMembers: 1_024, CaptureMembers: 4, TempDir: t.TempDir()}
	report, err := GenerateProvSQLNonceJoinDependency(cell, options)
	if err != nil {
		t.Fatal(err)
	}
	base := provSQLManifest(cell, FrozenProvSQLManifestSpecHashes(), report)
	if err := VerifyProvSQLNonceJoinManifest(base, options); err != nil {
		t.Fatal(err)
	}
	if base.Expected.RowCount == nil || *base.Expected.RowCount != 3 ||
		base.Expected.ColumnCount == nil || *base.Expected.ColumnCount != 4 ||
		base.Expected.ReleaseCandidateCardinality == nil || *base.Expected.ReleaseCandidateCardinality != 12 ||
		base.Expected.DependencyCandidateCardinality == nil || *base.Expected.DependencyCandidateCardinality != 29_003 ||
		base.Expected.OutcomeCandidateCardinality != nil || base.Expected.ExistingCardinality != nil {
		t.Fatalf("ProvSQL manifest expectations = %+v", base.Expected)
	}
	mutations := map[string]func(*OracleManifest){
		"binding key": func(value *OracleManifest) { value.BindingKey = "1k/2" },
		"dependency": func(value *OracleManifest) {
			value.Expected.DependencyCandidateSetSHA256 = strings.Repeat("a", 64)
		},
		"release": func(value *OracleManifest) { value.Expected.ReleaseCandidateSetSHA256 = strings.Repeat("b", 64) },
		"result":  func(value *OracleManifest) { value.Expected.CanonicalResultSHA256 = strings.Repeat("c", 64) },
		"spec":    func(value *OracleManifest) { value.QuerySpecSHA256 = strings.Repeat("d", 64) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := base
			mutate(&changed)
			if reflect.DeepEqual(changed, base) {
				t.Fatal("mutation retained original manifest")
			}
			if err := VerifyProvSQLNonceJoinManifest(changed, options); err == nil {
				t.Fatal("semantic verifier accepted a mutated ProvSQL manifest")
			}
		})
	}
}

func TestProvSQLManifestGenerationCommandIsCredentialFree(t *testing.T) {
	command := ProvSQLManifestGenerationCommand(FrozenProvSQLManifestSpecHashes())
	for _, forbidden := range []string{"dsn", "sql-file", "sample", "evidence", "scale-manifests"} {
		if strings.Contains(strings.ToLower(command), forbidden) {
			t.Fatalf("generation command contains forbidden input %q: %s", forbidden, command)
		}
	}
}

func TestProvSQLManifestSchemaRejectsOpenOrMismatchedCells(t *testing.T) {
	value, err := os.ReadFile(filepath.Join("..", "final-v5-wsl2", "oracle-manifests", "provsql",
		"nonce-join-group", "1k", "taskgate", "1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var base map[string]any
	if err := json.Unmarshal(value, &base); err != nil {
		t.Fatal(err)
	}
	validate := oracleManifestSchemaValidator(t)
	mutations := map[string]func(map[string]any){
		"missing binding key":    func(instance map[string]any) { delete(instance, "binding_key") },
		"scale and key disagree": func(instance map[string]any) { instance["scale"] = "10k" },
		"outside closed set":     func(instance map[string]any) { instance["binding_key"] = "1k/6" },
		"premature outcome": func(instance map[string]any) {
			expected := instance["expected"].(map[string]any)
			expected["outcome_candidate_cardinality"] = float64(1)
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
				t.Fatal("JSON Schema accepted an open or mismatched ProvSQL cell")
			}
		})
	}
}
