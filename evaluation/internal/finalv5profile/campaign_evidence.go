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
)

const ProfileCampaignEvidenceVersion = "taskgate-final-v5-profile-campaign-evidence-v1"

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
		case "profile_binding", "activation_evidence", "environment", "fresh_proof", "gateway_image", "cleanup":
			if file.Experiment != "" {
				return fmt.Errorf("deployment evidence %s must not name an experiment", file.Kind)
			}
		case "config", "selected_cells", "raw_jsonl":
			if !plannedExperiments[file.Experiment] {
				return fmt.Errorf("deployment evidence %s names unplanned experiment %q", file.Kind, file.Experiment)
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
		"fresh_proof", "gateway_image", "cleanup"} {
		if kinds[kind+"/"] != 1 {
			return fmt.Errorf("evidence kind %s count=%d, want 1", kind, kinds[kind+"/"])
		}
	}
	for _, experiment := range planned.Experiments {
		if kinds["config/"+experiment] != 1 || kinds["selected_cells/"+experiment] != 1 ||
			kinds["raw_jsonl/"+experiment] != 1 {
			return fmt.Errorf("experiment %s lacks one config/selected-cells/raw set", experiment)
		}
		if err := validateCampaignConfig(root, campaignID, commit, experiment, record.Files); err != nil {
			return err
		}
		if err := validateSelectedCells(root, experiment, planned, record.Files); err != nil {
			return err
		}
	}
	if err := validateProfileBinding(root, planned, record.Files); err != nil {
		return err
	}
	if err := validateActivationEvidence(root, planned, record.Files); err != nil {
		return err
	}
	if err := validateEnvironmentAndImage(root, commit, record.Files); err != nil {
		return err
	}
	observed := map[string]bool{}
	for experiment, file := range rawByExperiment {
		if err := validateCampaignJSONL(filepath.Join(root, file.Path), campaignID, experiment, planned, observed); err != nil {
			return err
		}
	}
	if len(observed) != len(wantCells) {
		return fmt.Errorf("observed cells=%d, want %d", len(observed), len(wantCells))
	}
	return validateCleanup(root, record.Files)
}

func validateCampaignEvidenceFile(root string, file CampaignEvidenceFile) error {
	if file.Kind == "" || file.Path == "" || filepath.IsAbs(file.Path) || !validCampaignDigest(file.SHA256) || file.Bytes < 1 {
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
	var binding struct {
		Version              string `json:"version"`
		ProfileID            string `json:"profile_id"`
		CatalogSHA256        string `json:"catalog_sha256"`
		ClosureSHA256        string `json:"closure_sha256"`
		DatasetBindingSHA256 string `json:"dataset_binding_sha256"`
		PublicationIdentity  string `json:"publication_identity"`
	}
	if err := decodeCampaignFile(campaignEvidencePath(root, files, "profile_binding"), &binding); err != nil {
		return fmt.Errorf("profile binding: %w", err)
	}
	if binding.Version != "taskgate-final-v5-profile-binding-v1" || binding.ProfileID != planned.ProfileID ||
		binding.CatalogSHA256 != planned.CatalogSHA256 {
		return errors.New("ProfileBinding differs from the planned profile")
	}
	return nil
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

func validateCampaignJSONL(path, campaignID, experiment string, planned PlannedDeploy, observed map[string]bool) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	allowed := map[string]bool{}
	for _, cell := range planned.Cells {
		allowed[cell] = true
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	lines := 0
	for scanner.Scan() {
		lines++
		var sample struct {
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
		if err := json.Unmarshal(scanner.Bytes(), &sample); err != nil {
			return fmt.Errorf("decode JSONL line %d: %w", lines, err)
		}
		identity := sample.ExperimentID + "/" + sample.CellID
		if sample.CampaignID != campaignID || sample.CampaignClass != "pilot" ||
			sample.DeploymentID != "deployment-01" || sample.ExperimentID != experiment ||
			sample.Status != "pass" || sample.PublicationEligible || !allowed[identity] ||
			sample.ProfileBinding == nil || sample.ProfileBinding.ProfileID != planned.ProfileID ||
			sample.ProfileBinding.CatalogSHA256 != planned.CatalogSHA256 {
			return fmt.Errorf("JSONL line %d differs from its profile assignment", lines)
		}
		if observed[identity] {
			return fmt.Errorf("JSONL cell %s is duplicated", identity)
		}
		observed[identity] = true
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if lines == 0 {
		return errors.New("raw JSONL is empty")
	}
	return nil
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
