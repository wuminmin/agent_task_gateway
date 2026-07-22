package dataconnector

import (
	"context"
	"database/sql"
	"net/url"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/testpostgres"
)

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

func liveSchemaDigest(t *testing.T, ctx context.Context, db *sql.DB, expected []ViewSchema) string {
	t.Helper()
	actual := make([]ViewSchema, 0, len(expected))
	for _, view := range expected {
		rows, err := db.QueryContext(ctx, `
SELECT column_name, data_type
FROM information_schema.columns
WHERE table_schema=$1 AND table_name=$2
ORDER BY ordinal_position`, view.Schema, view.View)
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
