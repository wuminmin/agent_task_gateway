package finalv5oracle

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
)

const (
	BenchmarkDatasetSpecID           = "taskgate-final-v5-benchmark-dataset-v1"
	BenchmarkDatasetGeneratorVersion = "taskgate-final-v5-benchmark-generator-v1"
	BenchmarkDatasetGeneratorSeed    = int64(20260803)
	benchmarkDatasetDomainV1         = "TASKGATE-FINAL-V5-BENCHMARK-DATASET-V1\x00"
	benchmarkProductDomainV1         = "TASKGATE-FINAL-V5-BENCHMARK-PRODUCT-V1\x00"
)

// DatasetField binds one ordered Product field to the stable-key and
// collation metadata that PostgreSQL, the Catalog, and the Oracle must share.
type DatasetField struct {
	Name             string  `json:"name"`
	SQLType          SQLType `json:"sql_type"`
	StableKey        bool    `json:"stable_key"`
	CollationName    string  `json:"collation_name,omitempty"`
	CollationVersion string  `json:"collation_version,omitempty"`
}

type DatasetProductFingerprint struct {
	ProductID       string `json:"product_id"`
	SourceNamespace string `json:"source_namespace"`
	Snapshot        string `json:"snapshot"`
	RowCount        int64  `json:"row_count"`
	SHA256          string `json:"sha256"`
}

// DatasetProductSpec is the detached schema/binding supplied to a read-only
// PostgreSQL adapter. Rows must be yielded in stable-key order.
type DatasetProductSpec struct {
	ProductID       string         `json:"product_id"`
	SourceNamespace string         `json:"source_namespace"`
	Snapshot        string         `json:"snapshot"`
	Fields          []DatasetField `json:"fields"`
	RowCount        int64          `json:"row_count"`
}

// DatasetRowStream is the transport-independent live-row boundary. A future
// adapter may back it with PostgreSQL without importing production semantics
// into this package.
type DatasetRowStream func(yield func([]any) error) error

// DatasetFingerprintSummary contains no dataset rows. PeakBufferedRows is a
// testable statement that the complete 815,000-row corpus was streamed.
type DatasetFingerprintSummary struct {
	DatasetSpecID    string                      `json:"dataset_spec_id"`
	GeneratorVersion string                      `json:"generator_version"`
	Seed             int64                       `json:"seed"`
	ProductCount     int                         `json:"product_count"`
	RowCount         int64                       `json:"row_count"`
	SHA256           string                      `json:"sha256"`
	PeakBufferedRows int                         `json:"peak_buffered_rows"`
	Products         []DatasetProductFingerprint `json:"products"`
}

type benchmarkDatasetProduct struct {
	productID       string
	sourceNamespace string
	snapshot        string
	fields          []DatasetField
	rowCount        int64
	row             func(int64) ([]any, error)
}

// BenchmarkDatasetFingerprint independently regenerates the logical values
// described by the source-controlled Dataset Spec. It never connects to
// PostgreSQL and never retains more than the current row.
func BenchmarkDatasetFingerprint() (DatasetFingerprintSummary, error) {
	return fingerprintBenchmarkDataset(benchmarkDatasetProducts(), BenchmarkDatasetGeneratorSeed, nil)
}

// BenchmarkDatasetProductSpecs returns the exact ordered schema required from
// live PostgreSQL before a Dataset Binding can be reviewed.
func BenchmarkDatasetProductSpecs() []DatasetProductSpec {
	products := benchmarkDatasetProducts()
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

// BenchmarkDatasetFingerprintFromStreams hashes five live, stable-key-ordered
// Product streams with exactly the same encoding as formula regeneration.
// It rejects missing, unknown, truncated, or overlong Product streams.
func BenchmarkDatasetFingerprintFromStreams(streams map[string]DatasetRowStream) (DatasetFingerprintSummary, error) {
	return fingerprintBenchmarkDatasetWithStreams(
		benchmarkDatasetProducts(), BenchmarkDatasetGeneratorSeed, streams, nil,
	)
}

func fingerprintBenchmarkDataset(products []benchmarkDatasetProduct, seed int64, mutate func(string, int64, []any)) (DatasetFingerprintSummary, error) {
	return fingerprintBenchmarkDatasetWithStreams(products, seed, nil, mutate)
}

func fingerprintBenchmarkDatasetWithStreams(
	products []benchmarkDatasetProduct,
	seed int64,
	streams map[string]DatasetRowStream,
	mutate func(string, int64, []any),
) (DatasetFingerprintSummary, error) {
	if len(products) == 0 {
		return DatasetFingerprintSummary{}, errors.New("benchmark dataset has no products")
	}
	if streams != nil && len(streams) != len(products) {
		return DatasetFingerprintSummary{}, errors.New("live benchmark dataset streams do not cover the exact Product set")
	}
	datasetHash := sha256.New()
	_, _ = datasetHash.Write([]byte(benchmarkDatasetDomainV1))
	writeFramed(datasetHash, []byte(BenchmarkDatasetSpecID))
	writeFramed(datasetHash, []byte(BenchmarkDatasetGeneratorVersion))
	writeUint64(datasetHash, uint64(seed))
	writeUint64(datasetHash, uint64(len(products)))

	summary := DatasetFingerprintSummary{
		DatasetSpecID: BenchmarkDatasetSpecID, GeneratorVersion: BenchmarkDatasetGeneratorVersion,
		Seed: seed, ProductCount: len(products), PeakBufferedRows: 1,
		Products: make([]DatasetProductFingerprint, 0, len(products)),
	}
	seenProducts := make(map[string]bool, len(products))
	for _, product := range products {
		if product.productID == "" || product.sourceNamespace == "" || product.snapshot == "" ||
			seenProducts[product.productID] || product.rowCount < 0 || len(product.fields) == 0 || product.row == nil {
			return DatasetFingerprintSummary{}, fmt.Errorf("benchmark Product %q has an invalid specification", product.productID)
		}
		seenProducts[product.productID] = true
		productHash := sha256.New()
		_, _ = productHash.Write([]byte(benchmarkProductDomainV1))
		if err := writeDatasetProductHeader(productHash, product); err != nil {
			return DatasetFingerprintSummary{}, fmt.Errorf("benchmark Product %q: %w", product.productID, err)
		}
		rowIndex := int64(0)
		writeRow := func(row []any) error {
			if rowIndex >= product.rowCount {
				return fmt.Errorf("benchmark Product %q yielded more than %d rows", product.productID, product.rowCount)
			}
			if len(row) != len(product.fields) {
				return fmt.Errorf("benchmark Product %q row %d has %d fields; expected %d",
					product.productID, rowIndex+1, len(row), len(product.fields))
			}
			if mutate != nil {
				mutate(product.productID, rowIndex, row)
			}
			if err := writeDatasetRow(productHash, product.fields, row); err != nil {
				return fmt.Errorf("benchmark Product %q row %d: %w", product.productID, rowIndex+1, err)
			}
			rowIndex++
			return nil
		}
		if streams == nil {
			for rowIndex < product.rowCount {
				row, rowErr := product.row(rowIndex)
				if rowErr != nil {
					return DatasetFingerprintSummary{}, fmt.Errorf("benchmark Product %q row %d: %w", product.productID, rowIndex+1, rowErr)
				}
				if err := writeRow(row); err != nil {
					return DatasetFingerprintSummary{}, err
				}
			}
		} else {
			stream, present := streams[product.productID]
			if !present || stream == nil {
				return DatasetFingerprintSummary{}, fmt.Errorf("benchmark Product %q has no live row stream", product.productID)
			}
			if err := stream(writeRow); err != nil {
				return DatasetFingerprintSummary{}, fmt.Errorf("benchmark Product %q live row stream: %w", product.productID, err)
			}
		}
		if rowIndex != product.rowCount {
			return DatasetFingerprintSummary{}, fmt.Errorf("benchmark Product %q yielded %d rows; expected %d",
				product.productID, rowIndex, product.rowCount)
		}
		productDigest := productHash.Sum(nil)
		writeFramed(datasetHash, productDigest)
		summary.RowCount += product.rowCount
		summary.Products = append(summary.Products, DatasetProductFingerprint{
			ProductID: product.productID, SourceNamespace: product.sourceNamespace,
			Snapshot: product.snapshot, RowCount: product.rowCount, SHA256: hex.EncodeToString(productDigest),
		})
	}
	if streams != nil {
		for productID := range streams {
			if !seenProducts[productID] {
				return DatasetFingerprintSummary{}, fmt.Errorf("unknown live benchmark Product stream %q", productID)
			}
		}
	}
	summary.SHA256 = hex.EncodeToString(datasetHash.Sum(nil))
	return summary, nil
}

func writeDatasetProductHeader(target hash.Hash, product benchmarkDatasetProduct) error {
	writeFramed(target, []byte(product.productID))
	writeFramed(target, []byte(product.sourceNamespace))
	writeFramed(target, []byte(product.snapshot))
	writeUint64(target, uint64(product.rowCount))
	writeUint64(target, uint64(len(product.fields)))
	stableKeys := 0
	seenFields := make(map[string]bool, len(product.fields))
	for _, field := range product.fields {
		if field.Name == "" || seenFields[field.Name] {
			return errors.New("dataset field name is empty or duplicated")
		}
		seenFields[field.Name] = true
		if _, err := NormalizeSQLType(string(field.SQLType)); err != nil {
			return err
		}
		collatable := field.SQLType == SQLText
		if collatable != (field.CollationName != "" && field.CollationVersion != "") {
			return fmt.Errorf("dataset field %q has incomplete or inapplicable collation metadata", field.Name)
		}
		if field.StableKey {
			stableKeys++
		}
		writeFramed(target, []byte(field.Name))
		writeFramed(target, []byte(field.SQLType))
		if field.StableKey {
			_, _ = target.Write([]byte{1})
		} else {
			_, _ = target.Write([]byte{0})
		}
		writeFramed(target, []byte(field.CollationName))
		writeFramed(target, []byte(field.CollationVersion))
	}
	if stableKeys == 0 {
		return errors.New("benchmark Product has no stable key")
	}
	return nil
}

func writeDatasetRow(target hash.Hash, fields []DatasetField, row []any) error {
	writeUint64(target, uint64(len(row)))
	for index, raw := range row {
		value, err := NormalizeTypedValue(fields[index].SQLType, raw)
		if err != nil {
			return fmt.Errorf("field %q: %w", fields[index].Name, err)
		}
		writeFramed(target, []byte(value.SQLType()))
		if value.IsNull() {
			_, _ = target.Write([]byte{0})
			writeFramed(target, nil)
		} else {
			_, _ = target.Write([]byte{1})
			writeFramed(target, value.CanonicalBytes())
		}
	}
	return nil
}

func benchmarkDatasetProducts() []benchmarkDatasetProduct {
	return []benchmarkDatasetProduct{
		{
			productID: "provsql_orders", sourceNamespace: "final_v5.provsql_orders",
			snapshot: "final-v5-provsql-2026-v1", rowCount: 50_000,
			fields: []DatasetField{
				{Name: "orderkey", SQLType: SQLBigInt, StableKey: true},
				{Name: "status", SQLType: SQLBigInt},
				{Name: "partition_key", SQLType: SQLInteger},
			},
			row: func(index int64) ([]any, error) {
				key := index + 1
				return []any{key, key % 3, int32(1)}, nil
			},
		},
		{
			productID: "provsql_lineitem", sourceNamespace: "final_v5.provsql_lineitem",
			snapshot: "final-v5-provsql-2026-v1", rowCount: 250_000,
			fields: []DatasetField{
				{Name: "orderkey", SQLType: SQLBigInt, StableKey: true},
				{Name: "linenumber", SQLType: SQLInteger, StableKey: true},
				{Name: "extendedprice", SQLType: SQLNumeric},
				{Name: "partition_key", SQLType: SQLInteger},
			},
			row: func(index int64) ([]any, error) {
				orderKey, lineNumber := index/5+1, index%5+1
				cents := ((orderKey*13)+(lineNumber*7))%100_000 + 100
				return []any{orderKey, int32(lineNumber), artifactDecimal(cents, 2), int32(1)}, nil
			},
		},
		{
			productID: "provsql_nonce", sourceNamespace: "final_v5.provsql_nonce",
			snapshot: "final-v5-provsql-2026-v1", rowCount: 1_000,
			fields: []DatasetField{
				{Name: "nonce_id", SQLType: SQLBigInt, StableKey: true},
				{Name: "partition_key", SQLType: SQLInteger},
			},
			row: func(index int64) ([]any, error) { return []any{index + 1, int32(1)}, nil },
		},
		{
			productID: ExposureScaleProductID, sourceNamespace: ExposureScaleSourceNamespace,
			snapshot: ExposureScaleSnapshot, rowCount: ExposureScaleMaximumDatasetFacts / ExposureScaleFactsPerRow,
			fields: []DatasetField{
				{Name: "member_rank", SQLType: SQLBigInt, StableKey: true},
				{Name: "metric", SQLType: SQLNumeric},
				{Name: "family_id", SQLType: SQLInteger},
				{Name: "partition_key", SQLType: SQLInteger},
			},
			row: exposureScaleDatasetRow,
		},
		{
			productID: "final_v5_result_heavy", sourceNamespace: "final_v5.result_heavy",
			snapshot: "final-v5-result-heavy-2026-v1", rowCount: 100_000,
			fields: resultHeavyDatasetFields(),
			row:    func(index int64) ([]any, error) { return ArtifactRow(index, 16) },
		},
	}
}

// exposureScaleDatasetRow is the sole evaluation-side implementation of the
// frozen Product formula. Both typed PostgreSQL agreement and independent Fact
// generation consume these values, so formula drift cannot split the evidence
// chain into two self-consistent models.
func exposureScaleDatasetRow(index int64) ([]any, error) {
	rowCount := ExposureScaleMaximumDatasetFacts / ExposureScaleFactsPerRow
	if index < 0 || index >= rowCount {
		return nil, errors.New("exposure-scale row is outside the source-controlled dataset")
	}
	rank := index + 1
	cents := (rank*13)%100_000 + 100
	return []any{rank, artifactDecimal(cents, 2), int32(1), int32(1)}, nil
}

func resultHeavyDatasetFields() []DatasetField {
	result := make([]DatasetField, len(artifactColumnsV1))
	for index, column := range artifactColumnsV1 {
		result[index] = DatasetField{Name: column.Name, SQLType: column.Type, StableKey: index == 0}
		if column.Type == SQLText {
			result[index].CollationName = "en_US.utf8"
			result[index].CollationVersion = "2.36"
		}
	}
	return result
}
