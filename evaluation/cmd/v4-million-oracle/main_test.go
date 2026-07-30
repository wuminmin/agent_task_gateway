package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteExclusiveAtomicNeverOverwritesEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	if err := writeExclusiveAtomic(path, []byte("first\n")); err != nil {
		t.Fatal(err)
	}
	if err := writeExclusiveAtomic(path, []byte("second\n")); err == nil {
		t.Fatal("evidence report was overwritten")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "first\n" {
		t.Fatalf("report changed to %q", raw)
	}
}
