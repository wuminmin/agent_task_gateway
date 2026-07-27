package exposure

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestPostgreSQL16JSONBDifferentialCanonicalization checks the V2 JSONB
// canonicalizer against PostgreSQL's own json/jsonb semantics. The Compose
// acceptance suite supplies BUSINESS_TEST_POSTGRES_DSN; ordinary unit runs
// skip this database-dependent check.
func TestPostgreSQL16JSONBDifferentialCanonicalization(t *testing.T) {
	dsn := os.Getenv("BUSINESS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("BUSINESS_TEST_POSTGRES_DSN is required for PostgreSQL differential tests")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open business PostgreSQL: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping business PostgreSQL: %v", err)
	}

	var serverVersion int
	if err := db.QueryRowContext(ctx, `SELECT current_setting('server_version_num')::integer`).Scan(&serverVersion); err != nil {
		t.Fatalf("read PostgreSQL version: %v", err)
	}
	if serverVersion/10000 != 16 {
		t.Fatalf("PostgreSQL major version = %d, want 16", serverVersion/10000)
	}

	if _, err := CanonicalSQLTypeV2("json"); err == nil {
		t.Fatal("PostgreSQL json entered the V2 type domain")
	}

	tests := []struct {
		name  string
		left  string
		right string
	}{
		{name: "whitespace", left: `{"a": 1}`, right: `{"a":1}`},
		{name: "object key order", left: `{"a":1,"b":2}`, right: `{"b":2,"a":1}`},
		{name: "duplicate object keys", left: `{"a":1,"a":2}`, right: `{"a":2}`},
		{name: "numeric lexical form", left: `{"n":1.00}`, right: `{"n":1}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var leftJSON, rightJSON, leftJSONB, rightJSONB string
			var jsonbEqual bool
			err := db.QueryRowContext(ctx, `
SELECT ($1::text)::json::text,
       ($2::text)::json::text,
       ($1::text)::jsonb::text,
       ($2::text)::jsonb::text,
       ($1::text)::jsonb = ($2::text)::jsonb`, test.left, test.right).
				Scan(&leftJSON, &rightJSON, &leftJSONB, &rightJSONB, &jsonbEqual)
			if err != nil {
				t.Fatalf("query PostgreSQL json/jsonb semantics: %v", err)
			}
			if leftJSON == rightJSON {
				t.Fatalf("PostgreSQL json did not preserve the tested lexical distinction: %q", leftJSON)
			}
			if !jsonbEqual {
				t.Fatalf("PostgreSQL jsonb values are not equal: %q / %q", leftJSONB, rightJSONB)
			}

			// Feed the original spellings to the Go encoder. PostgreSQL's jsonb
			// rendering has already erased some of the distinctions under test.
			leftCanonical, err := CanonicalSQLValue("jsonb", []byte(test.left))
			if err != nil {
				t.Fatalf("canonicalize left jsonb: %v", err)
			}
			rightCanonical, err := CanonicalSQLValue("jsonb", []byte(test.right))
			if err != nil {
				t.Fatalf("canonicalize right jsonb: %v", err)
			}
			if leftCanonical != rightCanonical {
				t.Fatalf("V2 canonical values disagree for PostgreSQL-equal jsonb: %q / %q", leftCanonical, rightCanonical)
			}

			leftFact, err := NewBaseCellFactV2("jsonb.source", "snapshot-1", "row-1", "payload", "jsonb", []byte(test.left))
			if err != nil {
				t.Fatalf("construct left jsonb FactID: %v", err)
			}
			rightFact, err := NewBaseCellFactV2("jsonb.source", "snapshot-1", "row-1", "payload", "jsonb", []byte(test.right))
			if err != nil {
				t.Fatalf("construct right jsonb FactID: %v", err)
			}
			leftHash, err := leftFact.Hash()
			if err != nil {
				t.Fatalf("hash left jsonb FactID: %v", err)
			}
			rightHash, err := rightFact.Hash()
			if err != nil {
				t.Fatalf("hash right jsonb FactID: %v", err)
			}
			if leftHash != rightHash {
				t.Fatalf("PostgreSQL-equal jsonb values produced different FactIDs: %s / %s", leftHash, rightHash)
			}
		})
	}
}
