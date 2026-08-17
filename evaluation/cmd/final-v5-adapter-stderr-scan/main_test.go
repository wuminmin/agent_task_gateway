package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadSensitiveJSONValuesCollectsPrivateScalarsAndRejectsOpenFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "secrets.json")
	if err := os.WriteFile(path, []byte(`{"first":"one","nested":{"second":"two"},"empty":"","number":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	values, err := readSensitiveJSONValues(path)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, value := range values {
		seen[value] = true
	}
	if len(values) != 2 || !seen["one"] || !seen["two"] {
		t.Fatalf("sensitive values = %#v", values)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readSensitiveJSONValues(path); err == nil {
		t.Fatal("group/world-readable sensitive JSON was accepted")
	}
}
