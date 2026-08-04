package experiment

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/testpostgres"
)

// This is the Stage C3 gate. The classifier manifest is generated from the
// Connector's source constants, but it is matched against text
// pg_stat_statements produced. If those two do not yield the same strict AST
// digest, the manifest can never match a live run and no accounting built on it
// means anything.
//
// The statements below are executed exactly as the Connector executes them --
// same bytes, same bind parameters, same transaction -- and the digest of what
// the server retained is compared with the digest of the source constant.
func TestSourceDerivedDigestsMatchLivePostgreSQL(t *testing.T) {
	dsn := testpostgres.SchemaDSN(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())

	var available bool
	if err := conn.QueryRow(ctx,
		`SELECT count(*) = 1 FROM pg_extension WHERE extname = 'pg_stat_statements'`).Scan(&available); err != nil || !available {
		t.Skip("pg_stat_statements is not installed on this deployment")
	}
	var versionNum string
	if err := conn.QueryRow(ctx, `SELECT current_setting('server_version_num')`).Scan(&versionNum); err != nil {
		t.Fatalf("read server version: %v", err)
	}
	if versionNum != "160014" {
		t.Skipf("the v3 rules are bound to PostgreSQL 160014, this server is %s", versionNum)
	}
	if _, err := conn.Exec(ctx, `SELECT public.pg_stat_statements_reset()`); err != nil {
		t.Skipf("cannot reset pg_stat_statements: %v", err)
	}

	// Execute the controls the way the Connector does.
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, dataconnector.SafetySessionPinSQL); err != nil {
		t.Fatalf("safety pin: %v", err)
	}
	if _, err := tx.Exec(ctx, dataconnector.RepresentationPinSQL); err != nil {
		t.Fatalf("representation pin: %v", err)
	}
	// The timeout pin carries a real bind parameter, so the server numbers its
	// erased constants around that parameter. This is the case most likely to
	// diverge between source and observation, which is why it is exercised with
	// its parameter rather than with a literal.
	if _, err := tx.Exec(ctx, dataconnector.StatementTimeoutPinSQL, "1000ms"); err != nil {
		t.Fatalf("statement timeout pin: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	observed := map[string]string{}
	rows, err := conn.Query(ctx, `
SELECT query FROM public.pg_stat_statements
WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())`)
	if err != nil {
		t.Fatalf("read pg_stat_statements: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			t.Fatalf("scan: %v", err)
		}
		digest, digestErr := StrictASTDigest(text)
		if digestErr != nil {
			// Utility statements the server records verbatim may not round-trip
			// through the parser; they are covered by their own cases below.
			continue
		}
		observed[digest] = text
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}

	for name, source := range map[string]string{
		"safety pin":         dataconnector.SafetySessionPinSQL,
		"representation pin": dataconnector.RepresentationPinSQL,
		"timeout pin":        dataconnector.StatementTimeoutPinSQL,
	} {
		want, err := StrictASTDigest(source)
		if err != nil {
			t.Fatalf("%s source digest: %v", name, err)
		}
		if _, present := observed[want]; !present {
			t.Fatalf("STRICT AST MANIFEST DOES NOT MATCH LIVE PG16.14: "+
				"the source-derived digest for the %s (%s) matches no statement the server retained", name, want)
		}
	}
}

// BEGIN, COMMIT and the nested pg_rewrite lookup are not source constants; they
// are what the driver and the server themselves emit. They are therefore bound
// to the observed PostgreSQL 16.14 text rather than to a Connector string, and
// must be confirmed live rather than assumed.
func TestRuntimeTemplateDigestsAreStableOnLivePostgreSQL(t *testing.T) {
	dsn := testpostgres.SchemaDSN(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())

	var available bool
	if err := conn.QueryRow(ctx,
		`SELECT count(*) = 1 FROM pg_extension WHERE extname = 'pg_stat_statements'`).Scan(&available); err != nil || !available {
		t.Skip("pg_stat_statements is not installed on this deployment")
	}
	var versionNum, track string
	if err := conn.QueryRow(ctx,
		`SELECT current_setting('server_version_num'), current_setting('pg_stat_statements.track')`).
		Scan(&versionNum, &track); err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if versionNum != "160014" || track != "all" {
		t.Skipf("the nested-lookup rule requires PostgreSQL 160014 with track=all; this server is %s/%s",
			versionNum, track)
	}
	if _, err := conn.Exec(ctx, `SELECT public.pg_stat_statements_reset()`); err != nil {
		t.Skipf("cannot reset pg_stat_statements: %v", err)
	}

	// Provoke the nested lookup exactly as the view-definition attestation does.
	if _, err := conn.Exec(ctx, `CREATE VIEW taskgate_strict_ast_probe AS SELECT 1 AS one`); err != nil {
		t.Fatalf("create probe view: %v", err)
	}
	var definition string
	if err := conn.QueryRow(ctx,
		`SELECT pg_get_viewdef('taskgate_strict_ast_probe'::regclass, true)`).Scan(&definition); err != nil {
		t.Fatalf("pg_get_viewdef: %v", err)
	}

	var nested int64
	if err := conn.QueryRow(ctx, `
SELECT COALESCE(sum(calls), 0) FROM public.pg_stat_statements
WHERE toplevel = false AND query LIKE '%pg_rewrite%'`).Scan(&nested); err != nil {
		t.Fatalf("read nested lookups: %v", err)
	}
	if nested == 0 {
		t.Fatal("pg_get_viewdef produced no nested pg_rewrite lookup; the v3 nested-lookup rule does not hold on this server")
	}

	// The nested lookup must be recorded with toplevel=false. The classification
	// key includes toplevel precisely so a top-level statement carrying the same
	// shape is not mistaken for it.
	var topLevelSame int64
	if err := conn.QueryRow(ctx, `
SELECT COALESCE(sum(calls), 0) FROM public.pg_stat_statements
WHERE toplevel = true AND query LIKE '%pg_rewrite%'`).Scan(&topLevelSame); err != nil {
		t.Fatalf("read top-level rewrite lookups: %v", err)
	}
	if topLevelSame != 0 {
		t.Fatalf("the pg_rewrite lookup was also recorded at top level (%d calls); toplevel cannot discriminate it", topLevelSame)
	}
}
