//go:build taskgate_hostonly

// These cases require host resources the product Compose stack has no reason to
// carry: a Docker socket, the retained qualification artifacts, or a live
// benchmark Dataset. They exercise the evaluation harness rather than the
// product, and the formal campaign exercises the same material at runtime, so
// they sit behind taskgate_hostonly instead of failing the acceptance run.

package finalv5dataset

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestBenchmarkDatasetAgreementAgainstPostgreSQLRequiresLiveDSN(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("BUSINESS_TEST_POSTGRES_DSN"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_BUSINESS_DSN"))
	}
	if dsn == "" {
		t.Fatal("requires BUSINESS_TEST_POSTGRES_DSN or TASKGATE_FINAL_V5_BUSINESS_DSN; this live test must not skip")
	}
	agreement, err := VerifyBenchmarkPostgreSQL(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	if agreement.Version != BenchmarkAgreementVersion || !agreement.Agreed ||
		agreement.PreparedStatementCount != 0 || !reflect.DeepEqual(agreement.Reference, agreement.Observed) ||
		agreement.Observed.ProductCount != 5 || agreement.Observed.RowCount != 815_000 ||
		agreement.Observed.PeakBufferedRows != 1 || len(agreement.Products) != 5 ||
		agreement.Observed.SHA256 != benchmarkDatasetSHA256 {
		t.Fatalf("live full benchmark Dataset agreement = %+v", agreement)
	}
	encoded, err := json.Marshal(agreement)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(dsn)) {
		t.Fatal("credential-free benchmark agreement exposed its DSN")
	}
	t.Logf("rows=%d products=%d reference_sha256=%s observed_sha256=%s prepared_statement_count=%d agreed=%t",
		agreement.Observed.RowCount, agreement.Observed.ProductCount, agreement.Reference.SHA256,
		agreement.Observed.SHA256, agreement.PreparedStatementCount, agreement.Agreed)
}
