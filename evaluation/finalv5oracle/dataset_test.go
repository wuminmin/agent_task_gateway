package finalv5oracle

import (
	"slices"
	"testing"
)

func TestBenchmarkDatasetFingerprintDeterministicAndBounded(t *testing.T) {
	one, err := BenchmarkDatasetFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	two, err := BenchmarkDatasetFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if one.SHA256 != two.SHA256 || !slices.Equal(one.Products, two.Products) {
		t.Fatal("identical benchmark dataset regeneration changed its fingerprint")
	}
	if one.ProductCount != 5 || one.RowCount != 815_000 || one.PeakBufferedRows != 1 {
		t.Fatalf("dataset summary = %+v", one)
	}
	wantRows := []int64{50_000, 250_000, 1_000, 414_000, 100_000}
	for index, product := range one.Products {
		if product.RowCount != wantRows[index] || !validSHA256(product.SHA256) {
			t.Fatalf("dataset Product %d = %+v", index, product)
		}
	}
	t.Logf("benchmark_dataset_sha256=%s", one.SHA256)
}

func TestBenchmarkDatasetFingerprintBindsSeedProductTypeCollationAndValue(t *testing.T) {
	products := benchmarkDatasetProducts()
	for index := range products {
		products[index].rowCount = min(products[index].rowCount, 3)
	}
	base, err := fingerprintBenchmarkDataset(cloneDatasetProducts(products), BenchmarkDatasetGeneratorSeed, nil)
	if err != nil {
		t.Fatal(err)
	}
	seeded, err := fingerprintBenchmarkDataset(cloneDatasetProducts(products), BenchmarkDatasetGeneratorSeed+1, nil)
	if err != nil || seeded.SHA256 == base.SHA256 {
		t.Fatalf("seed mutation digest=%s err=%v", seeded.SHA256, err)
	}

	mutateSpec := func(change func([]benchmarkDatasetProduct)) string {
		changed := cloneDatasetProducts(products)
		change(changed)
		summary, fingerprintErr := fingerprintBenchmarkDataset(changed, BenchmarkDatasetGeneratorSeed, nil)
		if fingerprintErr != nil {
			t.Fatal(fingerprintErr)
		}
		return summary.SHA256
	}
	for name, digest := range map[string]string{
		"product":   mutateSpec(func(values []benchmarkDatasetProduct) { values[0].productID += "_changed" }),
		"type":      mutateSpec(func(values []benchmarkDatasetProduct) { values[0].fields[1].SQLType = SQLNumeric }),
		"collation": mutateSpec(func(values []benchmarkDatasetProduct) { values[4].fields[1].CollationVersion = "2.37" }),
	} {
		if digest == base.SHA256 {
			t.Fatalf("%s mutation retained dataset digest", name)
		}
	}
	valueChanged, err := fingerprintBenchmarkDataset(cloneDatasetProducts(products), BenchmarkDatasetGeneratorSeed,
		func(product string, row int64, values []any) {
			if product == "provsql_orders" && row == 0 {
				values[1] = int64(2)
			}
		})
	if err != nil || valueChanged.SHA256 == base.SHA256 {
		t.Fatalf("value mutation digest=%s err=%v", valueChanged.SHA256, err)
	}
}

func TestResultHeavyDatasetProjectionMatchesArtifactOracle(t *testing.T) {
	products := benchmarkDatasetProducts()
	resultHeavy := products[len(products)-1]
	for _, index := range []int64{0, 99, 9_999, 99_999} {
		row, err := resultHeavy.row(index)
		if err != nil {
			t.Fatal(err)
		}
		x4, err := ArtifactRow(index, 4)
		if err != nil || !slices.Equal(row[:4], x4) {
			t.Fatalf("row %d x4 mismatch: %v err=%v", index+1, row[:4], err)
		}
	}
}

func TestLiveBenchmarkStreamsUseTheExactFormulaFingerprint(t *testing.T) {
	products := benchmarkDatasetProducts()
	streams := make(map[string]DatasetRowStream, len(products))
	for _, product := range products {
		product := product
		streams[product.productID] = func(yield func([]any) error) error {
			for index := int64(0); index < product.rowCount; index++ {
				row, err := product.row(index)
				if err != nil {
					return err
				}
				if err := yield(row); err != nil {
					return err
				}
			}
			return nil
		}
	}
	formula, err := BenchmarkDatasetFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	live, err := BenchmarkDatasetFingerprintFromStreams(streams)
	if err != nil || live.SHA256 != formula.SHA256 || !slices.Equal(live.Products, formula.Products) {
		t.Fatalf("live fingerprint = %+v, formula = %+v, err=%v", live, formula, err)
	}

	truncated := make(map[string]DatasetRowStream, len(streams))
	for name, stream := range streams {
		truncated[name] = stream
	}
	orders := products[0]
	truncated[orders.productID] = func(yield func([]any) error) error {
		for index := int64(0); index < orders.rowCount-1; index++ {
			row, err := orders.row(index)
			if err != nil {
				return err
			}
			if err := yield(row); err != nil {
				return err
			}
		}
		return nil
	}
	if _, err := BenchmarkDatasetFingerprintFromStreams(truncated); err == nil {
		t.Fatal("truncated live Product stream was accepted")
	}

	specs := BenchmarkDatasetProductSpecs()
	if len(specs) != len(products) || specs[4].ProductID != "final_v5_result_heavy" || len(specs[4].Fields) != 16 {
		t.Fatalf("detached live Product specs = %+v", specs)
	}
}

func cloneDatasetProducts(input []benchmarkDatasetProduct) []benchmarkDatasetProduct {
	result := append([]benchmarkDatasetProduct(nil), input...)
	for index := range result {
		result[index].fields = append([]DatasetField(nil), input[index].fields...)
	}
	return result
}
