//go:build taskgate_integration

package main

import (
	"os"
	"strings"
	"testing"
)

// The live ProvSQL dataset agreement runs only in the DSN-enabled integration
// suite (go test -tags=taskgate_integration), where it must not skip; the
// default build, including the exposure evaluation container, has no business
// PostgreSQL to agree against.
func TestProvSQLDatasetAgreementAgainstPostgreSQLRequiresLiveDSN(t *testing.T) {
	if strings.TrimSpace(os.Getenv("BUSINESS_TEST_POSTGRES_DSN")) == "" &&
		strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_BUSINESS_DSN")) == "" {
		t.Fatal("requires BUSINESS_TEST_POSTGRES_DSN or TASKGATE_FINAL_V5_BUSINESS_DSN; this live test must not skip")
	}
	agreement, err := liveProvSQLDatasetAgreement(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !agreement.Agreed || agreement.PreparedStatementCount != 0 ||
		!reflect.DeepEqual(agreement.Reference, agreement.Observed) ||
		agreement.Reference.ProductCount != 3 || agreement.Reference.RowCount != finalv5oracle.ProvSQLDatasetRows ||
		len(agreement.Reference.Products) != 3 {
		t.Fatalf("live ProvSQL typed Dataset agreement = %+v", agreement)
	}
	encoded, err := json.Marshal(agreement)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"BUSINESS_TEST_POSTGRES_DSN", "TASKGATE_FINAL_V5_BUSINESS_DSN"} {
		if secret := strings.TrimSpace(os.Getenv(name)); secret != "" && bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("credential-free agreement exposed %s", name)
		}
	}
	t.Logf("rows=%d products=%d reference_sha256=%s observed_sha256=%s prepared_statement_count=%d agreed=%t",
		agreement.Observed.RowCount, agreement.Observed.ProductCount, agreement.Reference.SHA256,
		agreement.Observed.SHA256, agreement.PreparedStatementCount, agreement.Agreed)
}
