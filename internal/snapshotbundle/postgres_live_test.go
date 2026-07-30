package snapshotbundle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestScanDemoPostgresSnapshot compiles the human-reviewed artifact from the
// actual frozen materialized view. Matching all five expected digests proves
// that the DB values—not the deliberately forged JSON row below—are the exact
// cells used by the compiler. Ordinary unit runs skip without the business DB.
func TestScanDemoPostgresSnapshot(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("BUSINESS_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("BUSINESS_TEST_POSTGRES_DSN is required for snapshot scanner tests")
	}
	inputFile, err := os.Open(filepath.Join("..", "..", "config", "snapshots", "expense-detail-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	input, err := DecodeCompilerInput(inputFile)
	closeErr := inputFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	input.Snapshot.Rows = []SnapshotRow{{EntityKey: "forged-json-row", Values: map[string]any{"receipt_no": "forged"}}}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	scanned, err := ScanPostgresSnapshot(ctx, input, dsn)
	if err != nil {
		t.Fatalf("ScanPostgresSnapshot: %v", err)
	}
	if len(scanned.Snapshot.Rows) != 10 {
		t.Fatalf("scanned rows = %d, want 10", len(scanned.Snapshot.Rows))
	}
	for _, row := range scanned.Snapshot.Rows {
		if row.EntityKey != "" {
			t.Fatalf("DB scan retained caller entity-key assertion %q", row.EntityKey)
		}
	}
	bundle, err := Compile(scanned)
	if err != nil {
		t.Fatalf("compile DB-scanned snapshot: %v", err)
	}
	if bundle.Manifest.ManifestDigest != input.ExpectedDigests.ManifestDigest {
		t.Fatalf("DB-scanned manifest = %s, want reviewed %s", bundle.Manifest.ManifestDigest, input.ExpectedDigests.ManifestDigest)
	}

	nonMaterialized := input
	nonMaterialized.SourceRelation = "reporting.datasource_attestation"
	if _, err := ScanPostgresSnapshot(ctx, nonMaterialized, dsn); err == nil || !strings.Contains(err.Error(), "materialized view") {
		t.Fatalf("snapshot scanner accepted a non-materialized relation: %v", err)
	}

	wrongType := input
	wrongType.Snapshot.Fields = append([]SnapshotField(nil), input.Snapshot.Fields...)
	for index := range wrongType.Snapshot.Fields {
		if wrongType.Snapshot.Fields[index].Name == "receipt_no" {
			wrongType.Snapshot.Fields[index].SQLType = "numeric"
			wrongType.Snapshot.Fields[index].Collation = ""
			wrongType.Snapshot.Fields[index].CollationVersion = ""
		}
	}
	if _, err := ScanPostgresSnapshot(ctx, wrongType, dsn); err == nil || !strings.Contains(err.Error(), "type mismatch") {
		t.Fatalf("snapshot scanner accepted wrong physical type: %v", err)
	}

	wrongCollation := input
	wrongCollation.Snapshot.Fields = append([]SnapshotField(nil), input.Snapshot.Fields...)
	for index := range wrongCollation.Snapshot.Fields {
		if wrongCollation.Snapshot.Fields[index].Name == "receipt_no" {
			wrongCollation.Snapshot.Fields[index].Collation = "C"
			wrongCollation.Snapshot.Fields[index].CollationVersion = "builtin"
		}
	}
	if _, err := ScanPostgresSnapshot(ctx, wrongCollation, dsn); err == nil || !strings.Contains(err.Error(), "collation mismatch") {
		t.Fatalf("snapshot scanner accepted wrong collation: %v", err)
	}
}
