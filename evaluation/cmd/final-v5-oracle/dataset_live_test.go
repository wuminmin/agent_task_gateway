package main

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5dataset"
)

func TestDatasetFingerprintLiveCommandAgainstPostgreSQLRequiresLiveDSN(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("BUSINESS_TEST_POSTGRES_DSN"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_BUSINESS_DSN"))
	}
	if dsn == "" {
		t.Fatal("requires BUSINESS_TEST_POSTGRES_DSN or TASKGATE_FINAL_V5_BUSINESS_DSN; this live test must not skip")
	}
	code, output, errorsOutput := invokeCLI([]string{"dataset-fingerprint-live"}, "")
	if code != 0 || errorsOutput != "" {
		t.Fatalf("dataset-fingerprint-live code=%d stderr=%q", code, errorsOutput)
	}
	var agreement finalv5dataset.BenchmarkAgreement
	if err := json.Unmarshal([]byte(output), &agreement); err != nil {
		t.Fatal(err)
	}
	if agreement.Version != finalv5dataset.BenchmarkAgreementVersion || !agreement.Agreed ||
		agreement.PreparedStatementCount != 0 || !reflect.DeepEqual(agreement.Reference, agreement.Observed) ||
		agreement.Observed.ProductCount != 5 || agreement.Observed.RowCount != 815_000 ||
		agreement.Observed.PeakBufferedRows != 1 || len(agreement.Products) != 5 ||
		agreement.Observed.SHA256 != "f90239bb32ef9542089ca8f1bd7c30c7870cbe627e835698364bdb9b4dc15978" {
		t.Fatalf("live command benchmark Dataset agreement = %+v", agreement)
	}
	if bytes.Contains([]byte(output), []byte(dsn)) {
		t.Fatal("credential-free live command output exposed its DSN")
	}
	t.Logf("rows=%d products=%d reference_sha256=%s observed_sha256=%s prepared_statement_count=%d agreed=%t",
		agreement.Observed.RowCount, agreement.Observed.ProductCount, agreement.Reference.SHA256,
		agreement.Observed.SHA256, agreement.PreparedStatementCount, agreement.Agreed)
}
