package experiment

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

func TestCurrentReceiptVersionAcceptsProductionV10AndRejectsForgedVersions(t *testing.T) {
	if !currentReceiptVersion(queryreceipt.Version) {
		t.Fatalf("production Receipt version %q was rejected", queryreceipt.Version)
	}
	for _, forged := range []string{"8", "11"} {
		t.Run(forged, func(t *testing.T) {
			if currentReceiptVersion(forged) {
				t.Fatalf("forged non-current Receipt version %q was accepted", forged)
			}
		})
	}
}

func TestCurrentReceiptVersionIsUsedByEveryLiveEvidenceGate(t *testing.T) {
	numericPin := regexp.MustCompile(`ReceiptVersion\s*!=\s*"[0-9]+"`)
	for _, path := range []string{
		"rls_validation.go",
		"attack_validation.go",
		"concurrency_validation.go",
		"rq5_validation.go",
		"finalize.go",
	} {
		value, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(value), "currentReceiptVersion(") {
			t.Fatalf("%s does not use the single-sourced current Receipt gate", path)
		}
		if numericPin.Match(value) {
			t.Fatalf("%s restored a numeric ReceiptVersion comparison", path)
		}
	}
}

func TestHistoricalSampleV1ReceiptVersionRemainsWireData(t *testing.T) {
	historical := validTestSample()
	historical.ReceiptVersion = "8"
	encoded, err := json.Marshal(historical)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Sample
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode retained sample-v1 wire: %v", err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("validate retained sample-v1 wire: %v", err)
	}
	if decoded.SchemaVersion != SampleSchemaVersion || decoded.ReceiptVersion != "8" {
		t.Fatalf("historical sample-v1 wire was reinterpreted: schema=%d receipt=%q",
			decoded.SchemaVersion, decoded.ReceiptVersion)
	}
}
