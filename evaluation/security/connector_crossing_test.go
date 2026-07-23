package security

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
	"taskbound.local/agent-data-gateway/internal/testpostgres"
)

// TestConnectorCrossing measures unauthorized-crossing attempts across the
// logical/physical boundary and the schema-attestation fence. The policy cases
// run in-process against the sqlpolicy engine; the live schema-drift case runs
// against PostgreSQL and is skipped without CONTROL_TEST_POSTGRES_DSN. Every
// case records an unauthorized crossing if the attempt is NOT blocked.
func TestConnectorCrossing(t *testing.T) {
	rec := &violationRecorder{}
	defer func() {
		writeExperimentSummary(t, "connector_crossing", map[string]any{"unauthorized_crossings": rec.count})
	}()
	corpus := loadSecurityCorpus(t, "connector-crossing.json")
	for _, c := range corpus.Cases {
		c := c
		t.Run(c.ID, func(t *testing.T) {
			runConnectorCrossingCase(t, c.ID, rec)
		})
	}
}

func runConnectorCrossingCase(t *testing.T, id string, rec *violationRecorder) {
	t.Helper()
	switch id {
	case "policy_denies_ungranted_relation":
		policyDenies(t, rec, "SELECT count(*) FROM zzz_ungranted_product", "ungranted relation was authorized")
	case "policy_denies_system_object":
		policyDenies(t, rec, "SELECT count(*) FROM pg_catalog.pg_class", "system object was authorized")
	case "policy_denies_write_statement":
		policyDenies(t, rec, "DELETE FROM zzz_ungranted_product", "write statement was authorized")
	case "live_schema_drift_detected_after_ddl":
		liveSchemaDriftDetected(t, rec)
	default:
		t.Fatalf("unknown connector-crossing case: %s", id)
	}
}

// crossingGrant loads the AST-gateway product grant used by the rest of the
// security corpus and wraps it as a sqlpolicy grant.
func crossingGrant(t *testing.T) sqlpolicy.Grant {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "ast-gateway", "tpch.json"))
	if err != nil {
		t.Fatalf("read ast-gateway grant: %v", err)
	}
	var config struct {
		Products []sqlpolicy.ProductGrant `json:"products"`
	}
	if err := json.Unmarshal(raw, &config); err != nil || len(config.Products) == 0 {
		t.Fatalf("invalid ast-gateway grant: %v", err)
	}
	return sqlpolicy.Grant{Products: config.Products}
}

func policyDenies(t *testing.T, rec *violationRecorder, agentSQL, failure string) {
	t.Helper()
	engine := sqlpolicy.New(sqlpolicy.Config{})
	_, err := engine.Authorize(sqlpolicy.Request{SQL: agentSQL, Grant: crossingGrant(t), RowLimit: 10_000})
	if err == nil {
		rec.crossing(t, failure)
	}
}

// liveSchemaDriftDetected proves the data connector re-attests on construction
// and refuses to start after DDL changes a reporting view definition. This is
// the runtime fence that closes the schema-attestation/execution TOCTOU window.
func liveSchemaDriftDetected(t *testing.T, rec *violationRecorder) {
	t.Helper()
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
	crossingExec(t, db, `CREATE TABLE source_a(month text, total_amount numeric)`)
	crossingExec(t, db, `CREATE TABLE source_b(month text, total_amount numeric)`)
	crossingExec(t, db, `CREATE VIEW expense_summary AS SELECT month, total_amount FROM source_a`)

	expected := []dataconnector.ViewSchema{{
		Schema: schema, View: "expense_summary",
		Columns: []dataconnector.SchemaColumn{{Name: "month", PostgreSQLType: "text"}, {Name: "total_amount", PostgreSQLType: "numeric"}},
	}}
	digest := liveDigest(t, ctx, db, expected)
	initial, err := dataconnector.New(ctx, dataconnector.Config{
		DSN: dsn, StatementTimeout: time.Second, ConnectTimeout: time.Second,
		MaxRows: 10, MaxConnections: 1, ExpectedSchema: expected, ExpectedSchemaDigest: digest,
	})
	if err != nil {
		t.Fatalf("initial connector attestation failed: %v", err)
	}
	initial.Close()

	crossingExec(t, db, `CREATE OR REPLACE VIEW expense_summary AS SELECT month, total_amount FROM source_b`)
	drifted, err := dataconnector.New(ctx, dataconnector.Config{
		DSN: dsn, StatementTimeout: time.Second, ConnectTimeout: time.Second,
		MaxRows: 10, MaxConnections: 1, ExpectedSchema: expected, ExpectedSchemaDigest: digest,
	})
	if err == nil {
		drifted.Close()
		rec.crossing(t, "view definition drift was accepted by re-attestation")
	}
	if !dataconnector.IsCode(err, dataconnector.CodeSchemaDrift) {
		rec.crossing(t, "view definition drift did not surface CodeSchemaDrift")
	}
}

func crossingExec(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// liveDigest mirrors the connector's attestation query so the expected digest
// matches what the live schema produces before drift is injected.
func liveDigest(t *testing.T, ctx context.Context, db *sql.DB, expected []dataconnector.ViewSchema) string {
	t.Helper()
	actual := make([]dataconnector.ViewSchema, 0, len(expected))
	for _, view := range expected {
		rows, err := db.QueryContext(ctx, `
SELECT column_name, data_type
FROM information_schema.columns
WHERE table_schema=$1 AND table_name=$2
ORDER BY ordinal_position`, view.Schema, view.View)
		if err != nil {
			t.Fatalf("query live columns: %v", err)
		}
		var columns []dataconnector.SchemaColumn
		for rows.Next() {
			var column dataconnector.SchemaColumn
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
		actual = append(actual, dataconnector.ViewSchema{Schema: view.Schema, View: view.View, Definition: definition, Columns: columns})
	}
	digest, err := dataconnector.SchemaDigest(actual)
	if err != nil {
		t.Fatalf("digest live schema: %v", err)
	}
	return digest
}
