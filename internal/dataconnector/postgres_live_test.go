package dataconnector

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"taskbound.local/agent-data-gateway/internal/testpostgres"
)

func TestQueryReturnsTimeTZAsLosslessText(t *testing.T) {
	dsn := testpostgres.SchemaDSN(t)
	ctx := context.Background()
	connector, err := New(ctx, Config{
		DSN: dsn, StatementTimeout: time.Second, ConnectTimeout: time.Second,
		MaxRows: 10, MaxConnections: 1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer connector.Close()

	result, err := connector.Query(ctx, QueryRequest{
		SQL:              `SELECT '04:05:06.789123-08:30:15'::pg_catalog.timetz AS legacy_time`,
		StatementTimeout: time.Second,
		MaxRows:          1,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(result.Columns) != 1 || result.Columns[0].DataTypeOID != pgtype.TimetzOID {
		t.Fatalf("timetz columns = %+v", result.Columns)
	}
	if len(result.Rows) != 1 || len(result.Rows[0]) != 1 || result.Rows[0][0] != "04:05:06.789123-08:30:15" {
		t.Fatalf("timetz rows = %#v", result.Rows)
	}
}

func TestLiveSchemaDigestDetectsViewDefinitionDrift(t *testing.T) {
	dsn := testpostgres.SchemaDSN(t)
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	schema := parsed.Query().Get("search_path")
	if schema == "" {
		t.Fatal("test DSN did not include search_path")
	}
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	defer db.Close()
	mustExec(t, db, `CREATE TABLE source_a(month text, total_amount numeric)`)
	mustExec(t, db, `CREATE TABLE source_b(month text, total_amount numeric)`)
	mustExec(t, db, `CREATE VIEW expense_summary AS SELECT month, total_amount FROM source_a`)

	expected := []ViewSchema{{
		Schema: schema, View: "expense_summary",
		Columns: []SchemaColumn{{Name: "month", PostgreSQLType: "text"}, {Name: "total_amount", PostgreSQLType: "numeric"}},
	}}
	digest := liveSchemaDigest(t, ctx, db, expected)
	connector, err := New(ctx, Config{
		DSN: dsn, StatementTimeout: time.Second, ConnectTimeout: time.Second,
		MaxRows: 10, MaxConnections: 1, ExpectedSchema: expected, ExpectedSchemaDigest: digest,
	})
	if err != nil {
		t.Fatalf("initial connector attestation: %v", err)
	}
	connector.Close()

	mustExec(t, db, `CREATE OR REPLACE VIEW expense_summary AS SELECT month, total_amount FROM source_b`)
	connector, err = New(ctx, Config{
		DSN: dsn, StatementTimeout: time.Second, ConnectTimeout: time.Second,
		MaxRows: 10, MaxConnections: 1, ExpectedSchema: expected, ExpectedSchemaDigest: digest,
	})
	if err == nil {
		connector.Close()
		t.Fatal("view definition drift was accepted")
	}
	if !IsCode(err, CodeSchemaDrift) {
		t.Fatalf("view definition drift error = %v, want %s", err, CodeSchemaDrift)
	}
}

func TestQueryReattestsSchemaDigestInsideReadOnlyTransaction(t *testing.T) {
	dsn := testpostgres.SchemaDSN(t)
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	schema := parsed.Query().Get("search_path")
	if schema == "" {
		t.Fatal("test DSN did not include search_path")
	}
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	defer db.Close()
	mustExec(t, db, `CREATE TABLE source_a(month text, total_amount numeric)`)
	mustExec(t, db, `CREATE TABLE source_b(month text, total_amount numeric)`)
	mustExec(t, db, `CREATE VIEW expense_summary AS SELECT month, total_amount FROM source_a`)

	expected := []ViewSchema{{
		Schema: schema, View: "expense_summary",
		Columns: []SchemaColumn{{Name: "month", PostgreSQLType: "text"}, {Name: "total_amount", PostgreSQLType: "numeric"}},
	}}
	digest := liveSchemaDigest(t, ctx, db, expected)
	connector, err := New(ctx, Config{
		DSN: dsn, StatementTimeout: time.Second, ConnectTimeout: time.Second,
		MaxRows: 10, MaxConnections: 1, ExpectedSchema: expected, ExpectedSchemaDigest: digest,
	})
	if err != nil {
		t.Fatalf("initial connector attestation: %v", err)
	}
	defer connector.Close()

	mustExec(t, db, `CREATE OR REPLACE VIEW expense_summary AS SELECT month, total_amount FROM source_b`)
	_, err = connector.Query(ctx, QueryRequest{
		SQL:              fmt.Sprintf(`SELECT month, total_amount FROM %s.expense_summary`, schema),
		StatementTimeout: time.Second,
		MaxRows:          10,
	})
	if !IsCode(err, CodeSchemaDrift) {
		t.Fatalf("query after in-transaction schema re-attestation = %v, want %s", err, CodeSchemaDrift)
	}
}

func TestLiveSchemaDigestSupportsFrozenMaterializedPublication(t *testing.T) {
	dsn := testpostgres.SchemaDSN(t)
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	schema := parsed.Query().Get("search_path")
	if schema == "" {
		t.Fatal("test DSN did not include search_path")
	}
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	defer db.Close()
	mustExec(t, db, `CREATE TABLE source_a(month text, total_amount numeric)`)
	mustExec(t, db, `INSERT INTO source_a VALUES ('2026-01', 10)`)
	mustExec(t, db, `CREATE MATERIALIZED VIEW expense_summary AS SELECT month, total_amount FROM source_a`)

	expected := []ViewSchema{{
		Schema: schema, View: "expense_summary",
		Columns: []SchemaColumn{{Name: "month", PostgreSQLType: "text"}, {Name: "total_amount", PostgreSQLType: "numeric"}},
	}}
	digest := liveSchemaDigest(t, ctx, db, expected)
	connector, err := New(ctx, Config{
		DSN: dsn, StatementTimeout: time.Second, ConnectTimeout: time.Second,
		MaxRows: 10, MaxConnections: 1, ExpectedSchema: expected, ExpectedSchemaDigest: digest,
	})
	if err != nil {
		t.Fatalf("attest materialized publication: %v", err)
	}
	defer connector.Close()

	// Mutating the seed relation does not alter an already published snapshot.
	mustExec(t, db, `UPDATE source_a SET month = '2026-02', total_amount = 99`)
	result, err := connector.Query(ctx, QueryRequest{
		SQL:              fmt.Sprintf(`SELECT month FROM %s.expense_summary`, schema),
		StatementTimeout: time.Second, MaxRows: 10,
	})
	if err != nil {
		t.Fatalf("query materialized publication: %v", err)
	}
	if result.RowCount != 1 || fmt.Sprint(result.Rows[0][0]) != "2026-01" {
		t.Fatalf("materialized publication changed with its seed relation: %+v", result.Rows)
	}
}

func liveSchemaDigest(t *testing.T, ctx context.Context, db *sql.DB, expected []ViewSchema) string {
	t.Helper()
	actual := make([]ViewSchema, 0, len(expected))
	for _, view := range expected {
		rows, err := db.QueryContext(ctx, `
SELECT attr.attname,
       CASE
           WHEN typ.typtype = 'd' THEN
               CASE
                   WHEN base_typ.typelem <> 0 AND base_typ.typlen = -1 THEN 'ARRAY'
                   WHEN base_typ_ns.nspname = 'pg_catalog' THEN format_type(typ.typbasetype, NULL)
                   ELSE 'USER-DEFINED'
               END
           ELSE
               CASE
                   WHEN typ.typelem <> 0 AND typ.typlen = -1 THEN 'ARRAY'
                   WHEN typ_ns.nspname = 'pg_catalog' THEN format_type(attr.atttypid, NULL)
                   ELSE 'USER-DEFINED'
               END
       END
FROM pg_namespace AS ns
JOIN pg_class AS cls ON cls.relnamespace = ns.oid
JOIN pg_attribute AS attr ON attr.attrelid = cls.oid AND attr.attnum > 0 AND NOT attr.attisdropped
JOIN pg_type AS typ ON typ.oid = attr.atttypid
JOIN pg_namespace AS typ_ns ON typ_ns.oid = typ.typnamespace
LEFT JOIN pg_type AS base_typ ON typ.typtype = 'd' AND base_typ.oid = typ.typbasetype
LEFT JOIN pg_namespace AS base_typ_ns ON base_typ_ns.oid = base_typ.typnamespace
WHERE ns.nspname=$1 AND cls.relname=$2
  AND cls.relkind IN ('r', 'v', 'm', 'f', 'p')
ORDER BY attr.attnum`, view.Schema, view.View)
		if err != nil {
			t.Fatalf("query live columns: %v", err)
		}
		var columns []SchemaColumn
		for rows.Next() {
			var column SchemaColumn
			if err := rows.Scan(&column.Name, &column.PostgreSQLType); err != nil {
				rows.Close()
				t.Fatalf("scan live column: %v", err)
			}
			columns = append(columns, column)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("close column rows: %v", err)
		}
		var definition string
		if err := db.QueryRowContext(ctx, `
WITH taskgate_schema_digest_path AS (
	SELECT set_config('search_path', 'pg_catalog', true)
)
SELECT pg_get_viewdef(format('%I.%I', $1::text, $2::text)::regclass, true)
FROM taskgate_schema_digest_path`, view.Schema, view.View).Scan(&definition); err != nil {
			t.Fatalf("query live view definition: %v", err)
		}
		actual = append(actual, ViewSchema{Schema: view.Schema, View: view.View, Definition: definition, Columns: columns})
	}
	digest, err := SchemaDigest(actual)
	if err != nil {
		t.Fatalf("digest live schema: %v", err)
	}
	return digest
}

func mustExec(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}
