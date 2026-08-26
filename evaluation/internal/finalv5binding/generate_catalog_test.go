package finalv5binding_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	contractfs "taskbound.local/agent-data-gateway/evaluation/final-v5-wsl2"
	"taskbound.local/agent-data-gateway/evaluation/finalv5contracts"
	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5binding"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5publication"
	"taskbound.local/agent-data-gateway/internal/catalog"
)

const observedProbeSHA256 = "0eb905408442997de37ac810683f18c758b614a716c50758312015aeb753d314"

func TestBuildCompleteBindingRejectsApprovedScaleOnlyCatalog(t *testing.T) {
	repositoryRoot := bindingRepositoryRoot(t)
	scaleBytes, err := os.ReadFile(filepath.Join(repositoryRoot, filepath.FromSlash(path.Join(
		path.Dir(finalv5publication.C2CandidateRelativePath), "catalog.yaml"))))
	if err != nil {
		t.Fatal(err)
	}
	scaleOnly, err := catalog.Parse(scaleBytes)
	if err != nil {
		t.Fatal(err)
	}
	contractRuntime, err := finalv5contracts.LoadRuntime()
	if err != nil {
		t.Fatal(err)
	}
	dataset, err := contractRuntime.DatasetIdentitySHA256()
	if err != nil {
		t.Fatal(err)
	}
	input := finalv5binding.CompleteBindingInput{DatasetSHA256: dataset,
		DatasetProbeSHA256: observedProbeSHA256, Catalog: scaleOnly, ArtifactRuntime: contractRuntime}
	if _, _, err := finalv5binding.BuildCompleteBinding(input); err == nil || !strings.Contains(err.Error(), "Artifact") {
		t.Fatalf("Scale-only C2 Catalog rejection = %v, want missing Artifact route", err)
	}
}

func externalTrackedScaleManifests(t *testing.T) []finalv5oracle.ExposureScaleManifestArtifact {
	t.Helper()
	result := make([]finalv5oracle.ExposureScaleManifestArtifact, 0, 24)
	for _, cell := range finalv5oracle.ExposureScaleDependencyCells() {
		for _, mode := range []string{finalv5oracle.ExposureScaleModeNovel,
			finalv5oracle.ExposureScaleModeSemanticReplay} {
			relative, err := finalv5oracle.ExposureScaleDependencyManifestPath(cell.Scale, mode)
			if err != nil {
				t.Fatal(err)
			}
			value, err := contractfs.FS.ReadFile(path.Join("oracle-manifests", relative))
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := finalv5oracle.DecodeManifest(value)
			if err != nil {
				t.Fatal(err)
			}
			result = append(result, finalv5oracle.ExposureScaleManifestArtifact{RelativePath: relative,
				SHA256: externalSHA256(value), Manifest: manifest})
		}
	}
	return result
}

func externalTrackedProvSQLManifests(t *testing.T) []finalv5oracle.ProvSQLManifestArtifact {
	t.Helper()
	result := make([]finalv5oracle.ProvSQLManifestArtifact, 0, 105)
	for _, cell := range finalv5oracle.ProvSQLNonceJoinCells() {
		relative, err := finalv5oracle.ProvSQLNonceJoinManifestPath(cell.Scale, cell.Nonce)
		if err != nil {
			t.Fatal(err)
		}
		value, err := contractfs.FS.ReadFile(path.Join("oracle-manifests", relative))
		if err != nil {
			t.Fatal(err)
		}
		manifest, err := finalv5oracle.DecodeManifest(value)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, finalv5oracle.ProvSQLManifestArtifact{RelativePath: relative,
			SHA256: externalSHA256(value), Manifest: manifest})
	}
	return result
}

func bindingRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate binding integration test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", ".."))
}

func externalSHA256(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
