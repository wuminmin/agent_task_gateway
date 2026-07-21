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
