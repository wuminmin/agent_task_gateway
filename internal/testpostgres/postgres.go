package testpostgres

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var schemaSequence atomic.Uint64

// SchemaDSN creates an isolated PostgreSQL schema and returns a DSN whose
// search_path points at it. Database-backed tests are skipped when the shared
// test DSN is not configured; make verify always configures it.
func SchemaDSN(t testing.TB) string {
	t.Helper()
	base := strings.TrimSpace(os.Getenv("CONTROL_TEST_POSTGRES_DSN"))
	if base == "" {
		t.Skip("CONTROL_TEST_POSTGRES_DSN is required for PostgreSQL control-store tests")
	}
	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse CONTROL_TEST_POSTGRES_DSN: %v", err)
	}
	schema := fmt.Sprintf("test_%d_%d", time.Now().UnixNano(), schemaSequence.Add(1))
	db, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open PostgreSQL test database: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		_ = db.Close()
		t.Fatalf("create PostgreSQL test schema: %v", err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_, _ = db.ExecContext(cleanupCtx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
		_ = db.Close()
	})
	return parsed.String()
}

// StatementStatsDSN is the deployment whose pg_stat_statements a test is about.
//
// It is deliberately NOT SchemaDSN. Three live tests -- the pin domain
// separation proof and both halves of the strict-AST C3 gate -- are about what
// the BUSINESS server retains in pg_stat_statements, and they were reading the
// control-store DSN, which has no such extension. Every one of them therefore
// skipped with "pg_stat_statements is not installed on this deployment" on a
// harness where it demonstrably is installed, so the gate that the whole
// classifier rests on had never actually run.
//
// A superuser DSN is required rather than gateway_reader's:
// pg_stat_statements_reset() is not grantable to the reader role, and a test
// that cannot reset the view cannot attribute what it then observes.
func StatementStatsDSN(t testing.TB) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("BUSINESS_ADMIN_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("BUSINESS_ADMIN_TEST_POSTGRES_DSN is required for pg_stat_statements tests; " +
			"scripts/db-test-env.sh test exports it")
	}
	return dsn
}

// statementStatsLockKey serializes the tests that reset pg_stat_statements.
//
// The view is server-wide, and `go test ./...` runs packages in parallel
// processes. Two tests that each reset it and then attribute what they observe
// would interleave into counts neither one produced -- and the failure would
// look like a property of the server rather than of the harness. The lock is
// session-scoped, so closing the connection releases it even on a panic.
const statementStatsLockKey = int64(0x7a5c1a7e57a75)

// LockStatementStats takes the server-wide lock for a test that is about to
// reset pg_stat_statements and read what accumulates after it.
func LockStatementStats(t testing.TB, execute func(query string, arguments ...any) error) {
	t.Helper()
	if err := execute(`SELECT pg_advisory_lock($1)`, statementStatsLockKey); err != nil {
		t.Fatalf("take the pg_stat_statements test lock: %v", err)
	}
}
