package main

import (
	"path/filepath"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/v4distribution"
)

func TestExecuteWritesNewReportAndValidatesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "distribution.json")
	arguments := []string{"-output", path, "-cardinality", "1000", "-runs", "1", "-cluster-count", "10",
		"-random-seed", "17", "-replay-lookups-per-run", "64", "-max-peak-heap-bytes", "536870912"}
	if err := execute(arguments); err != nil {
		t.Fatal(err)
	}
	report, err := v4distribution.ReadAndValidate(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Cells) != 12 {
		t.Fatalf("cells=%d, want 12", len(report.Cells))
	}
	if err := execute([]string{"-validate", path}); err != nil {
		t.Fatal(err)
	}
	if err := execute(arguments); err == nil {
		t.Fatal("existing evidence file was overwritten")
	}
}

func TestExecuteRequiresExactlyOneMode(t *testing.T) {
	if err := execute(nil); err == nil {
		t.Fatal("missing output was accepted")
	}
	if err := execute([]string{"-output", "x", "-validate", "y"}); err == nil {
		t.Fatal("output plus validate was accepted")
	}
}
