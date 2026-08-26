//go:build taskgate_hostonly

// These cases require host resources the product Compose stack has no reason to
// carry: a Docker socket, the retained qualification artifacts, or a live
// benchmark Dataset. The formal campaign exercises the same material at runtime.

package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
)

func TestExposureScaleDatasetAgreementAgainstPostgreSQL(t *testing.T) {
	if strings.TrimSpace(os.Getenv("BUSINESS_TEST_POSTGRES_DSN")) == "" &&
		strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_BUSINESS_DSN")) == "" {
		t.Skip("requires BUSINESS_TEST_POSTGRES_DSN or TASKGATE_FINAL_V5_BUSINESS_DSN")
	}
	code, output, stderr := invokeCLI([]string{"scale-dataset-agreement"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("scale-dataset-agreement code=%d stderr=%q", code, stderr)
	}
	var agreement finalv5oracle.ExposureScaleDatasetAgreement
	if err := json.Unmarshal([]byte(output), &agreement); err != nil {
		t.Fatal(err)
	}
	if !agreement.Agreed || agreement.Reference != agreement.Observed || agreement.Reference.RowCount != 414_000 ||
		agreement.Reference.SHA256 != "8ada6ddeeb221e24b906493734fa613c1e222c15864f29ed6398b4b8f1bb34f6" {
		t.Fatalf("live typed Dataset agreement = %+v", agreement)
	}
	t.Logf("rows=%d product_sha256=%s agreed=%t", agreement.Observed.RowCount, agreement.Observed.SHA256, agreement.Agreed)
}
