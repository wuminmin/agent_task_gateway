package experiment

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var requiredPublicationExperiments = []string{"baseline", "scale", "artifact", "rls", "attack", "provsql", "compiler", "concurrency", "rq5"}

type CampaignExperimentEvidence struct {
	ExperimentID           string `json:"experiment_id"`
	SummarySHA256          string `json:"summary_sha256"`
	EvidenceManifestSHA256 string `json:"evidence_manifest_sha256"`
	AdapterSHA256          string `json:"adapter_sha256"`
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
		adapterBytes, err := os.ReadFile(filepath.Join(runDir, "adapter.sha256"))
		if err != nil {
			return campaign, err
		}
		oneAdapter := string(bytesTrimSpace(adapterBytes))
		if !validSHA256(oneAdapter) {
			return campaign, fmt.Errorf("%s adapter digest invalid", name)
		}
		buildBytes, err := os.ReadFile(filepath.Join(runDir, "adapter-build.json"))
		var build struct {
			SchemaVersion    int    `json:"schema_version"`
			SubmissionCommit string `json:"submission_commit"`
			BinarySHA256     string `json:"binary_sha256"`
			SourceSHA256     string `json:"source_sha256"`
			GoVersion        string `json:"go_version"`
			BuildCommand     string `json:"build_command"`
			SourceFiles      string `json:"source_files"`
		}
		if err != nil || StrictJSON(buildBytes, &build) != nil || build.SchemaVersion != 1 || build.SubmissionCommit != config.SubmissionCommit || build.BinarySHA256 != oneAdapter || !validSHA256(build.SourceSHA256) || sha256Hex([]byte(build.SourceFiles)) != build.SourceSHA256 || build.GoVersion == "" || build.BuildCommand != "go build -trimpath -o final-v5-adapter ./evaluation/cmd/final-v5-adapter" || build.SourceFiles == "" {
			return campaign, fmt.Errorf("%s source adapter build binding invalid", name)
		}
		if binarySHA, binaryErr := FileSHA256(filepath.Join(campaignRoot, "source-adapter", "final-v5-adapter")); binaryErr != nil || binarySHA != oneAdapter {
			return campaign, fmt.Errorf("%s unified adapter binary is missing or differs from its build binding", name)
		}
		if adapterDigest == "" {
			adapterDigest = oneAdapter
		} else if oneAdapter != adapterDigest {
			return campaign, errors.New("experiments were not executed by one frozen unified adapter")
		}
		if campaign.CampaignID == "" {
			campaign = PublicationCampaignSummary{SchemaVersion: 1, CampaignID: config.CampaignID, SubmissionCommit: config.SubmissionCommit, ProtocolVersion: config.ProtocolVersion, ProtocolSHA256: config.ProtocolSHA256, WorkloadSHA256: config.WorkloadSHA256, AcceptanceSHA256: config.AcceptanceSHA256, StatisticsSHA256: config.StatisticsSHA256}
		} else if campaign.CampaignID != config.CampaignID || campaign.SubmissionCommit != config.SubmissionCommit || campaign.ProtocolVersion != config.ProtocolVersion || campaign.ProtocolSHA256 != config.ProtocolSHA256 || campaign.WorkloadSHA256 != config.WorkloadSHA256 || campaign.AcceptanceSHA256 != config.AcceptanceSHA256 || campaign.StatisticsSHA256 != config.StatisticsSHA256 {
			return campaign, errors.New("experiment campaign/protocol bindings differ")
		}
		summarySHA, _ := FileSHA256(filepath.Join(runDir, "generated", "summary.json"))
		manifestSHA, _ := FileSHA256(manifestPath)
		campaign.Experiments = append(campaign.Experiments, CampaignExperimentEvidence{ExperimentID: name, SummarySHA256: summarySHA, EvidenceManifestSHA256: manifestSHA, AdapterSHA256: oneAdapter})
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
