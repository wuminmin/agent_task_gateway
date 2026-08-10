package finalv5oracle

import (
	"errors"
	"fmt"
)

const ExposureScaleDatasetAgreementVersion = "taskgate-final-v5-exposure-scale-dataset-agreement-v1"

// DatasetStreamColumn is the typed PostgreSQL boundary supplied by an adapter
// before any row bytes are admitted to the independent dataset fingerprint.
type DatasetStreamColumn struct {
	Name          string  `json:"name"`
	PostgreSQLOID uint32  `json:"postgresql_oid"`
	SQLType       SQLType `json:"sql_type"`
}

// ExposureScaleDatasetAgreement is credential-free evidence that the frozen
// 414,000-row formula and a live, stable-key-ordered PostgreSQL stream have the
// same typed Product bytes. It contains no source rows or connection material.
type ExposureScaleDatasetAgreement struct {
	Version          string                    `json:"version"`
	DatasetSpecID    string                    `json:"dataset_spec_id"`
	GeneratorVersion string                    `json:"generator_version"`
	Seed             int64                     `json:"seed"`
	ProductID        string                    `json:"product_id"`
	Columns          []DatasetStreamColumn     `json:"columns"`
	Reference        DatasetProductFingerprint `json:"reference"`
	Observed         DatasetProductFingerprint `json:"observed"`
	Agreed           bool                      `json:"agreed"`
}

// ExposureScaleDatasetProductSpec returns the detached, fixed typed schema
// for the only Product C1 is authorized to read from PostgreSQL.
func ExposureScaleDatasetProductSpec() DatasetProductSpec {
	product, _ := exposureScaleDatasetProduct()
	return DatasetProductSpec{
		ProductID: product.productID, SourceNamespace: product.sourceNamespace,
		Snapshot: product.snapshot, Fields: append([]DatasetField(nil), product.fields...),
		RowCount: product.rowCount,
	}
}

// ExposureScaleDatasetStreamColumns returns the exact column-name/OID order
// required from the fixed PostgreSQL query. An adapter must reject any other
// shape before yielding values.
func ExposureScaleDatasetStreamColumns() []DatasetStreamColumn {
	return []DatasetStreamColumn{
		{Name: "member_rank", PostgreSQLOID: 20, SQLType: SQLBigInt},
		{Name: "metric", PostgreSQLOID: 1700, SQLType: SQLNumeric},
		{Name: "family_id", PostgreSQLOID: 23, SQLType: SQLInteger},
		{Name: "partition_key", PostgreSQLOID: 23, SQLType: SQLInteger},
	}
}

// AgreeExposureScaleDatasetStream independently regenerates the frozen
// Product, fingerprints a live typed stream through the same normalizer, and
// fails closed unless the two fingerprints agree exactly.
func AgreeExposureScaleDatasetStream(columns []DatasetStreamColumn, stream DatasetRowStream) (ExposureScaleDatasetAgreement, error) {
	agreement := ExposureScaleDatasetAgreement{
		Version: ExposureScaleDatasetAgreementVersion, DatasetSpecID: BenchmarkDatasetSpecID,
		GeneratorVersion: BenchmarkDatasetGeneratorVersion, Seed: BenchmarkDatasetGeneratorSeed,
		ProductID: ExposureScaleProductID, Columns: append([]DatasetStreamColumn(nil), columns...),
	}
	if stream == nil {
		return agreement, errors.New("exposure-scale live Dataset stream is nil")
	}
	wantColumns := ExposureScaleDatasetStreamColumns()
	if len(columns) != len(wantColumns) {
		return agreement, fmt.Errorf("exposure-scale live Dataset has %d columns; expected %d", len(columns), len(wantColumns))
	}
	for index := range columns {
		resolved, err := SQLTypeFromPostgresOID(columns[index].PostgreSQLOID)
		if err != nil {
			return agreement, fmt.Errorf("exposure-scale live Dataset column %d: %w", index+1, err)
		}
		if columns[index].Name != wantColumns[index].Name ||
			columns[index].PostgreSQLOID != wantColumns[index].PostgreSQLOID ||
			columns[index].SQLType != wantColumns[index].SQLType || resolved != wantColumns[index].SQLType {
			return agreement, fmt.Errorf("exposure-scale live Dataset column %d is %+v; expected %+v",
				index+1, columns[index], wantColumns[index])
		}
	}
	product, err := exposureScaleDatasetProduct()
	if err != nil {
		return agreement, err
	}
	reference, err := fingerprintBenchmarkDataset([]benchmarkDatasetProduct{product}, BenchmarkDatasetGeneratorSeed, nil)
	if err != nil {
		return agreement, fmt.Errorf("regenerate exposure-scale Dataset Product: %w", err)
	}
	observed, err := fingerprintBenchmarkDatasetWithStreams(
		[]benchmarkDatasetProduct{product}, BenchmarkDatasetGeneratorSeed,
		map[string]DatasetRowStream{ExposureScaleProductID: stream}, nil,
	)
	if err != nil {
		return agreement, fmt.Errorf("fingerprint exposure-scale live Dataset Product: %w", err)
	}
	agreement.Reference = reference.Products[0]
	agreement.Observed = observed.Products[0]
	agreement.Agreed = agreement.Reference == agreement.Observed
	if !agreement.Agreed {
		return agreement, errors.New("exposure-scale live Dataset Product disagrees with the frozen typed formula")
	}
	return agreement, nil
}

func exposureScaleDatasetProduct() (benchmarkDatasetProduct, error) {
	for _, product := range benchmarkDatasetProducts() {
		if product.productID == ExposureScaleProductID {
			return product, nil
		}
	}
	return benchmarkDatasetProduct{}, errors.New("frozen benchmark Dataset omits exposure-scale Product")
}
