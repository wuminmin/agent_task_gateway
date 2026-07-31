package dataconnector

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestConnectorRegistersTimeTZAsLosslessText(t *testing.T) {
	typeMap := pgtype.NewMap()
	if _, exists := typeMap.TypeForOID(pgtype.TimetzOID); exists {
		t.Fatal("pgx unexpectedly registered a native timetz codec")
	}
	registerConnectorDataTypes(typeMap)
	dataType, exists := typeMap.TypeForOID(pgtype.TimetzOID)
	if !exists || dataType.Name != "timetz" || dataType.Codec.PreferredFormat() != pgx.TextFormatCode {
		t.Fatalf("registered timetz type = %#v, exists=%v", dataType, exists)
	}
	decoded, err := dataType.Codec.DecodeValue(typeMap, pgtype.TimetzOID, pgx.TextFormatCode, []byte("04:05:06.789123-08:30:15"))
	if err != nil {
		t.Fatalf("DecodeValue: %v", err)
	}
	if decoded != "04:05:06.789123-08:30:15" {
		t.Fatalf("decoded timetz = %#v", decoded)
	}
}

func TestNormalizeConfigDefaultsAndRejectsSecretsInErrors(t *testing.T) {
	config, err := normalizeConfig(Config{DSN: "postgres://reader:super-secret@postgres/demo"})
	if err != nil {
		t.Fatalf("normalizeConfig() error = %v", err)
	}
	if config.StatementTimeout != DefaultStatementTimeout || config.MaxRows != DefaultMaxRows || config.MaxConnections != DefaultMaxConnections {
		t.Fatalf("normalizeConfig() defaults = %+v", config)
	}

	_, err = normalizeConfig(Config{DSN: "postgres://reader:super-secret@postgres/demo", MaxRows: -1})
	if !IsCode(err, CodeInvalidConfig) {
		t.Fatalf("normalizeConfig() error = %v", err)
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("safe error leaked DSN: %v", err)
	}
}

func TestCeilingsCanOnlyNarrow(t *testing.T) {
	if got := clampRows(25, 100); got != 25 {
		t.Errorf("clampRows(25, 100) = %d", got)
	}
	if got := clampRows(250, 100); got != 100 {
		t.Errorf("clampRows(250, 100) = %d", got)
	}
	if got := clampRows(0, 100); got != 100 {
		t.Errorf("clampRows(0, 100) = %d", got)
	}

	maximum := 5 * time.Second
	if got := clampTimeout(time.Second, maximum); got != time.Second {
		t.Errorf("clampTimeout(short) = %s", got)
	}
	if got := clampTimeout(30*time.Second, maximum); got != maximum {
		t.Errorf("clampTimeout(long) = %s", got)
	}
	if got := timeoutSetting(500 * time.Microsecond); got != "1ms" {
		t.Errorf("timeoutSetting(sub-ms) = %q", got)
	}
}

func TestSchemaAttestationComparisonIsExact(t *testing.T) {
	expected := []SchemaColumn{{Name: "month", PostgreSQLType: "text"}, {Name: "total_amount", PostgreSQLType: "numeric"}}
	if !sameSchemaColumns(expected, []SchemaColumn{{Name: "month", PostgreSQLType: "TEXT"}, {Name: "total_amount", PostgreSQLType: "numeric"}}) {
		t.Fatal("equivalent PostgreSQL type spelling was rejected")
	}
	for name, actual := range map[string][]SchemaColumn{
		"missing":   {{Name: "month", PostgreSQLType: "text"}},
		"reordered": {{Name: "total_amount", PostgreSQLType: "numeric"}, {Name: "month", PostgreSQLType: "text"}},
		"renamed":   {{Name: "month", PostgreSQLType: "text"}, {Name: "amount", PostgreSQLType: "numeric"}},
		"retyped":   {{Name: "month", PostgreSQLType: "text"}, {Name: "total_amount", PostgreSQLType: "double precision"}},
	} {
		t.Run(name, func(t *testing.T) {
			if sameSchemaColumns(expected, actual) {
				t.Fatalf("schema drift %q was accepted", name)
			}
		})
	}
}

func TestSchemaAttestationPinsDeterministicCollationNameAndVersion(t *testing.T) {
	expected := []SchemaColumn{{Name: "label", PostgreSQLType: "text", Collation: "en_US.utf8", CollationVersion: "2.36", CollationDeterministic: true}}
	actual := []SchemaColumn{{Name: "label", PostgreSQLType: "text", Collation: "en_US.utf8", CollationVersion: "2.36", CollationDeterministic: true}}
	if !sameSchemaColumns(expected, actual) {
		t.Fatal("exact deterministic collation was rejected")
	}
	for _, mutate := range []func(*SchemaColumn){
		func(column *SchemaColumn) { column.Collation = "C" },
		func(column *SchemaColumn) { column.CollationVersion = "2.37" },
		func(column *SchemaColumn) { column.CollationDeterministic = false },
	} {
		candidate := append([]SchemaColumn(nil), actual...)
		mutate(&candidate[0])
		if sameSchemaColumns(expected, candidate) {
			t.Fatalf("collation drift was accepted: %+v", candidate[0])
		}
	}
}

func TestSchemaDigestIsDeterministicAndColumnOrderSensitive(t *testing.T) {
	left, err := SchemaDigest([]ViewSchema{
		{Schema: "reporting", View: "expense_summary", Columns: []SchemaColumn{{Name: "month", PostgreSQLType: "text"}, {Name: "total_amount", PostgreSQLType: "numeric"}}},
		{Schema: "reporting", View: "expense_detail", Columns: []SchemaColumn{{Name: "receipt_no", PostgreSQLType: "text"}}},
	})
	if err != nil {
		t.Fatalf("SchemaDigest: %v", err)
	}
	right, err := SchemaDigest([]ViewSchema{
		{Schema: "reporting", View: "expense_detail", Columns: []SchemaColumn{{Name: "receipt_no", PostgreSQLType: "TEXT"}}},
		{Schema: "reporting", View: "expense_summary", Columns: []SchemaColumn{{Name: "month", PostgreSQLType: "TEXT"}, {Name: "total_amount", PostgreSQLType: "numeric"}}},
	})
	if err != nil {
		t.Fatalf("SchemaDigest reordered views: %v", err)
	}
	if left != right {
		t.Fatalf("schema digest depends on view order: %s != %s", left, right)
	}
	reorderedColumns, err := SchemaDigest([]ViewSchema{
		{Schema: "reporting", View: "expense_summary", Columns: []SchemaColumn{{Name: "total_amount", PostgreSQLType: "numeric"}, {Name: "month", PostgreSQLType: "text"}}},
		{Schema: "reporting", View: "expense_detail", Columns: []SchemaColumn{{Name: "receipt_no", PostgreSQLType: "text"}}},
	})
	if err != nil {
		t.Fatalf("SchemaDigest reordered columns: %v", err)
	}
	if reorderedColumns == left {
		t.Fatal("schema digest accepted reordered columns")
	}
	changedDefinition, err := SchemaDigest([]ViewSchema{
		{Schema: "reporting", View: "expense_summary", Definition: "SELECT month, total_amount FROM source_a", Columns: []SchemaColumn{{Name: "month", PostgreSQLType: "text"}, {Name: "total_amount", PostgreSQLType: "numeric"}}},
		{Schema: "reporting", View: "expense_detail", Columns: []SchemaColumn{{Name: "receipt_no", PostgreSQLType: "text"}}},
	})
	if err != nil {
		t.Fatalf("SchemaDigest changed definition: %v", err)
	}
	if changedDefinition == left {
		t.Fatal("schema digest accepted changed view definition")
	}
	sameDefinitionSpacing, err := SchemaDigest([]ViewSchema{
		{Schema: "reporting", View: "expense_summary", Definition: "SELECT   month,\n total_amount   FROM source_a", Columns: []SchemaColumn{{Name: "month", PostgreSQLType: "text"}, {Name: "total_amount", PostgreSQLType: "numeric"}}},
		{Schema: "reporting", View: "expense_detail", Columns: []SchemaColumn{{Name: "receipt_no", PostgreSQLType: "text"}}},
	})
	if err != nil {
		t.Fatalf("SchemaDigest definition spacing: %v", err)
	}
	if sameDefinitionSpacing != changedDefinition {
		t.Fatal("schema digest depended on insignificant definition whitespace")
	}
}

func TestPostgreSQLMajorVersionParsing(t *testing.T) {
	for input, want := range map[string]int{"160002": 16, "150007": 15, "90624": 9} {
		got, err := postgresMajorVersion(input)
		if err != nil || got != want {
			t.Fatalf("postgresMajorVersion(%q) = %d, %v; want %d", input, got, err, want)
		}
	}
	if _, err := postgresMajorVersion("not-a-version"); err == nil {
		t.Fatal("invalid PostgreSQL version was accepted")
	}
}

func TestCompareAttestationRejectsIdentityAndSchemaMismatches(t *testing.T) {
	expected := ExpectedAttestation{
		DatasourceID: "taskgate-test-expenses", Database: "travel_demo",
		User: "gateway_reader", PostgreSQLMajorVersion: 16,
	}
	valid := Attestation{
		DatasourceID: "taskgate-test-expenses", Database: "travel_demo",
		User: "gateway_reader", PostgreSQLMajorVersion: 16,
		SchemaDigest: strings.Repeat("a", 64),
	}
	for name, mutate := range map[string]func(*Attestation){
		"datasource": func(value *Attestation) { value.DatasourceID = "other-source" },
		"database":   func(value *Attestation) { value.Database = "other_database" },
		"user":       func(value *Attestation) { value.User = "other_reader" },
		"version":    func(value *Attestation) { value.PostgreSQLMajorVersion = 15 },
		"schema":     func(value *Attestation) { value.SchemaDigest = strings.Repeat("b", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			actual := valid
			mutate(&actual)
			connector := Connector{expectedAttestation: expected, expectedSchemaDigest: valid.SchemaDigest}
			if err := connector.compareAttestation(actual); !IsCode(err, CodeSchemaDrift) {
				t.Fatalf("compareAttestation() = %v, want %s", err, CodeSchemaDrift)
			}
		})
	}
}

func TestClassifyQueryError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code ErrorCode
	}{
		{name: "deadline", err: context.DeadlineExceeded, code: CodeQueryTimeout},
		{name: "postgres cancellation", err: &pgconn.PgError{Code: "57014"}, code: CodeQueryTimeout},
		{name: "ordinary", err: errors.New("query failed"), code: CodeQueryFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyQueryError(test.err); !IsCode(got, test.code) {
				t.Fatalf("classifyQueryError() = %v, want %s", got, test.code)
			}
		})
	}
}

func TestClosedConnectorFailsSafely(t *testing.T) {
	var connector *Connector
	_, err := connector.Query(context.Background(), QueryRequest{SQL: "SELECT 1", MaxRows: 1})
	if !IsCode(err, CodeConnection) {
		t.Fatalf("Query() error = %v", err)
	}
}
