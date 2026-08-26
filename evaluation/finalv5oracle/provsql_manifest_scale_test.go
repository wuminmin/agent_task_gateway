//go:build taskgate_scale

// This case regenerates and validates all 105 ProvSQL nonce-join manifests.
// It carries a multi-minute budget and never finished inside the acceptance
// run's per-package limit, so it belongs on the taskgate_scale lane the
// repository already reserves for costly scale work.

package finalv5oracle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestTrackedProvSQLNonceJoinManifestsRegenerateAndValidateAll105Cells(t *testing.T) {
	manifestRoot := filepath.Join("..", "final-v5-wsl2", "oracle-manifests")
	values := make(map[string][]byte, 105)
	err := filepath.WalkDir(filepath.Join(manifestRoot, "provsql"), func(current string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return os.ErrInvalid
		}
		relative, err := filepath.Rel(manifestRoot, current)
		if err != nil {
			return err
		}
		value, err := os.ReadFile(current)
		if err != nil {
			return err
		}
		values[filepath.ToSlash(relative)] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := VerifyProvSQLNonceJoinManifestSet(values, StreamSetOptions{
		MaxInMemoryMembers: 64 * 1024, CaptureMembers: 2, TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 105 {
		t.Fatalf("generated %d ProvSQL manifests; want 105", len(artifacts))
	}
	validateSchema := oracleManifestSchemaValidator(t)
	paths, digests := make(map[string]bool, 105), make(map[string]bool, 105)
	dependencies, releases := make(map[string]bool, 105), make(map[string]bool, 105)
	wantCardinality := map[string]int64{"1k": 29_003, "10k": 290_003, "45k": 1_305_003}
	wantLimit := map[string]int64{"1k": 1_000, "10k": 10_000, "45k": 45_000}
	for _, artifact := range artifacts {
		var instance any
		if err := json.Unmarshal(values[artifact.RelativePath], &instance); err != nil {
			t.Fatal(err)
		}
		if err := validateSchema(instance); err != nil {
			t.Fatalf("schema rejected %s: %v", artifact.RelativePath, err)
		}
		dependency := artifact.Manifest.Expected.DependencyCandidateSetSHA256
		release := artifact.Manifest.Expected.ReleaseCandidateSetSHA256
		if paths[artifact.RelativePath] || digests[artifact.SHA256] || dependencies[dependency] || releases[release] ||
			!validSHA256(artifact.SHA256) || !validSHA256(dependency) || !validSHA256(release) {
			t.Fatalf("duplicate/invalid manifest artifact %+v", artifact)
		}
		paths[artifact.RelativePath], digests[artifact.SHA256] = true, true
		dependencies[dependency], releases[release] = true, true
		cell, err := ParseProvSQLBindingKey(artifact.Manifest.BindingKey)
		if err != nil {
			t.Fatal(err)
		}
		expected := artifact.Manifest.Expected
		if expected.RowCount == nil || *expected.RowCount != 3 ||
			expected.ColumnCount == nil || *expected.ColumnCount != 4 ||
			expected.ReleaseCandidateCardinality == nil || *expected.ReleaseCandidateCardinality != 12 ||
			cell.Limit != wantLimit[cell.Scale] || expected.DependencyCandidateCardinality == nil ||
			*expected.DependencyCandidateCardinality != wantCardinality[cell.Scale] ||
			expected.OutcomeCandidateCardinality != nil || expected.OutcomeCandidateSetSHA256 != "" {
			t.Fatalf("%s has inconsistent expectations: %+v", artifact.RelativePath, expected)
		}
	}
}
