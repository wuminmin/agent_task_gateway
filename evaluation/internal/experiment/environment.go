package experiment

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type EnvironmentManifest struct {
	SchemaVersion       int            `json:"schema_version"`
	CampaignID          string         `json:"campaign_id"`
	DeploymentID        string         `json:"deployment_id"`
	CapturedAt          string         `json:"captured_at"`
	GitCommit           string         `json:"git_commit"`
	GitStatus           []string       `json:"git_status"`
	PublicationEligible bool           `json:"publication_eligible"`
	Host                map[string]any `json:"host"`
	Software            map[string]any `json:"software"`
	Storage             map[string]any `json:"storage"`
	Datasets            map[string]any `json:"datasets"`
}

func RecordEnvironment(repo, campaignID, deploymentID string, eligible bool, datasets map[string]any) (EnvironmentManifest, error) {
	if repo == "" || campaignID == "" || deploymentID == "" {
		return EnvironmentManifest{}, errors.New("repo, campaign, and deployment are required")
	}
	run := func(name string, args ...string) (string, error) {
		command := exec.Command(name, args...)
		command.Dir = repo
		value, err := command.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("record environment command %s: %w", name, err)
		}
		return strings.TrimSpace(string(value)), nil
	}
	commit, err := run("git", "rev-parse", "HEAD")
	if err != nil {
		return EnvironmentManifest{}, err
	}
	statusText, err := run("git", "status", "--short")
	if err != nil {
		return EnvironmentManifest{}, err
	}
	var status []string
	if statusText != "" {
		status = strings.Split(statusText, "\n")
	}
	manifest := EnvironmentManifest{SchemaVersion: 1, CampaignID: campaignID, DeploymentID: deploymentID, CapturedAt: time.Now().UTC().Format(time.RFC3339Nano), GitCommit: commit, GitStatus: status, PublicationEligible: eligible, Datasets: datasets}
	capture := func(target map[string]any, key, name string, args ...string) error {
		value, captureErr := run(name, args...)
		if captureErr != nil {
			return captureErr
		}
		if value == "" {
			return fmt.Errorf("record environment command %s returned empty output", name)
		}
		target[key] = value
		return nil
	}
	manifest.Host = map[string]any{"goos": runtime.GOOS, "goarch": runtime.GOARCH}
	for _, command := range []struct {
		key, name string
		args      []string
	}{
		{"os_release", "cat", []string{"/etc/os-release"}}, {"uname", "uname", []string{"-a"}},
		{"lscpu", "lscpu", nil}, {"nproc", "nproc", nil}, {"memory", "free", []string{"-b"}},
		{"meminfo", "cat", []string{"/proc/meminfo"}}, {"vmstat", "cat", []string{"/proc/vmstat"}},
	} {
		if err := capture(manifest.Host, command.key, command.name, command.args...); err != nil {
			return EnvironmentManifest{}, err
		}
	}
	manifest.Software = map[string]any{}
	for _, command := range []struct {
		key, name string
		args      []string
	}{
		{"go", "go", []string{"version"}}, {"postgresql", "psql", []string{"--version"}},
		{"docker_engine", "docker", []string{"version", "--format", "{{json .}}"}}, {"docker_compose", "docker", []string{"compose", "version"}},
		{"docker_info", "docker", []string{"info", "--format", "{{json .}}"}}, {"containerd", "containerd", []string{"--version"}},
		{"image_ids", "docker", []string{"image", "ls", "--no-trunc", "--digests", "--format", "{{.Repository}} {{.Tag}} {{.Digest}} {{.ID}}"}},
	} {
		if err := capture(manifest.Software, command.key, command.name, command.args...); err != nil {
			return EnvironmentManifest{}, err
		}
	}
	manifest.Storage = map[string]any{}
	for _, command := range []struct {
		key, name string
		args      []string
	}{
		{"df", "df", []string{"-T"}}, {"lsblk", "lsblk", []string{"-f"}}, {"cgroup_filesystem", "stat", []string{"-fc", "%T", "/sys/fs/cgroup"}},
	} {
		if err := capture(manifest.Storage, command.key, command.name, command.args...); err != nil {
			return EnvironmentManifest{}, err
		}
	}
	return manifest, nil
}

func WriteEnvironment(path string, manifest EnvironmentManifest) error {
	if manifest.SchemaVersion != 1 || manifest.CampaignID == "" || manifest.DeploymentID == "" ||
		manifest.CapturedAt == "" || manifest.GitCommit == "" ||
		(manifest.PublicationEligible && !validEnvironmentDatasetBindings(manifest.Datasets)) {
		return errors.New("invalid environment manifest")
	}
	value, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return writeExclusive(path, append(value, '\n'))
}

func WriteDeployment(path string, manifest DeploymentManifest) error {
	if manifest.SchemaVersion != 1 || manifest.CampaignID == "" || manifest.DeploymentID == "" ||
		!manifest.FreshDeployment || !validSHA256(manifest.FreshDeploymentProofSHA256) || !validSHA256(manifest.EnvironmentSHA256) || !validSHA256(manifest.WindowsEnvironmentSHA256) || !validSHA256(manifest.VMStatBeforeSHA256) || !validSHA256(manifest.VMStatAfterSHA256) || manifest.StartedAt == "" || manifest.FinishedAt == "" ||
		manifest.SwapInDelta < 0 || manifest.SwapOutDelta < 0 || manifest.UnexpectedContainerRestarts < 0 {
		return errors.New("invalid deployment manifest")
	}
	value, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return writeExclusive(path, append(value, '\n'))
}

func ReadDatasetBindings(path string) (map[string]any, error) {
	if path == "" {
		return map[string]any{}, nil
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 4<<20 {
		return nil, errors.New("dataset binding must be a bounded regular file")
	}
	value, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var decoded map[string]any
	if err := StrictJSON(value, &decoded); err != nil {
		return nil, err
	}
	redacted, ok := RedactSecrets(decoded).(map[string]any)
	if !ok {
		return nil, errors.New("dataset binding is not an object")
	}
	return redacted, nil
}

const deploymentVolumeIdentityDomain = "TASKGATE-FINAL-V5-DEPLOYMENT-VOLUME-ID-V1"

func deriveDeploymentVolumeID(proof FreshDeploymentProof) string {
	return sha256Hex([]byte(strings.Join([]string{
		deploymentVolumeIdentityDomain,
		proof.VolumeSetSHA256,
		proof.ControlPGSystemIdentifier,
		proof.BusinessPGSystemIdentifier,
	}, "\x00")))
}

func validPostgresSystemIdentifier(value string) bool {
	identifier, err := strconv.ParseUint(value, 10, 64)
	return err == nil && identifier != 0
}

func validEnvironmentDatasetBindings(datasets map[string]any) bool {
	if len(datasets) == 0 {
		return false
	}
	for _, name := range []string{"dataset_sha256", "catalog_sha256", "deployment_volume_id_sha256"} {
		value, ok := datasets[name].(string)
		if !ok || !validSHA256(value) {
			return false
		}
	}
	return true
}

// BindPublicationDatasets turns the reviewed dataset/Catalog declarations into
// an environment binding only after comparing them with independently captured
// live deployment evidence. Deployment volume identity is never accepted from
// author input; it is derived from the fresh Compose/PostgreSQL identities.
func BindPublicationDatasets(bindings map[string]any, proofPath, campaignID, deploymentID string) (map[string]any, error) {
	if len(bindings) == 0 || proofPath == "" || campaignID == "" || deploymentID == "" {
		return nil, errors.New("publication dataset bindings and fresh-deployment proof are required")
	}
	if _, supplied := bindings["deployment_volume_id_sha256"]; supplied {
		return nil, errors.New("deployment_volume_id_sha256 must be derived from fresh-deployment proof")
	}
	dataset, datasetOK := bindings["dataset_sha256"].(string)
	catalog, catalogOK := bindings["catalog_sha256"].(string)
	if !datasetOK || !catalogOK || !validSHA256(dataset) || !validSHA256(catalog) {
		return nil, errors.New("publication dataset/Catalog bindings are missing or invalid")
	}
	info, err := os.Lstat(proofPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 1<<20 {
		return nil, errors.New("fresh-deployment proof must be a bounded regular file")
	}
	value, err := os.ReadFile(proofPath)
	if err != nil {
		return nil, err
	}
	var proof FreshDeploymentProof
	if err := StrictJSON(value, &proof); err != nil {
		return nil, fmt.Errorf("decode fresh-deployment proof: %w", err)
	}
	derivedVolume := deriveDeploymentVolumeID(proof)
	if proof.SchemaVersion != 1 || proof.CampaignID != campaignID || proof.DeploymentID != deploymentID ||
		!validSHA256(proof.VolumeSetSHA256) || !validPostgresSystemIdentifier(proof.ControlPGSystemIdentifier) ||
		!validPostgresSystemIdentifier(proof.BusinessPGSystemIdentifier) || proof.ControlPGSystemIdentifier == proof.BusinessPGSystemIdentifier ||
		!validSHA256(proof.DeploymentVolumeIDSHA256) || proof.DeploymentVolumeIDSHA256 != derivedVolume ||
		!validSHA256(proof.DatasetFingerprintSHA256) || !validSHA256(proof.CatalogSHA256) {
		return nil, errors.New("fresh-deployment proof identity/digests are invalid")
	}
	proofPrefix := strings.TrimSuffix(proofPath, ".json")
	if proofPrefix == proofPath {
		return nil, errors.New("fresh-deployment proof path must end in .json")
	}
	for suffix, expected := range map[string]string{
		".dataset-fingerprint.txt": proof.DatasetFingerprintSHA256,
		".catalog.yaml":            proof.CatalogSHA256,
	} {
		companionPath := proofPrefix + suffix
		companionInfo, statErr := os.Lstat(companionPath)
		if statErr != nil || !companionInfo.Mode().IsRegular() || companionInfo.Mode()&os.ModeSymlink != 0 || companionInfo.Size() > 16<<20 {
			return nil, errors.New("fresh-deployment digest companion is missing or unsafe")
		}
		digest, digestErr := FileSHA256(companionPath)
		if digestErr != nil || digest != expected {
			return nil, errors.New("fresh-deployment digest companion differs from proof")
		}
	}
	if dataset != proof.DatasetFingerprintSHA256 {
		return nil, errors.New("reviewed dataset digest differs from the live dataset fingerprint")
	}
	if catalog != proof.CatalogSHA256 {
		return nil, errors.New("reviewed Catalog digest differs from the live Gateway Catalog")
	}
	bound := make(map[string]any, len(bindings)+1)
	for key, one := range bindings {
		bound[key] = one
	}
	bound["deployment_volume_id_sha256"] = derivedVolume
	return bound, nil
}
