package main

import (
	"bytes"
	"testing"
)

func TestRunRejectsOpenOrCredentialBearingCLIInputs(t *testing.T) {
	for _, arguments := range [][]string{
		nil,
		{"unknown"},
		{"generate"},
		{"generate", "--output-dir", "candidate", "--artifact-dir", "artifacts", "--dsn", "secret"},
		{"validate"},
		{"validate", "--input-dir", "candidate", "extra"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(arguments, &stdout, &stderr); code != 2 {
			t.Fatalf("run(%q) = %d, want usage failure 2 (stderr=%q)", arguments, code, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("run(%q) wrote stdout on usage failure", arguments)
		}
	}
}
