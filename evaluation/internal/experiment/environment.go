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

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5binding"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5dataset"
)

const (
	finalV5AdapterBindingSHAKey = "final_v5_adapter_sha256"
	datasetBindingFileSHAKey    = "dataset_binding_sha256"
	datasetProbeSQLSHAKey       = "dataset_probe_sql_sha256"
	datasetProbeSHAKey          = "dataset_probe_sha256"
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
	binding, err := finalv5binding.LoadPublicationFile(path, finalv5binding.CatalogPath)
	if err != nil {
		return nil, err
	}
	// Never copy private tasks, scopes, SQL, or exact oracles into the public
	// environment manifest. The frozen source adapter validates those bytes;
	// publication evidence retains only their complete-file and strict-section
	// identities.
	return map[string]any{
		"dataset_sha256":            binding.DatasetSHA256,
		datasetProbeSQLSHAKey:       finalv5binding.DatasetProbeSQLSHA256(),
		datasetProbeSHAKey:          binding.DatasetProbeSHA256,
		"catalog_sha256":            binding.CatalogSHA256,
		finalV5AdapterBindingSHAKey: binding.SectionSHA256,
		datasetBindingFileSHAKey:    binding.FileSHA256,
	}, nil
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
	if len(datasets) != 5 && len(datasets) != 7 {
		return false
	}
	for _, name := range []string{"dataset_sha256", "catalog_sha256", "deployment_volume_id_sha256",
		finalV5AdapterBindingSHAKey, datasetBindingFileSHAKey} {
		value, ok := datasets[name].(string)
		if !ok || !validSHA256(value) {
			return false
		}
	}
	if len(datasets) == 7 {
		for _, name := range []string{datasetProbeSQLSHAKey, datasetProbeSHAKey} {
			value, ok := datasets[name].(string)
			if !ok || !validSHA256(value) {
				return false
			}
		}
	}
	return true
}

func environmentBindingIdentity(manifest EnvironmentManifest) (string, string, error) {
	if !validEnvironmentDatasetBindings(manifest.Datasets) {
		return "", "", errors.New("environment binding identity is absent or invalid")
	}
	section, _ := manifest.Datasets[finalV5AdapterBindingSHAKey].(string)
	file, _ := manifest.Datasets[datasetBindingFileSHAKey].(string)
	return section, file, nil
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
	sectionSHA, sectionOK := bindings[finalV5AdapterBindingSHAKey].(string)
	fileSHA, fileOK := bindings[datasetBindingFileSHAKey].(string)
	if !datasetOK || !catalogOK || !sectionOK || !fileOK || !validSHA256(dataset) ||
		!validSHA256(catalog) || !validSHA256(sectionSHA) || !validSHA256(fileSHA) {
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
	if (proof.SchemaVersion != legacyFreshDeploymentProofSchemaVersion &&
		proof.SchemaVersion != freshDeploymentProofSchemaVersion) ||
		proof.CampaignID != campaignID || proof.DeploymentID != deploymentID ||
		!validSHA256(proof.VolumeSetSHA256) || !validPostgresSystemIdentifier(proof.ControlPGSystemIdentifier) ||
		!validPostgresSystemIdentifier(proof.BusinessPGSystemIdentifier) || proof.ControlPGSystemIdentifier == proof.BusinessPGSystemIdentifier ||
		!validSHA256(proof.DeploymentVolumeIDSHA256) || proof.DeploymentVolumeIDSHA256 != derivedVolume ||
		!validSHA256(proof.CatalogSHA256) || validateFreshDeploymentDatasetIdentity(proof) != nil {
		return nil, errors.New("fresh-deployment proof identity/digests are invalid")
	}
	proofPrefix := strings.TrimSuffix(proofPath, ".json")
	if proofPrefix == proofPath {
		return nil, errors.New("fresh-deployment proof path must end in .json")
	}
	companions := freshDeploymentDatasetCompanions(proof)
	companions[".catalog.yaml"] = proof.CatalogSHA256
	for suffix, expected := range companions {
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
	if proof.SchemaVersion == freshDeploymentProofSchemaVersion {
		if err := validateFreshDeploymentDatasetAgreement(proof,
			proofPrefix+".dataset-identity.json"); err != nil {
			return nil, err
		}
	}
	if dataset != freshDeploymentDatasetSHA256(proof) {
		return nil, errors.New("reviewed typed Dataset identity differs from the fresh-deployment proof")
	}
	if catalog != proof.CatalogSHA256 {
		return nil, errors.New("reviewed Catalog digest differs from the live Gateway Catalog")
	}
	if proof.SchemaVersion == legacyFreshDeploymentProofSchemaVersion {
		if len(bindings) != 4 {
			return nil, errors.New("fresh-deployment proof v1 requires the frozen four-field reviewed binding")
		}
	} else {
		probeSQL, probeSQLOK := bindings[datasetProbeSQLSHAKey].(string)
		probe, probeOK := bindings[datasetProbeSHAKey].(string)
		if len(bindings) != 6 || !probeSQLOK || !probeOK || !validSHA256(probeSQL) || !validSHA256(probe) ||
			probeSQL != proof.DatasetProbeSQLSHA256 || probe != proof.DatasetProbeSHA256 {
			return nil, errors.New("reviewed Dataset sanity-probe identities differ from the fresh-deployment proof")
		}
	}
	bound := make(map[string]any, len(bindings)+1)
	for key, one := range bindings {
		bound[key] = one
	}
	bound["deployment_volume_id_sha256"] = derivedVolume
	return bound, nil
}

func validateFreshDeploymentDatasetIdentity(proof FreshDeploymentProof) error {
	switch proof.SchemaVersion {
	case legacyFreshDeploymentProofSchemaVersion:
		if !validSHA256(proof.DatasetFingerprintSHA256) || proof.DatasetSHA256 != "" ||
			proof.DatasetIdentityEvidenceSHA256 != "" || proof.DatasetProbeSQLSHA256 != "" ||
			proof.DatasetProbeSHA256 != "" {
			return errors.New("fresh-deployment proof v1 Dataset fingerprint is invalid")
		}
	case freshDeploymentProofSchemaVersion:
		if proof.DatasetFingerprintSHA256 != "" || !validSHA256(proof.DatasetSHA256) ||
			!validSHA256(proof.DatasetIdentityEvidenceSHA256) ||
			!validSHA256(proof.DatasetProbeSQLSHA256) || !validSHA256(proof.DatasetProbeSHA256) {
			return errors.New("fresh-deployment proof v2 Dataset/probe identities are invalid")
		}
	default:
		return errors.New("fresh-deployment proof Dataset identity has no versioned semantics")
	}
	return nil
}

func freshDeploymentDatasetSHA256(proof FreshDeploymentProof) string {
	if proof.SchemaVersion == freshDeploymentProofSchemaVersion {
		return proof.DatasetSHA256
	}
	return proof.DatasetFingerprintSHA256
}

func freshDeploymentDatasetCompanions(proof FreshDeploymentProof) map[string]string {
	if proof.SchemaVersion == freshDeploymentProofSchemaVersion {
		return map[string]string{
			".dataset-identity.json": proof.DatasetIdentityEvidenceSHA256,
			".dataset-probe.sql":     proof.DatasetProbeSQLSHA256,
			".dataset-probe.txt":     proof.DatasetProbeSHA256,
		}
	}
	return map[string]string{".dataset-fingerprint.txt": proof.DatasetFingerprintSHA256}
}

func validateFreshDeploymentDatasetAgreement(proof FreshDeploymentProof, agreementPath string) error {
	if proof.SchemaVersion != freshDeploymentProofSchemaVersion {
		return errors.New("full live Dataset agreement is only defined for fresh-deployment proof v2")
	}
	info, err := os.Lstat(agreementPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 1<<20 {
		return errors.New("fresh-deployment full Dataset agreement is missing or unsafe")
	}
	payload, err := os.ReadFile(agreementPath)
	if err != nil {
		return fmt.Errorf("read fresh-deployment full Dataset agreement: %w", err)
	}
	var agreement finalv5dataset.BenchmarkAgreement
	if err := StrictJSON(payload, &agreement); err != nil {
		return fmt.Errorf("decode fresh-deployment full Dataset agreement: %w", err)
	}
	if err := finalv5dataset.ValidateBenchmarkAgreement(agreement); err != nil {
		return fmt.Errorf("validate fresh-deployment full Dataset agreement: %w", err)
	}
	if agreement.Observed.SHA256 != proof.DatasetSHA256 {
		return errors.New("fresh-deployment Dataset digest differs from its full live agreement")
	}
	return nil
}
