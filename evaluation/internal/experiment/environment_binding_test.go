package experiment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestBindPublicationDatasetsInjectsProofDerivedVolumeIdentity(t *testing.T) {
	proofPath, proof := writePublicationBindingProof(t, "binding-campaign", "deployment-01")
	bindings := map[string]any{
		"dataset_sha256": proof.DatasetFingerprintSHA256,
		"catalog_sha256": proof.CatalogSHA256,
	}

	bound, err := BindPublicationDatasets(bindings, proofPath, proof.CampaignID, proof.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	if got := bound["deployment_volume_id_sha256"]; got != proof.DeploymentVolumeIDSHA256 {
		t.Fatalf("bound deployment volume identity = %v, want %s", got, proof.DeploymentVolumeIDSHA256)
	}
	if _, supplied := bindings["deployment_volume_id_sha256"]; supplied {
		t.Fatal("binding input was mutated with a proof-owned deployment volume identity")
	}
	if got := deriveDeploymentVolumeID(proof); got != proof.DeploymentVolumeIDSHA256 {
		t.Fatalf("derived deployment volume identity = %s, want %s", got, proof.DeploymentVolumeIDSHA256)
	}
}

func TestBindPublicationDatasetsRejectsAuthorSuppliedVolumeIdentity(t *testing.T) {
	proofPath, proof := writePublicationBindingProof(t, "binding-campaign", "deployment-01")
	bindings := map[string]any{
		"dataset_sha256":              proof.DatasetFingerprintSHA256,
		"catalog_sha256":              proof.CatalogSHA256,
		"deployment_volume_id_sha256": proof.DeploymentVolumeIDSHA256,
	}

	if _, err := BindPublicationDatasets(bindings, proofPath, proof.CampaignID, proof.DeploymentID); err == nil {
		t.Fatal("author-supplied deployment volume identity was accepted")
	}
}

func TestBindPublicationDatasetsRejectsDatasetAndCatalogMismatch(t *testing.T) {
	proofPath, proof := writePublicationBindingProof(t, "binding-campaign", "deployment-01")
	tests := []struct {
		name    string
		dataset string
		catalog string
	}{
		{name: "dataset", dataset: strings.Repeat("e", 64), catalog: proof.CatalogSHA256},
		{name: "catalog", dataset: proof.DatasetFingerprintSHA256, catalog: strings.Repeat("f", 64)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bindings := map[string]any{"dataset_sha256": test.dataset, "catalog_sha256": test.catalog}
			if _, err := BindPublicationDatasets(bindings, proofPath, proof.CampaignID, proof.DeploymentID); err == nil {
				t.Fatalf("%s digest mismatch was accepted", test.name)
			}
		})
	}
}

func TestBindPublicationDatasetsRejectsProofIdentityMismatch(t *testing.T) {
	proofPath, proof := writePublicationBindingProof(t, "binding-campaign", "deployment-01")
	bindings := map[string]any{
		"dataset_sha256": proof.DatasetFingerprintSHA256,
		"catalog_sha256": proof.CatalogSHA256,
	}
	tests := []struct {
		name       string
		campaignID string
		deployment string
	}{
		{name: "campaign", campaignID: "other-campaign", deployment: proof.DeploymentID},
		{name: "deployment", campaignID: proof.CampaignID, deployment: "deployment-02"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := BindPublicationDatasets(bindings, proofPath, test.campaignID, test.deployment); err == nil {
				t.Fatalf("fresh proof with mismatched %s identity was accepted", test.name)
			}
		})
	}
}

func TestWriteEnvironmentRejectsMissingDeploymentVolumeIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "environment.json")
	manifest := EnvironmentManifest{
		SchemaVersion:       1,
		CampaignID:          "binding-campaign",
		DeploymentID:        "deployment-01",
		CapturedAt:          "2026-08-03T00:00:00Z",
		GitCommit:           strings.Repeat("a", 40),
		GitStatus:           []string{},
		PublicationEligible: true,
		Host:                map[string]any{"os": "test"},
		Software:            map[string]any{"go": "test"},
		Storage:             map[string]any{"fs": "test"},
		Datasets: map[string]any{
			"dataset_sha256": strings.Repeat("b", 64),
			"catalog_sha256": strings.Repeat("c", 64),
		},
	}
	if err := WriteEnvironment(path, manifest); err == nil {
		t.Fatal("environment lacking deployment_volume_id_sha256 was written")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("invalid environment left an output file: %v", err)
	}
}

func writePublicationBindingProof(t *testing.T, campaignID, deploymentID string) (string, FreshDeploymentProof) {
	t.Helper()
	environmentDir := filepath.Join(t.TempDir(), "environment")
	if err := os.MkdirAll(environmentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(environmentDir, deploymentID+".fresh")

	datasetBytes := []byte("frozen-publication-dataset")
	datasetPath := prefix + ".dataset-fingerprint.txt"
	if err := os.WriteFile(datasetPath, datasetBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	datasetSHA := bindingTestSHA256(datasetBytes)
	catalogBytes := []byte("catalog: frozen-publication-catalog\n")
	if err := os.WriteFile(prefix+".catalog.yaml", catalogBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	composeBytes := []byte("name: binding-fixture\n")
	composePath := prefix + ".compose-config.yaml"
	if err := os.WriteFile(composePath, composeBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	composeSHA := bindingTestSHA256(composeBytes)

	volumes := make([]DockerVolumeProof, 5)
	inspectObjects := make([]map[string]any, len(volumes))
	volumeSetLines := make([]string, 0, len(volumes))
	for index := range volumes {
		name := fmt.Sprintf("binding-volume-%d", index)
		createdAt := fmt.Sprintf("2026-08-03T00:00:%02dZ", index)
		object := map[string]any{"Name": name, "CreatedAt": createdAt, "Driver": "local"}
		canonical, err := json.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		volumes[index] = DockerVolumeProof{
			Name:          name,
			CreatedAt:     createdAt,
			Driver:        "local",
			InspectSHA256: bindingTestSHA256(canonical),
		}
		inspectObjects[index] = object
		volumeSetLines = append(volumeSetLines, strings.Join([]string{name, createdAt, "local", volumes[index].InspectSHA256}, "\t")+"\n")
	}
	sort.Strings(volumeSetLines)
	volumeSetSHA := bindingTestSHA256([]byte(strings.Join(volumeSetLines, "")))
	inspectBytes, err := json.MarshalIndent(inspectObjects, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	inspectBytes = append(inspectBytes, '\n')
	if err := os.WriteFile(prefix+".volume-inspect.json", inspectBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	proof := FreshDeploymentProof{
		SchemaVersion:                1,
		CampaignID:                   campaignID,
		DeploymentID:                 deploymentID,
		CapturedAt:                   "2026-08-03T00:00:00Z",
		ComposeProjectName:           "binding-fixture-project",
		ComposeConfigSHA256:          composeSHA,
		Volumes:                      volumes,
		VolumeSetSHA256:              volumeSetSHA,
		VolumeInspectSHA256:          bindingTestSHA256(inspectBytes),
		ControlPGSystemIdentifier:    "1000000000000000001",
		BusinessPGSystemIdentifier:   "2000000000000000001",
		ControlInitialCounts:         map[string]int64{"tasks": 0, "query_records": 0, "root_heads": 0, "result_artifacts": 0},
		DatasetFingerprintSHA256:     datasetSHA,
		CatalogSHA256:                bindingTestSHA256(catalogBytes),
		MinIOInitialObjectCount:      0,
		SnapshotArtifactVolumeSHA256: bindingTestSHA256([]byte("snapshot-artifact-volume")),
	}
	proof.DeploymentVolumeIDSHA256 = expectedBindingDeploymentVolumeID(proof)
	proofBytes, err := json.MarshalIndent(proof, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	proofPath := prefix + ".json"
	if err := os.WriteFile(proofPath, append(proofBytes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return proofPath, proof
}

func expectedBindingDeploymentVolumeID(proof FreshDeploymentProof) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("TASKGATE-FINAL-V5-DEPLOYMENT-VOLUME-ID-V1"))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(proof.VolumeSetSHA256))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(proof.ControlPGSystemIdentifier))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(proof.BusinessPGSystemIdentifier))
	return hex.EncodeToString(hash.Sum(nil))
}

func bindingTestSHA256(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
