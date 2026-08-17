package finalv5profile

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5publication"
)

const ProfileCampaignEvidenceVersion = "taskgate-final-v5-profile-campaign-evidence-v1"

const profileCampaignSampleV1Record = "taskgate-final-v5-profile-campaign-sample-v1"

type CampaignEvidenceFile struct {
	Kind       string `json:"kind"`
	Experiment string `json:"experiment,omitempty"`
	Path       string `json:"path"`
	SHA256     string `json:"sha256"`
	Bytes      int64  `json:"bytes"`
}

type ProfileCampaignDeploymentRecord struct {
	SchemaVersion       int                    `json:"schema_version"`
	CampaignID          string                 `json:"campaign_id"`
	CampaignClass       string                 `json:"campaign_class"`
	PublicationEligible bool                   `json:"publication_eligible"`
	FormalCampaign      bool                   `json:"formal_campaign"`
	SubmissionCommit    string                 `json:"submission_commit"`
	ComposeProject      string                 `json:"compose_project"`
	ProfileID           string                 `json:"profile_id"`
	ProfileAlias        string                 `json:"profile_alias"`
	CatalogPath         string                 `json:"catalog_path"`
	CatalogSHA256       string                 `json:"catalog_sha256"`
	Repetition          int                    `json:"repetition"`
	Cells               []string               `json:"cells"`
	Files               []CampaignEvidenceFile `json:"files"`
}

type ProfileCampaignEvidence struct {
	SchemaVersion       int                               `json:"schema_version"`
	Record              string                            `json:"record"`
	Status              string                            `json:"status"`
	CampaignID          string                            `json:"campaign_id"`
	CampaignClass       string                            `json:"campaign_class"`
	PublicationEligible bool                              `json:"publication_eligible"`
	FormalCampaign      bool                              `json:"formal_campaign"`
	CompleteMatrix      bool                              `json:"complete_matrix"`
	SubmissionCommit    string                            `json:"submission_commit"`
	Repetitions         int                               `json:"repetitions"`
	PlanSHA256          string                            `json:"plan_sha256"`
	ProfileAliases      []string                          `json:"profile_aliases"`
	Deployments         []ProfileCampaignDeploymentRecord `json:"deployments"`
}

func MergeProfileCampaignEvidence(plan CampaignPlan, planSHA, root, campaignID, commit string,
	repetitions int, aliases []string, recordPaths []string) (ProfileCampaignEvidence, error) {
	if !validCampaignDigest(planSHA) || !validCampaignCommit(commit) || repetitions < 1 || campaignID == "" {
		return ProfileCampaignEvidence{}, errors.New("invalid campaign evidence identity")
	}
	planned := make(map[string]PlannedDeploy, len(plan.Deployments))
	for _, deployment := range plan.Deployments {
		planned[deployment.Alias] = deployment
	}
	if len(aliases) == 0 {
		for alias := range planned {
			aliases = append(aliases, alias)
		}
	}
	sort.Strings(aliases)
	for index, alias := range aliases {
		if index > 0 && aliases[index-1] == alias {
			return ProfileCampaignEvidence{}, fmt.Errorf("profile alias %q is duplicated", alias)
		}
		deployment, present := planned[alias]
		if !present || !deployment.Ready {
			return ProfileCampaignEvidence{}, fmt.Errorf("profile alias %q is absent or not ready", alias)
		}
	}
	if len(recordPaths) != len(aliases)*repetitions {
		return ProfileCampaignEvidence{}, fmt.Errorf("deployment records=%d, want %d profiles x repetitions",
			len(recordPaths), len(aliases)*repetitions)
	}
	selected := make(map[string]bool, len(aliases))
	for _, alias := range aliases {
		selected[alias] = true
	}
	seen := map[string]bool{}
	records := make([]ProfileCampaignDeploymentRecord, 0, len(recordPaths))
	for _, recordPath := range recordPaths {
		record, err := readCampaignDeploymentRecord(recordPath)
		if err != nil {
			return ProfileCampaignEvidence{}, err
		}
		deployment, present := planned[record.ProfileAlias]
		if !present || !selected[record.ProfileAlias] {
			return ProfileCampaignEvidence{}, fmt.Errorf("record selects unrequested profile %q", record.ProfileAlias)
		}
		key := fmt.Sprintf("%s/%03d", record.ProfileAlias, record.Repetition)
		if seen[key] || record.Repetition < 1 || record.Repetition > repetitions {
			return ProfileCampaignEvidence{}, fmt.Errorf("invalid or duplicate deployment record %s", key)
		}
		seen[key] = true
		if err := validateCampaignDeploymentRecord(root, campaignID, commit, deployment, record); err != nil {
			return ProfileCampaignEvidence{}, fmt.Errorf("deployment %s: %w", key, err)
		}
		records = append(records, record)
	}
	for _, alias := range aliases {
		for repetition := 1; repetition <= repetitions; repetition++ {
			if !seen[fmt.Sprintf("%s/%03d", alias, repetition)] {
				return ProfileCampaignEvidence{}, fmt.Errorf("deployment %s/%03d is missing", alias, repetition)
			}
		}
	}
	sort.Slice(records, func(left, right int) bool {
		if records[left].ProfileAlias == records[right].ProfileAlias {
			return records[left].Repetition < records[right].Repetition
		}
		return records[left].ProfileAlias < records[right].ProfileAlias
	})
	return ProfileCampaignEvidence{SchemaVersion: 1, Record: ProfileCampaignEvidenceVersion, Status: "pass",
		CampaignID: campaignID, CampaignClass: "pilot", PublicationEligible: false, FormalCampaign: false,
		CompleteMatrix: len(aliases) == len(plan.Deployments), SubmissionCommit: commit, Repetitions: repetitions,
		PlanSHA256: planSHA, ProfileAliases: aliases, Deployments: records}, nil
}

func readCampaignDeploymentRecord(path string) (ProfileCampaignDeploymentRecord, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return ProfileCampaignDeploymentRecord{}, fmt.Errorf("read deployment record: %w", err)
	}
	var record ProfileCampaignDeploymentRecord
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return ProfileCampaignDeploymentRecord{}, fmt.Errorf("decode deployment record: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ProfileCampaignDeploymentRecord{}, errors.New("deployment record has trailing JSON")
	}
	return record, nil
}

func validateCampaignDeploymentRecord(root, campaignID, commit string, planned PlannedDeploy,
	record ProfileCampaignDeploymentRecord) error {
	if record.SchemaVersion != 1 || record.CampaignID != campaignID || record.CampaignClass != "pilot" ||
		record.PublicationEligible || record.FormalCampaign || record.SubmissionCommit != commit ||
		record.ComposeProject == "" || record.ProfileID != planned.ProfileID || record.ProfileAlias != planned.Alias ||
		record.CatalogPath != planned.CatalogPath || record.CatalogSHA256 != planned.CatalogSHA256 {
		return errors.New("deployment identity differs from the fixed campaign plan")
	}
	wantCells := append([]string(nil), planned.Cells...)
	gotCells := append([]string(nil), record.Cells...)
	sort.Strings(wantCells)
	sort.Strings(gotCells)
	if !reflect.DeepEqual(gotCells, wantCells) {
		return errors.New("deployment cells differ from its profile partition")
	}
	kinds := map[string]int{}
	rawByExperiment := map[string]CampaignEvidenceFile{}
	plannedExperiments := make(map[string]bool, len(planned.Experiments))
	for _, experiment := range planned.Experiments {
		plannedExperiments[experiment] = true
	}
	for _, file := range record.Files {
		if err := validateCampaignEvidenceFile(root, file); err != nil {
			return err
		}
		switch file.Kind {
		case "profile_binding", "activation_evidence", "environment", "fresh_proof", "gateway_image", "cleanup",
			"deployment_configuration":
			if file.Experiment != "" {
				return fmt.Errorf("deployment evidence %s must not name an experiment", file.Kind)
			}
		case "config", "selected_cells", "raw_jsonl", "adapter_stderr", "adapter_stderr_credential_scan":
			if !plannedExperiments[file.Experiment] {
				return fmt.Errorf("deployment evidence %s names unplanned experiment %q", file.Kind, file.Experiment)
			}
		case "runner_selected_cells", "rq5_cell_translation", "rq5_cell_map", "rq5_profile_binding",
			"rq5_catalog_family", "rq5_build_manifest":
			if file.Experiment != "rq5" || !plannedExperiments["rq5"] {
				return fmt.Errorf("deployment evidence %s is only valid for a planned RQ5 experiment", file.Kind)
			}
		default:
			return fmt.Errorf("deployment evidence has unknown kind %q", file.Kind)
		}
		key := file.Kind + "/" + file.Experiment
		kinds[key]++
		if file.Kind == "raw_jsonl" {
			rawByExperiment[file.Experiment] = file
		}
	}
	for _, kind := range []string{"profile_binding", "activation_evidence", "environment",
		"fresh_proof", "gateway_image", "cleanup", "deployment_configuration"} {
		if kinds[kind+"/"] != 1 {
			return fmt.Errorf("evidence kind %s count=%d, want 1", kind, kinds[kind+"/"])
		}
	}
	for _, experiment := range planned.Experiments {
		if kinds["config/"+experiment] != 1 || kinds["selected_cells/"+experiment] != 1 ||
			kinds["raw_jsonl/"+experiment] != 1 || kinds["adapter_stderr/"+experiment] != 1 ||
			kinds["adapter_stderr_credential_scan/"+experiment] != 1 {
			return fmt.Errorf("experiment %s lacks one config/selected-cells/raw/stderr/credential-scan set", experiment)
		}
		if err := validateCampaignConfig(root, campaignID, commit, experiment, record.Files); err != nil {
			return err
		}
		if err := validateSelectedCells(root, experiment, planned, record.Files); err != nil {
			return err
		}
		if err := validateAdapterStderrEvidence(root, experiment, record.Files); err != nil {
			return err
		}
	}
	var rq5Mapping *RQ5WorkloadCellMap
	rq5CatalogFamilySHA := ""
	if plannedExperiments["rq5"] {
		for _, kind := range []string{"runner_selected_cells", "rq5_cell_translation", "rq5_cell_map",
			"rq5_profile_binding", "rq5_catalog_family", "rq5_build_manifest"} {
			if kinds[kind+"/rq5"] != 1 {
				return fmt.Errorf("RQ5 evidence kind %s count=%d, want 1", kind, kinds[kind+"/rq5"])
			}
		}
		mapping, err := validateRQ5CoordinateEvidence(root, planned, record.Files)
		if err != nil {
			return err
		}
		rq5Mapping = &mapping
	}
	if err := validateProfileBinding(root, planned, record.Files); err != nil {
		return err
	}
	if err := validateProfileDeploymentConfig(root, planned, record.Files); err != nil {
		return err
	}
	if plannedExperiments["rq5"] {
		var err error
		rq5CatalogFamilySHA, err = validateRQ5CatalogFamilyBinding(root, record.SubmissionCommit, planned, record.Files)
		if err != nil {
			return err
		}
	}
	if err := validateActivationEvidence(root, planned, record.Files); err != nil {
		return err
	}
	if err := validateEnvironmentAndImage(root, commit, record.Files); err != nil {
		return err
	}
	observed := map[string]bool{}
	for experiment, file := range rawByExperiment {
		options := rq5CampaignJSONLOptions{}
		if experiment == "rq5" {
			options.Mapping, options.CatalogSHA256 = rq5Mapping, rq5CatalogFamilySHA
		}
		if err := validateCampaignJSONL(filepath.Join(root, file.Path), campaignID, experiment, planned, observed, options); err != nil {
			return err
		}
	}
	if len(observed) != len(wantCells) {
		return fmt.Errorf("observed cells=%d, want %d", len(observed), len(wantCells))
	}
	return validateCleanup(root, record.Files)
}

func validateCampaignEvidenceFile(root string, file CampaignEvidenceFile) error {
	allowEmpty := file.Kind == "adapter_stderr"
	if file.Kind == "" || file.Path == "" || filepath.IsAbs(file.Path) || !validCampaignDigest(file.SHA256) ||
		file.Bytes < 0 || (file.Bytes == 0 && !allowEmpty) {
		return errors.New("invalid campaign evidence file reference")
	}
	abs := filepath.Clean(filepath.Join(root, file.Path))
	relative, err := filepath.Rel(root, abs)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("campaign evidence file escapes campaign root")
	}
	info, err := os.Lstat(abs)
	if err != nil || !info.Mode().IsRegular() || info.Size() != file.Bytes {
		return fmt.Errorf("campaign evidence file is absent, unsafe, or changed: %s", file.Path)
	}
	payload, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(payload)
	if hex.EncodeToString(digest[:]) != file.SHA256 {
		return fmt.Errorf("campaign evidence digest changed: %s", file.Path)
	}
	return nil
}

func validateProfileDeploymentConfig(root string, planned PlannedDeploy, files []CampaignEvidenceFile) error {
	path := campaignEvidencePath(root, files, "deployment_configuration")
	var config ProfileDeploymentConfig
	if err := decodeStrictCampaignFile(path, &config); err != nil {
		return fmt.Errorf("profile deployment configuration: %w", err)
	}
	if config.SchemaVersion != 1 || config.Record != ProfileDeploymentConfigVersion ||
		config.ProfileAlias != planned.Alias || !validCampaignDigest(config.SourceSHA256) ||
		config.SourcePath == "" || filepath.IsAbs(config.SourcePath) || filepath.Clean(config.SourcePath) != config.SourcePath {
		return errors.New("profile deployment configuration identity is invalid")
	}
	sourcePath := filepath.Clean(filepath.Join(root, config.SourcePath))
	relative, err := filepath.Rel(root, sourcePath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("profile deployment configuration source escapes campaign root")
	}
	payload, err := readRegularProfileFile(sourcePath)
	if err != nil {
		return fmt.Errorf("profile deployment configuration source: %w", err)
	}
	var overrides ProfileDeploymentOverrides
	if err := decodeProfileDeploymentJSON(payload, &overrides); err != nil {
		return fmt.Errorf("decode retained profile deployment configuration source: %w", err)
	}
	if err := validateClosedProfileDeploymentOverrides(overrides); err != nil {
		return fmt.Errorf("retained profile deployment configuration source: %w", err)
	}
	digest := sha256.Sum256(payload)
	if hex.EncodeToString(digest[:]) != config.SourceSHA256 {
		return errors.New("profile deployment configuration source digest changed")
	}
	if planned.Alias == concurrencyDeploymentProfile {
		if err := validateProfileDeploymentEnvironment(config.Environment); err != nil {
			return fmt.Errorf("concurrency profile deployment configuration: %w", err)
		}
		return nil
	}
	if len(config.Environment) != 0 {
		return errors.New("ordinary profile unexpectedly carries a deployment override")
	}
	return nil
}

func validateAdapterStderrEvidence(root, experiment string, files []CampaignEvidenceFile) error {
	stderrFile, err := campaignExperimentEvidenceFile(files, "adapter_stderr", experiment)
	if err != nil {
		return err
	}
	scanFile, err := campaignExperimentEvidenceFile(files, "adapter_stderr_credential_scan", experiment)
	if err != nil {
		return err
	}
	stderrPath := filepath.Join(root, stderrFile.Path)
	info, err := os.Stat(stderrPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		return errors.New("adapter stderr is absent or not mode 0600")
	}
	var scan finalv5publication.AdapterStderrCredentialScan
	if err := decodeStrictCampaignFile(filepath.Join(root, scanFile.Path), &scan); err != nil {
		return fmt.Errorf("adapter stderr credential scan: %w", err)
	}
	if scan.SchemaVersion != 1 || scan.Record != finalv5publication.AdapterStderrCredentialScanVersion ||
		scan.Status != "pass" || scan.InputSHA256 != stderrFile.SHA256 || scan.InputBytes != stderrFile.Bytes ||
		scan.SensitiveValuesChecked < 1 || scan.URLUserinfoHits != 0 || scan.PEMMarkerHits != 0 ||
		scan.SecretAssignmentHits != 0 || scan.JSONScalarExactHits != 0 || scan.ExactValueSubstringHits != 0 {
		return errors.New("adapter stderr credential scan is incomplete or differs from the retained stderr")
	}
	return nil
}

func campaignEvidencePath(root string, files []CampaignEvidenceFile, kind string) string {
	for _, file := range files {
		if file.Kind == kind && file.Experiment == "" {
			return filepath.Join(root, file.Path)
		}
	}
	return ""
}

func decodeCampaignFile(path string, target any) error {
	payload, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("file has trailing JSON")
	}
	return nil
}

func validateCampaignConfig(root, campaignID, commit, experiment string, files []CampaignEvidenceFile) error {
	var config struct {
		SchemaVersion      int               `json:"schema_version"`
		CampaignClass      string            `json:"campaign_class"`
		PilotKind          string            `json:"pilot_kind"`
		CampaignID         string            `json:"campaign_id"`
		SubmissionCommit   string            `json:"submission_commit"`
		Deployments        int               `json:"deployments"`
		Warmups            int               `json:"warmups"`
		Samples            int               `json:"samples"`
		RandomSeed         int64             `json:"random_seed"`
		FreshRootPerSample bool              `json:"fresh_root_per_sample"`
		ExperimentID       string            `json:"experiment_id"`
		Workloads          []json.RawMessage `json:"workloads"`
		ProtocolVersion    string            `json:"protocol_version,omitempty"`
		ProtocolProfile    string            `json:"protocol_profile,omitempty"`
		ProtocolSHA256     string            `json:"protocol_sha256,omitempty"`
		WorkloadSHA256     string            `json:"workload_manifest_sha256,omitempty"`
		AcceptanceSHA256   string            `json:"acceptance_rules_sha256,omitempty"`
		StatisticsSHA256   string            `json:"statistics_sha256,omitempty"`
		ProcessReplicates  int               `json:"process_replicates,omitempty"`
		KernelOnly         bool              `json:"kernel_only,omitempty"`
	}
	path := ""
	for _, file := range files {
		if file.Kind == "config" && file.Experiment == experiment {
			path = filepath.Join(root, file.Path)
		}
	}
	if err := decodeCampaignFile(path, &config); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if config.SchemaVersion != 1 || config.CampaignClass != "pilot" || config.PilotKind != "real_system" ||
		config.CampaignID != campaignID || config.SubmissionCommit != commit || config.Deployments != 1 ||
		config.ExperimentID != experiment || config.Warmups != 0 || config.Samples != 1 || !config.FreshRootPerSample {
		return errors.New("pilot config differs from the mechanism-smoke contract")
	}
	return nil
}

func validateProfileBinding(root string, planned PlannedDeploy, files []CampaignEvidenceFile) error {
	var binding profileBindingWire
	if err := decodeCampaignFile(campaignEvidencePath(root, files, "profile_binding"), &binding); err != nil {
		return fmt.Errorf("profile binding: %w", err)
	}
	if binding.Version != "taskgate-final-v5-profile-binding-v1" || binding.ProfileID != planned.ProfileID ||
		binding.CatalogSHA256 != planned.CatalogSHA256 {
		return errors.New("ProfileBinding differs from the planned profile")
	}
	return nil
}

type profileBindingWire struct {
	Version              string `json:"version"`
	ProfileID            string `json:"profile_id"`
	CatalogSHA256        string `json:"catalog_sha256"`
	ClosureSHA256        string `json:"closure_sha256"`
	DatasetBindingSHA256 string `json:"dataset_binding_sha256"`
	PublicationIdentity  string `json:"publication_identity"`
}

func validateRQ5CatalogFamilyBinding(root, commit string, planned PlannedDeploy,
	files []CampaignEvidenceFile) (string, error) {
	outerPath := campaignEvidencePath(root, files, "profile_binding")
	var outer profileBindingWire
	if err := decodeStrictCampaignFile(outerPath, &outer); err != nil {
		return "", fmt.Errorf("outer profile binding: %w", err)
	}
	dynamicFile, err := campaignExperimentEvidenceFile(files, "rq5_profile_binding", "rq5")
	if err != nil {
		return "", err
	}
	var dynamic profileBindingWire
	if err := decodeStrictCampaignFile(filepath.Join(root, dynamicFile.Path), &dynamic); err != nil {
		return "", fmt.Errorf("RQ5 profile binding: %w", err)
	}
	familyFile, err := campaignExperimentEvidenceFile(files, "rq5_catalog_family", "rq5")
	if err != nil {
		return "", err
	}
	manifestFile, err := campaignExperimentEvidenceFile(files, "rq5_build_manifest", "rq5")
	if err != nil {
		return "", err
	}
	family, err := ResolveRQ5CatalogFamilyIdentity(filepath.Join(root, familyFile.Path),
		filepath.Join(root, manifestFile.Path), manifestFile.SHA256,
		RQ5CatalogFamilyOwner{ProfileID: planned.ProfileID, ProfileAlias: planned.Alias,
			ClosureSHA256: outer.ClosureSHA256, WorkloadCells: planned.Cells})
	if err != nil {
		return "", err
	}
	publicationIdentity, err := CanonicalPublicationSetSHA256(family.PublicationNames)
	if err != nil {
		return "", err
	}
	if family.SubmissionCommit != commit || dynamic.Version != outer.Version ||
		dynamic.ProfileID != planned.ProfileID || dynamic.ClosureSHA256 != outer.ClosureSHA256 ||
		dynamic.DatasetBindingSHA256 != outer.DatasetBindingSHA256 || dynamic.CatalogSHA256 != family.FamilySHA256 ||
		dynamic.PublicationIdentity != publicationIdentity {
		return "", errors.New("RQ5 ProfileBinding differs from its source-controlled Catalog family")
	}
	return family.FamilySHA256, nil
}

func validateActivationEvidence(root string, planned PlannedDeploy, files []CampaignEvidenceFile) error {
	var evidence struct {
		Record                string `json:"record"`
		Status                string `json:"status"`
		ProfileID             string `json:"profile_id"`
		CatalogSHA256         string `json:"catalog_sha256"`
		ActivationSmokePassed bool   `json:"activation_smoke_passed"`
	}
	if err := decodeCampaignFile(campaignEvidencePath(root, files, "activation_evidence"), &evidence); err != nil {
		return fmt.Errorf("activation evidence: %w", err)
	}
	if evidence.Record != ActivationEvidenceVersion || evidence.Status != "pass" ||
		!evidence.ActivationSmokePassed || evidence.ProfileID != planned.ProfileID ||
		evidence.CatalogSHA256 != planned.CatalogSHA256 {
		return errors.New("activation evidence differs from the planned profile")
	}
	return nil
}

func validateEnvironmentAndImage(root, commit string, files []CampaignEvidenceFile) error {
	var environment struct {
		GitCommit           string   `json:"git_commit"`
		GitStatus           []string `json:"git_status"`
		PublicationEligible bool     `json:"publication_eligible"`
	}
	if err := decodeCampaignFile(campaignEvidencePath(root, files, "environment"), &environment); err != nil {
		return fmt.Errorf("environment: %w", err)
	}
	if environment.GitCommit != commit || len(environment.GitStatus) != 0 || environment.PublicationEligible {
		return errors.New("environment is not clean, fixed-commit pilot evidence")
	}
	var image struct {
		RecordKind          string `json:"record_kind"`
		ExperimentClass     string `json:"experiment_class"`
		ProvenanceAssertion string `json:"provenance_assertion"`
		ContainerImageID    string `json:"container_image_id"`
		ImageID             string `json:"image_id"`
	}
	if err := decodeCampaignFile(campaignEvidencePath(root, files, "gateway_image"), &image); err != nil {
		return fmt.Errorf("Gateway image: %w", err)
	}
	if image.RecordKind != "taskgate-pilot-gateway-image-observation-v1" || image.ExperimentClass != "pilot" ||
		image.ProvenanceAssertion != "observation_only_not_publication_verification" ||
		image.ContainerImageID == "" || image.ContainerImageID != image.ImageID {
		return errors.New("Gateway image observation is incomplete")
	}
	return nil
}

func validateSelectedCells(root, experiment string, planned PlannedDeploy, files []CampaignEvidenceFile) error {
	path := ""
	for _, file := range files {
		if file.Kind == "selected_cells" && file.Experiment == experiment {
			path = filepath.Join(root, file.Path)
		}
	}
	var selected []string
	if err := decodeCampaignFile(path, &selected); err != nil {
		return fmt.Errorf("selected cells: %w", err)
	}
	want := make([]string, 0, len(planned.Cells))
	for _, cell := range planned.Cells {
		if strings.HasPrefix(cell, experiment+"/") {
			want = append(want, cell)
		}
	}
	sort.Strings(want)
	sort.Strings(selected)
	if !reflect.DeepEqual(selected, want) {
		return fmt.Errorf("selected cells for %s differ from the campaign plan", experiment)
	}
	return nil
}

func validateRQ5CoordinateEvidence(root string, planned PlannedDeploy,
	files []CampaignEvidenceFile) (RQ5WorkloadCellMap, error) {
	mapFile, err := campaignExperimentEvidenceFile(files, "rq5_cell_map", "rq5")
	if err != nil {
		return RQ5WorkloadCellMap{}, err
	}
	mapping, mappingSHA, err := LoadRQ5WorkloadCellMap(filepath.Join(root, mapFile.Path))
	if err != nil {
		return RQ5WorkloadCellMap{}, err
	}
	if mappingSHA != mapFile.SHA256 {
		return RQ5WorkloadCellMap{}, errors.New("retained RQ5 cell map digest differs from its evidence reference")
	}
	translationFile, err := campaignExperimentEvidenceFile(files, "rq5_cell_translation", "rq5")
	if err != nil {
		return RQ5WorkloadCellMap{}, err
	}
	var translation RQ5CellTranslationEvidence
	if err := decodeStrictCampaignFile(filepath.Join(root, translationFile.Path), &translation); err != nil {
		return RQ5WorkloadCellMap{}, fmt.Errorf("RQ5 cell translation: %w", err)
	}
	if translation.MappingRetainedPath != mapFile.Path {
		return RQ5WorkloadCellMap{}, errors.New("RQ5 cell translation names a different retained map path")
	}
	if err := translation.Validate(mapping, mappingSHA); err != nil {
		return RQ5WorkloadCellMap{}, err
	}
	campaignFile, err := campaignExperimentEvidenceFile(files, "selected_cells", "rq5")
	if err != nil {
		return RQ5WorkloadCellMap{}, err
	}
	runnerFile, err := campaignExperimentEvidenceFile(files, "runner_selected_cells", "rq5")
	if err != nil {
		return RQ5WorkloadCellMap{}, err
	}
	var campaignSelected, runnerSelected []string
	if err := decodeStrictCampaignFile(filepath.Join(root, campaignFile.Path), &campaignSelected); err != nil {
		return RQ5WorkloadCellMap{}, fmt.Errorf("RQ5 campaign selected cells: %w", err)
	}
	if err := decodeStrictCampaignFile(filepath.Join(root, runnerFile.Path), &runnerSelected); err != nil {
		return RQ5WorkloadCellMap{}, fmt.Errorf("RQ5 runner selected cells: %w", err)
	}
	if !equalSortedStrings(campaignSelected, translation.CampaignCells) ||
		!equalSortedStrings(runnerSelected, translation.ExperimentCells) {
		return RQ5WorkloadCellMap{}, errors.New("RQ5 retained selected-cell forms differ from the translation evidence")
	}
	wantCampaign := make([]string, 0, len(planned.Cells))
	for _, cell := range planned.Cells {
		if strings.HasPrefix(cell, "rq5/") {
			wantCampaign = append(wantCampaign, cell)
		}
	}
	if !equalSortedStrings(campaignSelected, wantCampaign) {
		return RQ5WorkloadCellMap{}, errors.New("RQ5 campaign coordinates differ from the planned profile partition")
	}
	return mapping, nil
}

func campaignExperimentEvidenceFile(files []CampaignEvidenceFile, kind, experiment string) (CampaignEvidenceFile, error) {
	var matched CampaignEvidenceFile
	count := 0
	for _, file := range files {
		if file.Kind == kind && file.Experiment == experiment {
			matched = file
			count++
		}
	}
	if count != 1 {
		return CampaignEvidenceFile{}, fmt.Errorf("evidence kind %s/%s count=%d, want 1", kind, experiment, count)
	}
	return matched, nil
}

func decodeStrictCampaignFile(path string, target any) error {
	payload, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("file has trailing JSON")
	}
	return nil
}

type rq5CampaignJSONLOptions struct {
	Mapping       *RQ5WorkloadCellMap
	CatalogSHA256 string
}

func validateCampaignJSONL(path, campaignID, experiment string, planned PlannedDeploy, observed map[string]bool,
	rq5Options ...rq5CampaignJSONLOptions) error {
	options := rq5CampaignJSONLOptions{}
	if len(rq5Options) > 1 {
		return errors.New("multiple RQ5 JSONL validation options supplied")
	}
	if len(rq5Options) == 1 {
		options = rq5Options[0]
	}
	rq5Mapping := options.Mapping
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	allowed := map[string]bool{}
	for _, cell := range planned.Cells {
		if strings.HasPrefix(cell, experiment+"/") {
			allowed[cell] = true
		}
	}
	if experiment == "rq5" {
		if rq5Mapping == nil || !digestPattern.MatchString(options.CatalogSHA256) {
			return errors.New("RQ5 JSONL validation requires the explicit coordinate map")
		}
		campaign := make([]string, 0, len(allowed))
		for cell := range allowed {
			campaign = append(campaign, cell)
		}
		translated, err := rq5Mapping.TranslateCampaignCells(campaign)
		if err != nil {
			return err
		}
		allowed = make(map[string]bool, len(translated))
		for _, cell := range translated {
			allowed[cell] = true
		}
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	lines := 0
	for scanner.Scan() {
		lines++
		sample, campaignClass, err := decodeCampaignJSONLLine(scanner.Bytes())
		if err != nil {
			return fmt.Errorf("decode JSONL line %d: %w", lines, err)
		}
		identity := sample.ExperimentID + "/" + sample.CellID
		expectedCatalog := planned.CatalogSHA256
		if experiment == "rq5" {
			expectedCatalog = options.CatalogSHA256
		}
		if sample.CampaignID != campaignID || campaignClass != "pilot" ||
			sample.DeploymentID != "deployment-01" || sample.ExperimentID != experiment ||
			sample.Status != "pass" || sample.PublicationEligible || !allowed[identity] ||
			sample.ProfileBinding == nil || sample.ProfileBinding.ProfileID != planned.ProfileID ||
			sample.ProfileBinding.CatalogSHA256 != expectedCatalog {
			return fmt.Errorf("JSONL line %d differs from its profile assignment", lines)
		}
		observedIdentity := identity
		if experiment == "rq5" {
			campaign, err := rq5Mapping.TranslateExperimentCells([]string{identity})
			if err != nil {
				return fmt.Errorf("JSONL line %d: %w", lines, err)
			}
			observedIdentity = campaign[0]
		}
		if observed[observedIdentity] {
			return fmt.Errorf("JSONL cell %s is duplicated", observedIdentity)
		}
		observed[observedIdentity] = true
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if lines == 0 {
		return errors.New("raw JSONL is empty")
	}
	return nil
}

type campaignJSONLSample struct {
	CampaignID          string `json:"campaign_id"`
	CampaignClass       string `json:"campaign_class"`
	DeploymentID        string `json:"deployment_id"`
	ExperimentID        string `json:"experiment_id"`
	CellID              string `json:"cell_id"`
	Status              string `json:"status"`
	PublicationEligible bool   `json:"publication_eligible"`
	ProfileBinding      *struct {
		ProfileID     string `json:"profile_id"`
		CatalogSHA256 string `json:"catalog_sha256"`
	} `json:"profile_binding"`
}

func decodeCampaignJSONLLine(line []byte) (campaignJSONLSample, string, error) {
	var discriminator struct {
		Record        string          `json:"record"`
		CampaignClass string          `json:"campaign_class"`
		Sample        json.RawMessage `json:"sample"`
	}
	if err := json.Unmarshal(line, &discriminator); err != nil {
		return campaignJSONLSample{}, "", err
	}
	if discriminator.Sample != nil {
		if discriminator.Record != profileCampaignSampleV1Record {
			return campaignJSONLSample{}, "", errors.New("profile campaign sample envelope has a missing or unknown record")
		}
		var envelope struct {
			SchemaVersion int             `json:"schema_version"`
			Record        string          `json:"record"`
			CampaignClass string          `json:"campaign_class"`
			Sample        json.RawMessage `json:"sample"`
		}
		decoder := json.NewDecoder(strings.NewReader(string(line)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&envelope); err != nil {
			return campaignJSONLSample{}, "", fmt.Errorf("decode profile campaign sample envelope: %w", err)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return campaignJSONLSample{}, "", errors.New("profile campaign sample envelope has trailing JSON")
		}
		if envelope.SchemaVersion != 1 || envelope.Record != profileCampaignSampleV1Record {
			return campaignJSONLSample{}, "", errors.New("profile campaign sample envelope has an unknown record version")
		}
		if envelope.CampaignClass != "pilot" {
			return campaignJSONLSample{}, "", errors.New("profile campaign sample envelope is not runner-stamped as pilot")
		}
		var sample campaignJSONLSample
		if err := json.Unmarshal(envelope.Sample, &sample); err != nil {
			return campaignJSONLSample{}, "", fmt.Errorf("decode nested profile campaign sample: %w", err)
		}
		return sample, envelope.CampaignClass, nil
	}
	var sample campaignJSONLSample
	if err := json.Unmarshal(line, &sample); err != nil {
		return campaignJSONLSample{}, "", err
	}
	return sample, sample.CampaignClass, nil
}

func validateCleanup(root string, files []CampaignEvidenceFile) error {
	var cleanup struct {
		Status     string `json:"status"`
		Containers int    `json:"containers"`
		Volumes    int    `json:"volumes"`
		Networks   int    `json:"networks"`
	}
	if err := decodeCampaignFile(campaignEvidencePath(root, files, "cleanup"), &cleanup); err != nil {
		return fmt.Errorf("cleanup: %w", err)
	}
	if cleanup.Status != "pass" || cleanup.Containers != 0 || cleanup.Volumes != 0 || cleanup.Networks != 0 {
		return errors.New("deployment cleanup left Compose resources")
	}
	return nil
}

func validCampaignDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validCampaignCommit(value string) bool {
	if len(value) != 40 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
