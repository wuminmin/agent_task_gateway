package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
)

func TestScaleManifestInstallIsAtomicClosedAndSiblingSafe(t *testing.T) {
	sourceRoot := filepath.Join("..", "..", "final-v5-wsl2", "oracle-manifests")
	values, err := readScaleManifestSet(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := make([]finalv5oracle.ExposureScaleManifestArtifact, 0, len(values))
	for relative, value := range values {
		manifest, err := finalv5oracle.DecodeManifest(value)
		if err != nil {
			t.Fatal(err)
		}
		digest, err := finalv5oracle.ManifestSHA256(manifest)
		if err != nil {
			t.Fatal(err)
		}
		artifacts = append(artifacts, finalv5oracle.ExposureScaleManifestArtifact{
			RelativePath: relative, SHA256: digest, Manifest: manifest,
		})
	}

	root := t.TempDir()
	sibling := filepath.Join(root, "scale", "outcome-merkle", "future.json")
	if err := os.MkdirAll(filepath.Dir(sibling), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sibling, []byte("future Scale outcome material\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := installScaleManifests(root, artifacts); err != nil {
		t.Fatal(err)
	}
	installed, err := readScaleManifestSet(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(installed) != 24 {
		t.Fatalf("installed %d dependency manifests; expected 24", len(installed))
	}
	for relative, want := range values {
		if !bytes.Equal(installed[relative], want) {
			t.Fatalf("installed manifest %s changed bytes", relative)
		}
	}
	if value, err := os.ReadFile(sibling); err != nil || string(value) != "future Scale outcome material\n" {
		t.Fatalf("Scale sibling workload changed: value=%q err=%v", value, err)
	}

	drifted := filepath.Join(root, filepath.FromSlash("scale/dependency-e2e/10k-overlap-50/novel.json"))
	if err := os.WriteFile(drifted, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := installScaleManifests(root, artifacts); err == nil {
		t.Fatal("installer overwrote a drifted dependency manifest tree")
	}
}

func TestReadScaleManifestSetRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.json")
	if err := os.WriteFile(target, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "scale", "dependency-e2e", "10k-overlap-0", "novel.json")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readScaleManifestSet(root); err == nil {
		t.Fatal("Scale manifest reader accepted a symlink")
	}
}

func TestScaleManifestInstallRejectsIncompleteOrUnboundArtifactsBeforeWriting(t *testing.T) {
	sourceRoot := filepath.Join("..", "..", "final-v5-wsl2", "oracle-manifests")
	values, err := readScaleManifestSet(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := make([]finalv5oracle.ExposureScaleManifestArtifact, 0, len(values))
	for relative, value := range values {
		manifest, err := finalv5oracle.DecodeManifest(value)
		if err != nil {
			t.Fatal(err)
		}
		digest, err := finalv5oracle.ManifestSHA256(manifest)
		if err != nil {
			t.Fatal(err)
		}
		artifacts = append(artifacts, finalv5oracle.ExposureScaleManifestArtifact{
			RelativePath: relative, SHA256: digest, Manifest: manifest,
		})
	}
	cases := map[string][]finalv5oracle.ExposureScaleManifestArtifact{
		"missing": append([]finalv5oracle.ExposureScaleManifestArtifact(nil), artifacts[:23]...),
		"extra":   append(append([]finalv5oracle.ExposureScaleManifestArtifact(nil), artifacts...), artifacts[0]),
	}
	badSHA := append([]finalv5oracle.ExposureScaleManifestArtifact(nil), artifacts...)
	badSHA[0].SHA256 = finalv5oracle.ExposureScaleDatasetSpecSHA256
	cases["bad SHA"] = badSHA
	traversal := append([]finalv5oracle.ExposureScaleManifestArtifact(nil), artifacts...)
	traversal[0].RelativePath = "../outside.json"
	cases["path traversal"] = traversal
	for name, changed := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := installScaleManifests(root, changed); err == nil {
				t.Fatal("installer accepted an incomplete or unbound artifact batch")
			}
			if _, err := os.Stat(filepath.Join(root, "scale", "dependency-e2e")); !os.IsNotExist(err) {
				t.Fatalf("failed validation left a Scale target behind: %v", err)
			}
		})
	}
}
