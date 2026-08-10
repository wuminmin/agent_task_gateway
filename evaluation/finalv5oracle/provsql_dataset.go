package finalv5oracle

import "errors"

const (
	ProvSQLOrdersProductID   = "provsql_orders"
	ProvSQLLineitemProductID = "provsql_lineitem"
	ProvSQLNonceProductID    = "provsql_nonce"
	ProvSQLSnapshot          = "final-v5-provsql-2026-v1"
	ProvSQLDatasetRows       = int64(301_000)
)

// ProvSQLDatasetProductSpecs returns the exact three typed Products admitted
// by the frozen nonce-join workload. It deliberately excludes the two sibling
// benchmark Products and returns detached copies suitable for a read-only live
// stream adapter.
func ProvSQLDatasetProductSpecs() []DatasetProductSpec {
	products, err := provSQLDatasetProducts()
	if err != nil {
		return nil
	}
	result := make([]DatasetProductSpec, len(products))
	for index, product := range products {
		result[index] = DatasetProductSpec{
			ProductID: product.productID, SourceNamespace: product.sourceNamespace,
			Snapshot: product.snapshot, Fields: append([]DatasetField(nil), product.fields...),
			RowCount: product.rowCount,
		}
	}
	return result
}

// ProvSQLDatasetFingerprint regenerates only the three frozen ProvSQL Products
// from their typed Product formulas. No fixture, production response, sample,
// evidence, or Scale oracle output is an input.
func ProvSQLDatasetFingerprint() (DatasetFingerprintSummary, error) {
	products, err := provSQLDatasetProducts()
	if err != nil {
		return DatasetFingerprintSummary{}, err
	}
	return fingerprintBenchmarkDataset(products, BenchmarkDatasetGeneratorSeed, nil)
}

// ProvSQLDatasetFingerprintFromStreams compares the same three-Product model
// with bounded, stable-key-ordered live typed streams.
func ProvSQLDatasetFingerprintFromStreams(streams map[string]DatasetRowStream) (DatasetFingerprintSummary, error) {
	products, err := provSQLDatasetProducts()
	if err != nil {
		return DatasetFingerprintSummary{}, err
	}
	return fingerprintBenchmarkDatasetWithStreams(products, BenchmarkDatasetGeneratorSeed, streams, nil)
}

func provSQLDatasetProducts() ([]benchmarkDatasetProduct, error) {
	all := benchmarkDatasetProducts()
	if len(all) < 3 || all[0].productID != ProvSQLOrdersProductID ||
		all[1].productID != ProvSQLLineitemProductID || all[2].productID != ProvSQLNonceProductID {
		return nil, errors.New("the frozen benchmark Dataset no longer begins with the three ProvSQL Products")
	}
	result := cloneProvSQLDatasetProducts(all[:3])
	if result[0].snapshot != ProvSQLSnapshot || result[1].snapshot != ProvSQLSnapshot ||
		result[2].snapshot != ProvSQLSnapshot ||
		result[0].rowCount+result[1].rowCount+result[2].rowCount != ProvSQLDatasetRows {
		return nil, errors.New("the frozen ProvSQL typed Product model changed")
	}
	return result, nil
}

func cloneProvSQLDatasetProducts(input []benchmarkDatasetProduct) []benchmarkDatasetProduct {
	result := append([]benchmarkDatasetProduct(nil), input...)
	for index := range result {
		result[index].fields = append([]DatasetField(nil), input[index].fields...)
	}
	return result
}
