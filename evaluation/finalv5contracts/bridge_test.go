package finalv5contracts

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/parquet-go/parquet-go"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
)

const liveCatalogPath = "../../config/catalog.yaml"

func loadBridge(t *testing.T) *Runtime {
	t.Helper()
	runtime, err := LoadRuntime()
	if err != nil {
		t.Fatalf("load contract bridge: %v", err)
	}
	return runtime
}

func TestBridgeBindsEveryFrozenArtifactCell(t *testing.T) {
	runtime := loadBridge(t)
	if runtime.ContractRelease() != contractReleaseV13 {
		t.Fatalf("contract release = %q", runtime.ContractRelease())
	}
	cells, err := runtime.ArtifactCells()
	if err != nil {
		t.Fatal(err)
	}
	if len(cells) != 6 {
		t.Fatalf("contract resolved %d artifact cells", len(cells))
	}
	seen := map[string]bool{}
	for _, cell := range cells {
		query, err := runtime.QueryContract(cell)
		if err != nil {
			t.Fatalf("%s: %v", cell.Identity, err)
		}
		// The rendered text is evidence, so it must carry the exact frozen
		// literal and no unbound placeholder.
		if strings.Contains(query.BDG.SQL, "$") || strings.Contains(query.Direct.SQL, "$") {
			t.Fatalf("%s rendered an unbound placeholder", cell.Identity)
		}
		if len(query.BDG.Parameters) != 1 || query.BDG.Parameters[0].SQLType != "nonnegative_int64" {
			t.Fatalf("%s did not bind exactly one non-negative row parameter", cell.Identity)
		}
		if query.BDG.PublicTool != PublicBDGTool {
			t.Fatalf("%s BDG arm is not the public entrypoint", cell.Identity)
		}
		manifest, digest, err := runtime.OracleManifest(cell)
		if err != nil {
			t.Fatalf("%s: %v", cell.Identity, err)
		}
		if manifest.DatasetSpecSHA256 != runtime.digests[datasetGeneratorPath] {
			t.Fatalf("%s oracle manifest is not bound to the indexed dataset generator", cell.Identity)
		}
		if seen[digest] {
			t.Fatalf("%s reuses another cell's oracle manifest", cell.Identity)
		}
		seen[digest] = true
	}
	if err := runtime.VerifyProjectionPrefix(); err != nil {
		t.Fatalf("x4 is not a stable prefix of x16: %v", err)
	}
}

func TestBridgeCompletenessIsFailClosed(t *testing.T) {
	runtime := loadBridge(t)
	required, err := runtime.ArtifactRequirements()
	if err != nil {
		t.Fatal(err)
	}
	for _, implemented := range [][]CellIdentity{
		nil,
		required[:5],
		append(append([]CellIdentity(nil), required[:5]...), required[0]),
		append(append([]CellIdentity(nil), required...),
			CellIdentity{ExperimentID: "artifact", WorkloadID: "result-heavy", Scale: "1m-x16", Mode: "novel"}),
	} {
		report, err := runtime.ArtifactCompleteness(implemented)
		if err != nil {
			t.Fatal(err)
		}
		if report.Complete {
			t.Fatalf("partial or polluted coverage %v was reported complete", implemented)
		}
	}
	report, err := runtime.ArtifactCompleteness(required)
	if err != nil || !report.Complete {
		t.Fatalf("exact coverage report = %+v, err=%v", report, err)
	}
}

// A contract edited without a matching index update must fail closed rather
// than silently change what an Adapter executes.
func TestBridgeRejectsTamperedContractsAndManifests(t *testing.T) {
	base := embeddedContractFiles(t)
	for name, mutate := range map[string]func(fstest.MapFS){
		"tampered dataset generator": func(files fstest.MapFS) {
			files[datasetGeneratorPath] = &fstest.MapFile{Data: append([]byte("-- drift\n"), files[datasetGeneratorPath].Data...)}
		},
		"tampered artifact contract": func(files fstest.MapFS) {
			files[artifactContractPath] = &fstest.MapFile{
				Data: bytes.Replace(files[artifactContractPath].Data, []byte(`"row_count": 100,`), []byte(`"row_count": 99,`), 1)}
		},
		"tampered query template": func(files fstest.MapFS) {
			files["sql/contracts/S6-x4-bdg.sql"] = &fstest.MapFile{Data: []byte("SELECT 1;\n")}
		},
		"tampered hash-locked protocol": func(files fstest.MapFS) {
			files[workloadManifestPath] = &fstest.MapFile{Data: []byte("schema_version: 1\n")}
		},
		"missing contract": func(files fstest.MapFS) {
			delete(files, normalizationContractPath)
		},
	} {
		files := cloneFS(base)
		mutate(files)
		if _, err := LoadRuntimeFS(files); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}

	// A manifest is only accepted while it still binds the indexed contracts.
	files := cloneFS(base)
	path := "oracle-manifests/artifact/result-heavy/100x4/novel.json"
	files[path] = &fstest.MapFile{Data: bytes.Replace(files[path].Data,
		[]byte(`"canonical_result_sha256":"e9a6f28caed552217cd30da5d405c08bda1d0e73099a0a3918093d697623ddf6"`),
		[]byte(`"canonical_result_sha256":"0000000000000000000000000000000000000000000000000000000000000000"`), 1)}
	runtime, err := LoadRuntimeFS(files)
	if err != nil {
		t.Fatal(err)
	}
	cell, err := runtime.ArtifactCell("100x4", "novel")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runtime.OracleManifest(cell); err == nil {
		t.Fatal("a mutated oracle manifest was accepted")
	}
}

// Wrong schema, wrong row count, wrong logical hash and a tampered Parquet must
// each fail the expected/actual comparison.
func TestBridgeRejectsWrongResultShapeAndContent(t *testing.T) {
	runtime := loadBridge(t)
	cell, err := runtime.ArtifactCell("100x4", "novel")
	if err != nil {
		t.Fatal(err)
	}
	query, err := runtime.QueryContract(cell)
	if err != nil {
		t.Fatal(err)
	}
	good := artifactParquet(t, 100, nil)
	direct := directObservation(t, query.Schema, 100, nil)
	bdg, err := NormalizeBDG(query.Schema, ParquetInput(bytes.NewReader(good), int64(len(good))))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.CompareResults(cell, direct, bdg); err != nil {
		t.Fatalf("the exact frozen result was rejected: %v", err)
	}

	t.Run("wrong row count", func(t *testing.T) {
		short := artifactParquet(t, 99, nil)
		observed, err := NormalizeBDG(query.Schema, ParquetInput(bytes.NewReader(short), int64(len(short))))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.CompareResults(cell, direct, observed); err == nil {
			t.Fatal("a 99-row released artifact was accepted for a 100-row cell")
		}
	})

	t.Run("wrong logical hash", func(t *testing.T) {
		mutated := artifactParquet(t, 100, func(index int, row *artifactParquetRow) {
			if index == 50 {
				*row.Amount = "0.00"
			}
		})
		observed, err := NormalizeBDG(query.Schema, ParquetInput(bytes.NewReader(mutated), int64(len(mutated))))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.CompareResults(cell, direct, observed); err == nil {
			t.Fatal("a released artifact with one mutated cell was accepted")
		}
	})

	t.Run("wrong schema", func(t *testing.T) {
		wide, err := finalv5oracle.ArtifactSchema(16)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := NormalizeBDG(wide, ParquetInput(bytes.NewReader(good), int64(len(good)))); err == nil {
			t.Fatal("an x4 artifact was normalized against the x16 schema")
		}
	})

	t.Run("direct and bdg disagree", func(t *testing.T) {
		shortDirect := directObservation(t, query.Schema, 100, func(index int, row []any) {
			if index == 7 {
				row[1] = "omega"
			}
		})
		if _, err := runtime.CompareResults(cell, shortDirect, bdg); err == nil {
			t.Fatal("disagreeing Direct and BDG results were accepted")
		}
	})

	t.Run("tampered parquet", func(t *testing.T) {
		// Flip a byte inside the encoded column data, after the header.
		tampered := append([]byte(nil), good...)
		tampered[len(tampered)/2] ^= 0xFF
		observed, err := NormalizeBDG(query.Schema, ParquetInput(bytes.NewReader(tampered), int64(len(tampered))))
		if err != nil {
			return
		}
		if _, err := runtime.CompareResults(cell, direct, observed); err == nil {
			t.Fatal("a tampered released artifact was accepted")
		}
	})

	t.Run("empty artifact", func(t *testing.T) {
		if _, err := NormalizeBDG(query.Schema, ParquetInput(bytes.NewReader(nil), 0)); err == nil {
			t.Fatal("an empty released artifact was accepted")
		}
	})
}

// The live Catalog must actually publish the contract Product and Publication,
// with generated digests and the reviewed collation.
func TestBridgeRejectsCatalogBindingMismatch(t *testing.T) {
	runtime := loadBridge(t)
	cell, err := runtime.ArtifactCell("100k-x16", "novel")
	if err != nil {
		t.Fatal(err)
	}
	live := LiveDeployment{CatalogPath: liveCatalogPath,
		CatalogSHA256:      strings.Repeat("a", 64),
		DatasetProbeSHA256: strings.Repeat("b", 64)}
	source, err := os.ReadFile(liveCatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := runtime.BindDeployment(cell, live)
	if err != nil {
		t.Fatalf("the live Catalog does not bind the frozen cell: %v", err)
	}
	if bound.ProductID != "final_v5_result_heavy" || bound.PublicationID != "final-v5-result-heavy-v1" ||
		len(bound.Columns) != 16 || bound.MaxRows < 100_000 || bound.MaxReleaseFacts < 100_000*16 {
		t.Fatalf("live binding = %+v", bound)
	}
	for name, mutate := range map[string]func(string) string{
		"missing Product": func(text string) string {
			return strings.Replace(text, "  - name: final_v5_result_heavy\n", "  - name: final_v5_absent_heavy\n", 1)
		},
		"drifted reporting view": func(text string) string {
			return strings.Replace(text, "    reporting_view: reporting.final_v5_result_heavy\n",
				"    reporting_view: reporting.expense_summary\n", 1)
		},
		"unsupported collation version": func(text string) string {
			return strings.Replace(text, `        collation_version: "2.36"
        description: Deterministic category`, `        collation_version: "2.35"
        description: Deterministic category`, 1)
		},
		"unsupported collation": func(text string) string {
			return strings.Replace(text, `        collation: en_US.utf8
        collation_version: "2.36"
        description: Deterministic region`, `        collation: C
        collation_version: "2.36"
        description: Deterministic region`, 1)
		},
		"fail-closed publication sentinel": func(text string) string {
			return strings.Replace(text, "    sidecar_digest: f50d45943e4388bc9cbc897be0ff337bf76cf004cd55e553fdab8695b567a1fc",
				"    sidecar_digest: "+strings.Repeat("0", 64), 1)
		},
		"missing Publication": func(text string) string {
			return strings.Replace(text, "  - name: final-v5-result-heavy-v1\n    source: travel_demo\n",
				"  - name: final-v5-absent-heavy-v1\n    source: travel_demo\n", 1)
		},
		"narrowed budget ceiling": func(text string) string {
			return strings.Replace(text, "  - name: final-v5-benchmark-low-v1\n    max_queries: 128\n    max_rows: 100000\n",
				"  - name: final-v5-benchmark-low-v1\n    max_queries: 128\n    max_rows: 1000\n", 1)
		},
	} {
		path := filepath.Join(t.TempDir(), "catalog.yaml")
		if err := os.WriteFile(path, []byte(mutate(string(source))), 0o600); err != nil {
			t.Fatal(err)
		}
		mutated := live
		mutated.CatalogPath = path
		if _, err := runtime.BindDeployment(cell, mutated); err == nil {
			t.Fatalf("%s was accepted as a live binding", name)
		}
	}
}

func TestBridgeRejectsNonSHA256LiveDigests(t *testing.T) {
	runtime := loadBridge(t)
	cell, err := runtime.ArtifactCell("100x4", "novel")
	if err != nil {
		t.Fatal(err)
	}
	for _, live := range []LiveDeployment{
		{CatalogPath: liveCatalogPath, CatalogSHA256: "", DatasetProbeSHA256: strings.Repeat("b", 64)},
		{CatalogPath: liveCatalogPath, CatalogSHA256: strings.Repeat("a", 64), DatasetProbeSHA256: "not-a-digest"},
	} {
		if _, err := runtime.BindDeployment(cell, live); err == nil {
			t.Fatalf("live deployment %+v was accepted", live)
		}
	}
}

type artifactParquetRow struct {
	RowID     *int64  `parquet:"row_id,optional"`
	Category  *string `parquet:"category,optional"`
	Amount    *string `parquet:"amount,optional"`
	EventDate *string `parquet:"event_date,optional"`
}

// artifactParquet builds an x4 released artifact from the independent oracle's
// own row generator, optionally mutating one row.
func artifactParquet(t *testing.T, rows int64, mutate func(int, *artifactParquetRow)) []byte {
	t.Helper()
	encoded := make([]artifactParquetRow, 0, rows)
	for index := int64(0); index < rows; index++ {
		values, err := finalv5oracle.ArtifactRow(index, 4)
		if err != nil {
			t.Fatal(err)
		}
		rowID, category := values[0].(int64), values[1].(string)
		amount, eventDate := values[2].(string), values[3].(string)
		row := artifactParquetRow{RowID: &rowID, Category: &category, Amount: &amount, EventDate: &eventDate}
		if mutate != nil {
			mutate(int(index), &row)
		}
		encoded = append(encoded, row)
	}
	var buffer bytes.Buffer
	writer := parquet.NewGenericWriter[artifactParquetRow](&buffer)
	if written, err := writer.Write(encoded); err != nil || written != len(encoded) {
		t.Fatalf("write artifact Parquet = %d, err=%v", written, err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func directObservation(t *testing.T, schema []finalv5oracle.ResultColumn, rows int64,
	mutate func(int, []any)) ObservedResult {
	t.Helper()
	observed, err := NormalizeDirect(schema, func(yield func([]any) error) error {
		for index := int64(0); index < rows; index++ {
			row, err := finalv5oracle.ArtifactRow(index, len(schema))
			if err != nil {
				return err
			}
			if mutate != nil {
				mutate(int(index), row)
			}
			if err := yield(row); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return observed
}

func embeddedContractFiles(t *testing.T) fstest.MapFS {
	t.Helper()
	runtime := loadBridge(t)
	files := fstest.MapFS{}
	for _, name := range []string{indexContractPath, baselineContractPath, artifactContractPath,
		normalizationContractPath, catalogCandidatePath, datasetGeneratorPath, datasetProbePath,
		protocolDocumentPath, workloadManifestPath} {
		value, err := runtime.readContract(name)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = &fstest.MapFile{Data: append([]byte(nil), value...)}
	}
	for path := range runtime.digests {
		if _, present := files[path]; present {
			continue
		}
		value, err := runtime.readContract(path)
		if err != nil {
			t.Fatal(err)
		}
		files[path] = &fstest.MapFile{Data: append([]byte(nil), value...)}
	}
	cells, err := runtime.ArtifactCells()
	if err != nil {
		t.Fatal(err)
	}
	for _, cell := range cells {
		value, err := runtime.readContract(cell.OracleManifestPath)
		if err != nil {
			t.Fatal(err)
		}
		files[cell.OracleManifestPath] = &fstest.MapFile{Data: append([]byte(nil), value...)}
	}
	return files
}

func cloneFS(source fstest.MapFS) fstest.MapFS {
	files := make(fstest.MapFS, len(source))
	for name, file := range source {
		files[name] = &fstest.MapFile{Data: append([]byte(nil), file.Data...)}
	}
	return files
}
