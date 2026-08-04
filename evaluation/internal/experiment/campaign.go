package experiment

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var requiredPublicationExperiments = []string{"baseline", "scale", "artifact", "rls", "attack", "provsql", "compiler", "concurrency", "rq5"}

const (
	sourceAdapterBuildCommand = "go build -buildvcs=false -trimpath -o final-v5-adapter ./evaluation/cmd/final-v5-adapter"
	observerBuildCommand      = "go build -buildvcs=false -trimpath -o final-v5-observer ./evaluation/cmd/final-v5-observer"
	rq5DriverBuildCommand     = "go build -buildvcs=false -trimpath -o rq5-sequential-driver ./evaluation/cmd/rq5-sequential-driver"
)

var rq5RequiredRuntimeSources = []string{
	"Dockerfile",
	"evaluation/daily-publication/Dockerfile",
	"evaluation/daily-publication/config.json",
	"evaluation/daily-publication/compose.yaml",
	"evaluation/daily-publication/harness.py",
	"evaluation/daily-publication/run.sh",
	"evaluation/daily-publication/sql/05-generate-daily-data.sh",
	"evaluation/daily-publication/sql/10-reader.sh",
	"evaluation/daily-publication/sql/dataset-manifest.sql",
	"evaluation/daily-publication-online/Dockerfile",
	"evaluation/daily-publication-online/compose.yaml",
	"evaluation/daily-publication-online/run.sh",
	"evaluation/daily-publication-online/validate.py",
	"evaluation/daily-publication-online/sql/10-online-runtime.sh",
	"evaluation/daily-publication-online/sql/20-clone-retained-databases.sh",
	"db/init/00-schema.sql",
}

// observerRequiredSources is the exact source set the observer binary is built
// from and bound to. It must name every file whose bytes change what a snapshot
// means, not merely the entry point: the formal-window contract decides which
// deployments may be measured at all, so a build that silently dropped it would
// still verify against this manifest.
var observerRequiredSources = []string{
	"evaluation/cmd/final-v5-observer/main.go",
	"evaluation/internal/experiment/observer.go",
	"evaluation/internal/experiment/formal_window.go",
}

type CampaignExperimentEvidence struct {
	ExperimentID           string `json:"experiment_id"`
	SummarySHA256          string `json:"summary_sha256"`
	EvidenceManifestSHA256 string `json:"evidence_manifest_sha256"`
	AdapterSHA256          string `json:"adapter_sha256"`
	ObserverSHA256         string `json:"observer_sha256"`
	RQ5DriverSHA256        string `json:"rq5_driver_sha256,omitempty"`
}

type sourceBuildBinding struct {
	SchemaVersion    int    `json:"schema_version"`
	SubmissionCommit string `json:"submission_commit"`
	BinarySHA256     string `json:"binary_sha256"`
	SourceSHA256     string `json:"source_sha256"`
	GoVersion        string `json:"go_version"`
	BuildCommand     string `json:"build_command"`
	SourceFiles      string `json:"source_files"`
}

type PublicationCampaignSummary struct {
	SchemaVersion    int                          `json:"schema_version"`
	CampaignID       string                       `json:"campaign_id"`
	SubmissionCommit string                       `json:"submission_commit"`
	ProtocolVersion  string                       `json:"protocol_version"`
	ProtocolSHA256   string                       `json:"protocol_sha256"`
	WorkloadSHA256   string                       `json:"workload_manifest_sha256"`
	AcceptanceSHA256 string                       `json:"acceptance_rules_sha256"`
	StatisticsSHA256 string                       `json:"statistics_sha256"`
	Status           string                       `json:"publication_campaign_status"`
	Experiments      []CampaignExperimentEvidence `json:"experiments"`
}

func FinalizePublicationCampaign(campaignRoot string) (PublicationCampaignSummary, error) {
	var campaign PublicationCampaignSummary
	adapterDigest := ""
	observerDigest := ""
	adapterBindingDigest := ""
	datasetBindingDigest := ""
	environmentDigests := make(map[string]string)
	for _, name := range requiredPublicationExperiments {
		runDir := filepath.Join(campaignRoot, name)
		config, _, err := LoadConfig(filepath.Join(runDir, "config.json"), name)
		if err != nil {
			return campaign, fmt.Errorf("%s config: %w", name, err)
		}
		summaryBytes, err := os.ReadFile(filepath.Join(runDir, "generated", "summary.json"))
		if err != nil {
			return campaign, fmt.Errorf("%s summary: %w", name, err)
		}
		var summary Summary
		if err := StrictJSON(summaryBytes, &summary); err != nil || summary.Status != "pass" || !summary.PublicationEligible {
			return campaign, fmt.Errorf("%s is not a passing sealed publication experiment", name)
		}
		manifestPath := filepath.Join(runDir, "evidence", "manifest.json")
		manifestBytes, err := os.ReadFile(manifestPath)
		if err != nil {
			return campaign, fmt.Errorf("%s evidence manifest: %w", name, err)
		}
		var manifest EvidenceManifest
		if err := StrictJSON(manifestBytes, &manifest); err != nil {
			return campaign, fmt.Errorf("%s evidence manifest: %w", name, err)
		}
		if manifest.CampaignID != config.CampaignID || manifest.SubmissionCommit != config.SubmissionCommit {
			return campaign, fmt.Errorf("%s evidence identity mismatch", name)
		}
		if err := verifyEvidenceManifest(runDir, manifest); err != nil {
			return campaign, fmt.Errorf("%s sealed evidence: %w", name, err)
		}
		for deployment := 1; deployment <= config.Deployments; deployment++ {
			deploymentID := fmt.Sprintf("deployment-%02d", deployment)
			relative := filepath.ToSlash(filepath.Join("environment", deploymentID+".json"))
			digest, ok := evidenceManifestFileSHA256(manifest, relative)
			if !ok {
				return campaign, fmt.Errorf("%s sealed evidence omits %s", name, relative)
			}
			if err := bindCampaignEnvironmentDigest(environmentDigests, deploymentID, digest); err != nil {
				return campaign, err
			}
			environmentBytes, err := os.ReadFile(filepath.Join(runDir, "environment", deploymentID+".json"))
			var environment EnvironmentManifest
			if err != nil || StrictJSON(environmentBytes, &environment) != nil {
				return campaign, fmt.Errorf("%s environment binding is unreadable: %s", name, deploymentID)
			}
			sectionSHA, fileSHA, err := environmentBindingIdentity(environment)
			if err != nil {
				return campaign, fmt.Errorf("%s environment binding is invalid: %s", name, deploymentID)
			}
			if adapterBindingDigest == "" {
				adapterBindingDigest, datasetBindingDigest = sectionSHA, fileSHA
			} else if sectionSHA != adapterBindingDigest || fileSHA != datasetBindingDigest {
				return campaign, errors.New("private dataset/adapter binding changed across deployments or experiments")
			}
		}
		oneAdapter, err := verifySourceBuildBinding(
			filepath.Join(runDir, "adapter.sha256"), filepath.Join(runDir, "adapter-build.json"),
			filepath.Join(campaignRoot, "source-adapter", "final-v5-adapter"),
			config.SubmissionCommit, sourceAdapterBuildCommand, nil)
		if err != nil {
			return campaign, fmt.Errorf("%s source adapter build binding invalid: %w", name, err)
		}
		if adapterDigest == "" {
			adapterDigest = oneAdapter
		} else if oneAdapter != adapterDigest {
			return campaign, errors.New("experiments were not executed by one frozen unified adapter")
		}
		oneObserver, err := verifySourceBuildBinding(
			filepath.Join(runDir, "observer.sha256"), filepath.Join(runDir, "observer-build.json"),
			filepath.Join(campaignRoot, "source-adapter", "final-v5-observer"),
			config.SubmissionCommit, observerBuildCommand, observerRequiredSources)
		if err != nil {
			return campaign, fmt.Errorf("%s source observer build binding invalid: %w", name, err)
		}
		for _, relative := range []string{"observer.sha256", "observer-build.json"} {
			sealedSHA, present := evidenceManifestFileSHA256(manifest, relative)
			actualSHA, hashErr := FileSHA256(filepath.Join(runDir, relative))
			if !present || hashErr != nil || sealedSHA != actualSHA {
				return campaign, fmt.Errorf("%s sealed evidence omits or changes %s", name, relative)
			}
		}
		if observerDigest == "" {
			observerDigest = oneObserver
		} else if oneObserver != observerDigest {
			return campaign, errors.New("experiments were not observed by one frozen source-built observer")
		}
		if campaign.CampaignID == "" {
			campaign = PublicationCampaignSummary{SchemaVersion: 1, CampaignID: config.CampaignID, SubmissionCommit: config.SubmissionCommit, ProtocolVersion: config.ProtocolVersion, ProtocolSHA256: config.ProtocolSHA256, WorkloadSHA256: config.WorkloadSHA256, AcceptanceSHA256: config.AcceptanceSHA256, StatisticsSHA256: config.StatisticsSHA256}
		} else if campaign.CampaignID != config.CampaignID || campaign.SubmissionCommit != config.SubmissionCommit || campaign.ProtocolVersion != config.ProtocolVersion || campaign.ProtocolSHA256 != config.ProtocolSHA256 || campaign.WorkloadSHA256 != config.WorkloadSHA256 || campaign.AcceptanceSHA256 != config.AcceptanceSHA256 || campaign.StatisticsSHA256 != config.StatisticsSHA256 {
			return campaign, errors.New("experiment campaign/protocol bindings differ")
		}
		rq5DriverDigest := ""
		if name == "rq5" {
			rq5DriverDigest, err = verifySourceBuildBinding(
				filepath.Join(runDir, "rq5-driver.sha256"), filepath.Join(runDir, "rq5-driver-build.json"),
				filepath.Join(campaignRoot, "source-adapter", "rq5-sequential-driver"),
				config.SubmissionCommit, rq5DriverBuildCommand, rq5RequiredRuntimeSources)
			if err != nil {
				return campaign, fmt.Errorf("RQ5 source driver build binding invalid: %w", err)
			}
			rq5ManifestSHA256, err := loadRQ5DriverBuildManifestSHA256(runDir)
			if err != nil {
				return campaign, fmt.Errorf("RQ5 driver build-manifest identity invalid: %w", err)
			}
			rawPaths, err := filepath.Glob(filepath.Join(runDir, "raw", "*.jsonl"))
			if err != nil || len(rawPaths) == 0 {
				return campaign, errors.New("RQ5 raw evidence is absent")
			}
			rq5Samples, err := ReadSamples(rawPaths)
			if err != nil {
				return campaign, fmt.Errorf("RQ5 raw evidence: %w", err)
			}
			if reasons := validateRQ5RuntimeIdentityConsistency(rq5Samples, rq5ManifestSHA256); len(reasons) != 0 {
				return campaign, errors.New(reasons[0])
			}
		}
		summarySHA, _ := FileSHA256(filepath.Join(runDir, "generated", "summary.json"))
		manifestSHA, _ := FileSHA256(manifestPath)
		campaign.Experiments = append(campaign.Experiments, CampaignExperimentEvidence{ExperimentID: name,
			SummarySHA256: summarySHA, EvidenceManifestSHA256: manifestSHA, AdapterSHA256: oneAdapter,
			ObserverSHA256: oneObserver, RQ5DriverSHA256: rq5DriverDigest})
	}
	campaign.Status = "pass"
	encoded, _ := json.MarshalIndent(campaign, "", "  ")
	outputDir := filepath.Join(campaignRoot, "campaign-generated")
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return campaign, err
	}
	if err := writeExclusive(filepath.Join(outputDir, "publication-campaign.json"), append(encoded, '\n')); err != nil {
		return campaign, err
	}
	return campaign, nil
}

func bindCampaignEnvironmentDigest(digests map[string]string, deploymentID, digest string) error {
	if digests == nil || deploymentID == "" || !validSHA256(digest) {
		return errors.New("campaign environment digest binding is invalid")
	}
	if expected := digests[deploymentID]; expected == "" {
		digests[deploymentID] = digest
	} else if digest != expected {
		return fmt.Errorf("deployment environment changed across experiments: %s", deploymentID)
	}
	return nil
}

func evidenceManifestFileSHA256(manifest EvidenceManifest, path string) (string, bool) {
	digest := ""
	for _, file := range manifest.Files {
		if file.Path != path {
			continue
		}
		if digest != "" || !validSHA256(file.SHA256) {
			return "", false
		}
		digest = file.SHA256
	}
	return digest, digest != ""
}

func verifySourceBuildBinding(digestPath, manifestPath, binaryPath, submissionCommit,
	expectedCommand string, requiredSources []string) (string, error) {
	digestInfo, err := os.Lstat(digestPath)
	if err != nil || !digestInfo.Mode().IsRegular() || digestInfo.Mode()&os.ModeSymlink != 0 || digestInfo.Mode().Perm()&0o022 != 0 {
		return "", errors.New("binary digest file is absent, unsafe, or group/world writable")
	}
	digestBytes, err := os.ReadFile(digestPath)
	if err != nil {
		return "", err
	}
	digest := string(bytesTrimSpace(digestBytes))
	if !validSHA256(digest) {
		return "", errors.New("binary digest is invalid")
	}
	return VerifySourceBuildManifest(manifestPath, binaryPath, digest, submissionCommit, expectedCommand, requiredSources)
}

// VerifySourceBuildManifest is shared by the pre-start adapter runtime gate
// and campaign finalizer so an observer cannot pass early under a weaker
// self-signed manifest contract.
func VerifySourceBuildManifest(manifestPath, binaryPath, digest, submissionCommit,
	expectedCommand string, requiredSources []string) (string, error) {
	if !validSHA256(digest) {
		return "", errors.New("binary digest is invalid")
	}
	manifestInfo, err := os.Lstat(manifestPath)
	if err != nil || !manifestInfo.Mode().IsRegular() || manifestInfo.Mode()&os.ModeSymlink != 0 || manifestInfo.Mode().Perm()&0o022 != 0 {
		return "", errors.New("build manifest is absent, unsafe, or group/world writable")
	}
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", err
	}
	var build sourceBuildBinding
	if err := StrictJSON(manifestBytes, &build); err != nil {
		return "", err
	}
	if build.SchemaVersion != 1 || build.SubmissionCommit != submissionCommit ||
		build.BinarySHA256 != digest || !validSHA256(build.SourceSHA256) ||
		sha256Hex([]byte(build.SourceFiles)) != build.SourceSHA256 || build.GoVersion == "" ||
		build.BuildCommand != expectedCommand || build.SourceFiles == "" {
		return "", errors.New("build manifest differs from its source/binary/commit contract")
	}
	if err := validateBoundSourceListing(build.SourceFiles, requiredSources); err != nil {
		return "", err
	}
	info, err := os.Lstat(binaryPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 || info.Mode()&0o111 == 0 {
		return "", errors.New("bound binary is absent, non-executable, or a symlink")
	}
	actual, err := FileSHA256(binaryPath)
	if err != nil || actual != digest {
		return "", errors.New("bound binary differs from its recorded digest")
	}
	return digest, nil
}

func validateBoundSourceListing(listing string, required []string) error {
	present := make(map[string]bool)
	for _, line := range bytesSplitLines([]byte(listing)) {
		if len(line) < 67 || line[64] != ' ' || line[65] != ' ' || !validSHA256(string(line[:64])) {
			return errors.New("source listing contains a malformed digest/path record")
		}
		path := string(line[66:])
		if path == "" || filepath.IsAbs(path) || filepath.Clean(path) != path || path == ".." ||
			len(path) >= 3 && path[:3] == ".."+string(filepath.Separator) || present[path] {
			return errors.New("source listing contains an unsafe or duplicate path")
		}
		present[path] = true
	}
	for _, path := range required {
		if !present[path] {
			return fmt.Errorf("source listing omits required runtime input %s", path)
		}
	}
	return nil
}

func bytesSplitLines(value []byte) [][]byte {
	var result [][]byte
	for len(value) > 0 {
		line := value
		if index := bytesIndexByte(value, '\n'); index >= 0 {
			line, value = value[:index], value[index+1:]
		} else {
			value = nil
		}
		if len(line) != 0 {
			result = append(result, line)
		}
	}
	return result
}

func bytesIndexByte(value []byte, wanted byte) int {
	for index, one := range value {
		if one == wanted {
			return index
		}
	}
	return -1
}

func verifyEvidenceManifest(root string, manifest EvidenceManifest) error {
	if manifest.SchemaVersion != 1 || !validSHA256(manifest.ManifestSHA256) || len(manifest.Files) == 0 {
		return errors.New("invalid evidence manifest")
	}
	for _, file := range manifest.Files {
		path := filepath.Join(root, filepath.FromSlash(file.Path))
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == ".." || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator) {
			return errors.New("evidence file escapes run directory")
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() != file.Bytes {
			return errors.New("sealed evidence file missing or resized")
		}
		digest, err := FileSHA256(path)
		if err != nil || digest != file.SHA256 {
			return errors.New("sealed evidence file digest mismatch")
		}
	}
	value, _ := json.Marshal(manifest.Files)
	if sha256Hex(value) != manifest.ManifestSHA256 {
		return errors.New("evidence manifest self-digest mismatch")
	}
	return nil
}

func bytesTrimSpace(value []byte) []byte {
	start, end := 0, len(value)
	for start < end && (value[start] == ' ' || value[start] == '\n' || value[start] == '\r' || value[start] == '\t') {
		start++
	}
	for end > start && (value[end-1] == ' ' || value[end-1] == '\n' || value[end-1] == '\r' || value[end-1] == '\t') {
		end--
	}
	return value[start:end]
}
