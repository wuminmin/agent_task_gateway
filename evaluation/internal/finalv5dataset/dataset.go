// Package finalv5dataset is the single PostgreSQL transport for the reviewed
// Final-V5 benchmark Dataset. It owns only five fixed, read-only Product
// queries and their physical PostgreSQL shape. Logical value normalization and
// Dataset hashing remain exclusively in finalv5oracle.
package finalv5dataset

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
)

const (
	// BenchmarkAgreementVersion identifies a credential-free comparison of the
	// complete reviewed formula with one live PostgreSQL snapshot.
	BenchmarkAgreementVersion = "taskgate-final-v5-benchmark-dataset-agreement-v1"

	benchmarkDatasetRowCount = int64(815_000)
	benchmarkDatasetSHA256   = "f90239bb32ef9542089ca8f1bd7c30c7870cbe627e835698364bdb9b4dc15978"

	resultHeavyProductID = "final_v5_result_heavy"

	provSQLOrdersQuery = `SELECT orderkey, status, partition_key
FROM reporting.provsql_orders
ORDER BY orderkey`
	provSQLLineitemQuery = `SELECT orderkey, linenumber, extendedprice, partition_key
FROM reporting.provsql_lineitem
ORDER BY orderkey, linenumber`
	provSQLNonceQuery = `SELECT nonce_id, partition_key
FROM reporting.provsql_nonce
ORDER BY nonce_id`
	exposureScaleQuery = `SELECT member_rank, metric, family_id, partition_key
FROM reporting.final_v5_exposure_scale
ORDER BY member_rank`
	resultHeavyQuery = `SELECT row_id, category, amount, event_date, sequence_no, approved,
       event_timestamp, description, quantity, unit_price, tax_amount,
       settled_date, processed_at, region, revision, active
FROM reporting.final_v5_result_heavy
ORDER BY row_id`

	// relationMetadataQuery binds the physical reporting relations to the
	// schema, type and collation metadata which the logical Dataset header
	// commits. The five data queries alone cannot observe a text column's
	// collation catalog/version.
	relationMetadataQuery = `SELECT n.nspname || '.' || c.relname AS relation_name,
       c.relkind::text AS relation_kind,
       a.attnum::integer AS attribute_number,
       a.attname::text AS column_name,
       a.atttypid::oid AS type_oid,
       COALESCE(coll.collname::text, '') AS collation_name,
       COALESCE(coll.collversion, '') AS collation_version,
       COALESCE(pg_collation_actual_version(coll.oid), '') AS collation_actual_version
FROM pg_catalog.pg_class AS c
INNER JOIN pg_catalog.pg_namespace AS n ON n.oid = c.relnamespace
INNER JOIN pg_catalog.pg_attribute AS a ON a.attrelid = c.oid
LEFT JOIN pg_catalog.pg_collation AS coll ON coll.oid = a.attcollation
WHERE n.nspname = 'reporting'
  AND c.relname IN ('provsql_orders', 'provsql_lineitem', 'provsql_nonce',
                    'final_v5_exposure_scale', 'final_v5_result_heavy')
  AND a.attnum > 0
  AND NOT a.attisdropped
ORDER BY CASE c.relname
           WHEN 'provsql_orders' THEN 1
           WHEN 'provsql_lineitem' THEN 2
           WHEN 'provsql_nonce' THEN 3
           WHEN 'final_v5_exposure_scale' THEN 4
           WHEN 'final_v5_result_heavy' THEN 5
           ELSE 6
         END,
         a.attnum`

	preparedStatementCountQuery = `SELECT count(*) FROM pg_prepared_statements`
)

// PostgreSQLColumn is the complete physical column shape admitted before any
// live value is yielded to the independent Dataset normalizer.
type PostgreSQLColumn struct {
	Name                   string                `json:"name"`
	PostgreSQLOID          uint32                `json:"postgresql_oid"`
	SQLType                finalv5oracle.SQLType `json:"sql_type"`
	StableKey              bool                  `json:"stable_key"`
	CollationName          string                `json:"collation_name,omitempty"`
	CollationVersion       string                `json:"collation_version,omitempty"`
	CollationActualVersion string                `json:"collation_actual_version,omitempty"`
}

// PostgreSQLProduct is the credential-free live relation/schema evidence for
// one reviewed Product. All five are materialized reporting relations.
type PostgreSQLProduct struct {
	ProductID    string             `json:"product_id"`
	Relation     string             `json:"relation"`
	RelationKind string             `json:"relation_kind"`
	Columns      []PostgreSQLColumn `json:"columns"`
}

// ProductDefinition adds the one fixed stable-key-ordered query used to obtain
// a PostgreSQLProduct's logical values. Query is intentionally excluded from
// retained agreement JSON; its source identity is the source-built binary.
type ProductDefinition struct {
	PostgreSQLProduct
	Query string `json:"-"`
}

// BenchmarkAgreement contains no rows or connection material. Reference is
// regenerated from the reviewed formula; Observed is streamed from exactly one
// repeatable-read PostgreSQL snapshot through the same normalizer.
type BenchmarkAgreement struct {
	Version                string                                  `json:"version"`
	Products               []PostgreSQLProduct                     `json:"products"`
	Reference              finalv5oracle.DatasetFingerprintSummary `json:"reference"`
	Observed               finalv5oracle.DatasetFingerprintSummary `json:"observed"`
	PreparedStatementCount int64                                   `json:"prepared_statement_count"`
	Agreed                 bool                                    `json:"agreed"`
}

var productDefinitions = []ProductDefinition{
	{
		PostgreSQLProduct: PostgreSQLProduct{
			ProductID: finalv5oracle.ProvSQLOrdersProductID,
			Relation:  "reporting.provsql_orders", RelationKind: "m",
			Columns: []PostgreSQLColumn{
				{Name: "orderkey", PostgreSQLOID: 20, SQLType: finalv5oracle.SQLBigInt, StableKey: true},
				{Name: "status", PostgreSQLOID: 20, SQLType: finalv5oracle.SQLBigInt},
				{Name: "partition_key", PostgreSQLOID: 23, SQLType: finalv5oracle.SQLInteger},
			},
		},
		Query: provSQLOrdersQuery,
	},
	{
		PostgreSQLProduct: PostgreSQLProduct{
			ProductID: finalv5oracle.ProvSQLLineitemProductID,
			Relation:  "reporting.provsql_lineitem", RelationKind: "m",
			Columns: []PostgreSQLColumn{
				{Name: "orderkey", PostgreSQLOID: 20, SQLType: finalv5oracle.SQLBigInt, StableKey: true},
				{Name: "linenumber", PostgreSQLOID: 23, SQLType: finalv5oracle.SQLInteger, StableKey: true},
				{Name: "extendedprice", PostgreSQLOID: 1700, SQLType: finalv5oracle.SQLNumeric},
				{Name: "partition_key", PostgreSQLOID: 23, SQLType: finalv5oracle.SQLInteger},
			},
		},
		Query: provSQLLineitemQuery,
	},
	{
		PostgreSQLProduct: PostgreSQLProduct{
			ProductID: finalv5oracle.ProvSQLNonceProductID,
			Relation:  "reporting.provsql_nonce", RelationKind: "m",
			Columns: []PostgreSQLColumn{
				{Name: "nonce_id", PostgreSQLOID: 20, SQLType: finalv5oracle.SQLBigInt, StableKey: true},
				{Name: "partition_key", PostgreSQLOID: 23, SQLType: finalv5oracle.SQLInteger},
			},
		},
		Query: provSQLNonceQuery,
	},
	{
		PostgreSQLProduct: PostgreSQLProduct{
			ProductID: finalv5oracle.ExposureScaleProductID,
			Relation:  "reporting.final_v5_exposure_scale", RelationKind: "m",
			Columns: []PostgreSQLColumn{
				{Name: "member_rank", PostgreSQLOID: 20, SQLType: finalv5oracle.SQLBigInt, StableKey: true},
				{Name: "metric", PostgreSQLOID: 1700, SQLType: finalv5oracle.SQLNumeric},
				{Name: "family_id", PostgreSQLOID: 23, SQLType: finalv5oracle.SQLInteger},
				{Name: "partition_key", PostgreSQLOID: 23, SQLType: finalv5oracle.SQLInteger},
			},
		},
		Query: exposureScaleQuery,
	},
	{
		PostgreSQLProduct: PostgreSQLProduct{
			ProductID: resultHeavyProductID,
			Relation:  "reporting.final_v5_result_heavy", RelationKind: "m",
			Columns: []PostgreSQLColumn{
				{Name: "row_id", PostgreSQLOID: 20, SQLType: finalv5oracle.SQLBigInt, StableKey: true},
				{Name: "category", PostgreSQLOID: 25, SQLType: finalv5oracle.SQLText,
					CollationName: "en_US.utf8", CollationVersion: "2.36", CollationActualVersion: "2.36"},
				{Name: "amount", PostgreSQLOID: 1700, SQLType: finalv5oracle.SQLNumeric},
				{Name: "event_date", PostgreSQLOID: 1082, SQLType: finalv5oracle.SQLDate},
				{Name: "sequence_no", PostgreSQLOID: 23, SQLType: finalv5oracle.SQLInteger},
				{Name: "approved", PostgreSQLOID: 16, SQLType: finalv5oracle.SQLBoolean},
				{Name: "event_timestamp", PostgreSQLOID: 1114, SQLType: finalv5oracle.SQLTimestampWithoutTZ},
				{Name: "description", PostgreSQLOID: 25, SQLType: finalv5oracle.SQLText,
					CollationName: "en_US.utf8", CollationVersion: "2.36", CollationActualVersion: "2.36"},
				{Name: "quantity", PostgreSQLOID: 20, SQLType: finalv5oracle.SQLBigInt},
				{Name: "unit_price", PostgreSQLOID: 1700, SQLType: finalv5oracle.SQLNumeric},
				{Name: "tax_amount", PostgreSQLOID: 1700, SQLType: finalv5oracle.SQLNumeric},
				{Name: "settled_date", PostgreSQLOID: 1082, SQLType: finalv5oracle.SQLDate},
				{Name: "processed_at", PostgreSQLOID: 1114, SQLType: finalv5oracle.SQLTimestampWithoutTZ},
				{Name: "region", PostgreSQLOID: 25, SQLType: finalv5oracle.SQLText,
					CollationName: "en_US.utf8", CollationVersion: "2.36", CollationActualVersion: "2.36"},
				{Name: "revision", PostgreSQLOID: 23, SQLType: finalv5oracle.SQLInteger},
				{Name: "active", PostgreSQLOID: 16, SQLType: finalv5oracle.SQLBoolean},
			},
		},
		Query: resultHeavyQuery,
	},
}

// ProductDefinitions returns detached copies of the exact five Product
// transports. It exists so the already-certified C1/D1 subset adapters and
// static boundary tests consume this same mapping rather than copy SQL/OIDs.
func ProductDefinitions() ([]ProductDefinition, error) {
	if err := validateProductDefinitions(); err != nil {
		return nil, err
	}
	result := make([]ProductDefinition, len(productDefinitions))
	for index, definition := range productDefinitions {
		result[index] = cloneProductDefinition(definition)
	}
	return result, nil
}

// ProductDefinitionFor returns one detached fixed Product transport.
func ProductDefinitionFor(productID string) (ProductDefinition, error) {
	definitions, err := ProductDefinitions()
	if err != nil {
		return ProductDefinition{}, err
	}
	for _, definition := range definitions {
		if definition.ProductID == productID {
			return definition, nil
		}
	}
	return ProductDefinition{}, fmt.Errorf("Product %q is outside the fixed Final-V5 benchmark Dataset", productID)
}

// DatasetStreamColumns returns the detached name/OID/type projection consumed
// by the existing C1 and D1 typed Product agreement records.
func DatasetStreamColumns(productID string) ([]finalv5oracle.DatasetStreamColumn, error) {
	definition, err := ProductDefinitionFor(productID)
	if err != nil {
		return nil, err
	}
	columns := make([]finalv5oracle.DatasetStreamColumn, len(definition.Columns))
	for index, column := range definition.Columns {
		columns[index] = finalv5oracle.DatasetStreamColumn{
			Name: column.Name, PostgreSQLOID: column.PostgreSQLOID, SQLType: column.SQLType,
		}
	}
	return columns, nil
}

// ProductStream returns a lazy, stable-key-ordered typed stream for exactly one
// fixed Product. Callers cannot supply SQL. The live field descriptions are
// rejected before the first value reaches finalv5oracle.
func ProductStream(ctx context.Context, tx pgx.Tx, productID string) (finalv5oracle.DatasetRowStream, error) {
	if ctx == nil || tx == nil {
		return nil, errors.New("fixed benchmark Dataset stream requires a context and read-only transaction")
	}
	definition, err := ProductDefinitionFor(productID)
	if err != nil {
		return nil, err
	}
	return func(yield func([]any) error) error {
		if yield == nil {
			return errors.New("fixed benchmark Dataset row callback is nil")
		}
		rows, queryErr := tx.Query(ctx, definition.Query, pgx.QueryExecModeSimpleProtocol)
		if queryErr != nil {
			return fmt.Errorf("execute fixed %s Dataset query failed", definition.ProductID)
		}
		defer rows.Close()
		if err := validateFieldDescriptions(rows, definition); err != nil {
			return err
		}
		for rows.Next() {
			values, valuesErr := rows.Values()
			if valuesErr != nil {
				return fmt.Errorf("read fixed %s Dataset row failed", definition.ProductID)
			}
			if err := yield(values); err != nil {
				return err
			}
		}
		if rows.Err() != nil {
			return fmt.Errorf("drain fixed %s Dataset rows failed", definition.ProductID)
		}
		return nil
	}, nil
}

// VerifyBenchmarkPostgreSQL compares all five live Products with the complete
// reviewed Dataset in one read-only repeatable-read transaction.
func VerifyBenchmarkPostgreSQL(ctx context.Context, dsn string) (BenchmarkAgreement, error) {
	agreement := BenchmarkAgreement{Version: BenchmarkAgreementVersion}
	if ctx == nil {
		return agreement, errors.New("fixed benchmark Dataset verification requires a context")
	}
	if strings.TrimSpace(dsn) == "" {
		return agreement, errors.New("fixed benchmark Dataset PostgreSQL DSN is required")
	}
	definitions, err := ProductDefinitions()
	if err != nil {
		return agreement, err
	}
	reference, err := finalv5oracle.BenchmarkDatasetFingerprint()
	if err != nil {
		return agreement, fmt.Errorf("regenerate reviewed benchmark Dataset: %w", err)
	}
	agreement.Reference = reference

	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return agreement, errors.New("connect fixed benchmark Dataset database failed")
	}
	defer connection.Close(context.Background())
	tx, err := connection.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return agreement, errors.New("begin read-only benchmark Dataset transaction failed")
	}
	defer tx.Rollback(context.Background())

	products, err := readAndValidateRelationMetadata(ctx, tx, definitions)
	if err != nil {
		return agreement, err
	}
	agreement.Products = products
	streams := make(map[string]finalv5oracle.DatasetRowStream, len(definitions))
	for _, definition := range definitions {
		stream, streamErr := ProductStream(ctx, tx, definition.ProductID)
		if streamErr != nil {
			return agreement, streamErr
		}
		streams[definition.ProductID] = stream
	}
	observed, err := finalv5oracle.BenchmarkDatasetFingerprintFromStreams(streams)
	if err != nil {
		return agreement, fmt.Errorf("fingerprint fixed live benchmark Dataset: %w", err)
	}
	agreement.Observed = observed
	agreement.Agreed = reflect.DeepEqual(agreement.Reference, agreement.Observed)
	agreement.PreparedStatementCount, err = PreparedStatementCount(ctx, tx)
	if err != nil {
		return agreement, errors.New("verify benchmark Dataset prepared-statement state failed")
	}
	if err := ValidateBenchmarkAgreement(agreement); err != nil {
		return agreement, err
	}
	if err := tx.Commit(ctx); err != nil {
		return agreement, errors.New("commit read-only benchmark Dataset transaction failed")
	}
	return agreement, nil
}

// ValidateBenchmarkAgreement is the single pure acceptance rule for a full
// five-Product Dataset probe, whether obtained from PostgreSQL or injected into
// a fresh-deployment proof. It performs no I/O and does not mutate agreement.
func ValidateBenchmarkAgreement(agreement BenchmarkAgreement) error {
	if agreement.Version != BenchmarkAgreementVersion {
		return fmt.Errorf("benchmark Dataset agreement version is %q; expected %q",
			agreement.Version, BenchmarkAgreementVersion)
	}
	definitions, err := ProductDefinitions()
	if err != nil {
		return err
	}
	wantProducts := make([]PostgreSQLProduct, len(definitions))
	for index, definition := range definitions {
		wantProducts[index] = definition.PostgreSQLProduct
	}
	if !reflect.DeepEqual(agreement.Products, wantProducts) {
		return errors.New("benchmark Dataset agreement Products differ from the exact five reviewed PostgreSQL shapes")
	}
	wantSummary, err := finalv5oracle.BenchmarkDatasetFingerprint()
	if err != nil {
		return fmt.Errorf("regenerate reviewed benchmark Dataset: %w", err)
	}
	if wantSummary.ProductCount != 5 || wantSummary.RowCount != benchmarkDatasetRowCount ||
		wantSummary.PeakBufferedRows != 1 || len(wantSummary.Products) != 5 ||
		wantSummary.SHA256 != benchmarkDatasetSHA256 {
		return errors.New("reviewed benchmark Dataset formula differs from the fixed five-Product identity")
	}
	if !reflect.DeepEqual(agreement.Reference, wantSummary) {
		return errors.New("benchmark Dataset agreement reference differs from the complete reviewed formula")
	}
	if !reflect.DeepEqual(agreement.Observed, wantSummary) {
		return errors.New("benchmark Dataset agreement probe differs from the complete reviewed formula")
	}
	if !agreement.Agreed {
		return errors.New("benchmark Dataset agreement is not affirmed")
	}
	if agreement.PreparedStatementCount != 0 {
		return fmt.Errorf("benchmark Dataset session contains %d prepared statements; expected zero",
			agreement.PreparedStatementCount)
	}
	return nil
}

// PreparedStatementCount is the one session-state query shared by the full
// Dataset verifier and the pre-existing C1/D1 subset agreements.
func PreparedStatementCount(ctx context.Context, tx pgx.Tx) (int64, error) {
	if ctx == nil || tx == nil {
		return 0, errors.New("prepared-statement verification requires a context and transaction")
	}
	var count int64
	if err := tx.QueryRow(ctx, preparedStatementCountQuery, pgx.QueryExecModeSimpleProtocol).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func validateProductDefinitions() error {
	specs := finalv5oracle.BenchmarkDatasetProductSpecs()
	if len(specs) != 5 || len(productDefinitions) != len(specs) {
		return fmt.Errorf("benchmark Dataset has %d logical Products and %d PostgreSQL transports; expected five each",
			len(specs), len(productDefinitions))
	}
	seen := make(map[string]bool, len(specs))
	for productIndex := range specs {
		spec, definition := specs[productIndex], productDefinitions[productIndex]
		if spec.ProductID == "" || seen[spec.ProductID] || definition.ProductID != spec.ProductID ||
			definition.Relation != "reporting."+spec.ProductID || definition.RelationKind != "m" ||
			strings.TrimSpace(definition.Query) == "" || len(definition.Columns) != len(spec.Fields) {
			return fmt.Errorf("benchmark Dataset Product %d has a mismatched PostgreSQL transport", productIndex+1)
		}
		seen[spec.ProductID] = true
		for columnIndex := range spec.Fields {
			field, column := spec.Fields[columnIndex], definition.Columns[columnIndex]
			resolved, err := finalv5oracle.SQLTypeFromPostgresOID(column.PostgreSQLOID)
			if err != nil || column.Name != field.Name || column.SQLType != field.SQLType || resolved != field.SQLType ||
				column.StableKey != field.StableKey || column.CollationName != field.CollationName ||
				column.CollationVersion != field.CollationVersion ||
				column.CollationActualVersion != field.CollationVersion {
				return fmt.Errorf("benchmark Dataset Product %s column %d has a mismatched PostgreSQL shape",
					spec.ProductID, columnIndex+1)
			}
		}
	}
	return nil
}

func validateFieldDescriptions(rows pgx.Rows, definition ProductDefinition) error {
	fields := rows.FieldDescriptions()
	if len(fields) != len(definition.Columns) {
		return fmt.Errorf("fixed %s Dataset stream has %d columns; expected %d",
			definition.ProductID, len(fields), len(definition.Columns))
	}
	for index, field := range fields {
		want := definition.Columns[index]
		resolved, err := finalv5oracle.SQLTypeFromPostgresOID(field.DataTypeOID)
		if err != nil || string(field.Name) != want.Name || field.DataTypeOID != want.PostgreSQLOID ||
			resolved != want.SQLType {
			return fmt.Errorf("fixed %s Dataset column %d differs from the reviewed name/OID/type",
				definition.ProductID, index+1)
		}
	}
	return nil
}

func readAndValidateRelationMetadata(ctx context.Context, tx pgx.Tx,
	definitions []ProductDefinition) ([]PostgreSQLProduct, error) {
	rows, err := tx.Query(ctx, relationMetadataQuery, pgx.QueryExecModeSimpleProtocol)
	if err != nil {
		return nil, errors.New("read fixed benchmark Dataset relation metadata failed")
	}
	defer rows.Close()
	type metadataColumn struct {
		relation, kind             string
		attributeNumber            int32
		name                       string
		oid                        uint32
		collation, version, actual string
	}
	var observed []metadataColumn
	for rows.Next() {
		var column metadataColumn
		if err := rows.Scan(&column.relation, &column.kind, &column.attributeNumber, &column.name,
			&column.oid, &column.collation, &column.version, &column.actual); err != nil {
			return nil, errors.New("decode fixed benchmark Dataset relation metadata failed")
		}
		observed = append(observed, column)
	}
	if rows.Err() != nil {
		return nil, errors.New("drain fixed benchmark Dataset relation metadata failed")
	}

	wantCount := 0
	for _, definition := range definitions {
		wantCount += len(definition.Columns)
	}
	if len(observed) != wantCount {
		return nil, fmt.Errorf("benchmark Dataset relation metadata has %d columns; expected %d", len(observed), wantCount)
	}
	products := make([]PostgreSQLProduct, len(definitions))
	position := 0
	for productIndex, definition := range definitions {
		product := PostgreSQLProduct{
			ProductID: definition.ProductID, Relation: definition.Relation,
			RelationKind: definition.RelationKind, Columns: make([]PostgreSQLColumn, len(definition.Columns)),
		}
		for columnIndex, want := range definition.Columns {
			got := observed[position]
			position++
			if got.relation != definition.Relation || got.kind != definition.RelationKind ||
				got.attributeNumber != int32(columnIndex+1) || got.name != want.Name || got.oid != want.PostgreSQLOID ||
				got.collation != want.CollationName || got.version != want.CollationVersion ||
				got.actual != want.CollationActualVersion {
				return nil, fmt.Errorf("benchmark Dataset relation %s column %d metadata differs from the reviewed shape",
					definition.Relation, columnIndex+1)
			}
			product.Columns[columnIndex] = want
		}
		products[productIndex] = product
	}
	return products, nil
}

func cloneProductDefinition(input ProductDefinition) ProductDefinition {
	result := input
	result.Columns = append([]PostgreSQLColumn(nil), input.Columns...)
	return result
}
