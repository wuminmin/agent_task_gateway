package experiment

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var deploymentIDPattern = regexp.MustCompile(`^deployment-([0-9]{2})$`)

// AdapterOperation is the versioned, credential-free contract sent to a
// deployment-specific adapter. The executable reads JSONL operations from
// stdin and returns one complete Sample JSON object per line on stdout.
type AdapterOperation struct {
	SchemaVersion     int    `json:"schema_version"`
	CampaignClass     string `json:"campaign_class"`
	CampaignID        string `json:"campaign_id"`
	DeploymentID      string `json:"deployment_id"`
	ExperimentID      string `json:"experiment_id"`
	CellID            string `json:"cell_id"`
	SampleID          string `json:"sample_id"`
	Iteration         int    `json:"iteration"`
	ProcessReplicate  int    `json:"process_replicate"`
	OrderPosition     int    `json:"order_position"`
	RandomSeed        int64  `json:"random_seed"`
	Warmup            bool   `json:"warmup"`
	FreshRootRequired bool   `json:"fresh_root_required"`
	RootGroupID       string `json:"root_group_id"`
	WorkloadID        string `json:"workload_id"`
	Scale             string `json:"scale"`
	Mode              string `json:"mode"`
}

func RunCommand(experimentID string) int {
	flags := flag.NewFlagSet(experimentID, flag.ContinueOnError)
	configPath := flags.String("config", "", "strict experiment config")
	validateOnly := flags.Bool("validate-only", false, "validate without executing")
	smokeDir := flags.String("smoke-output", "", "write a tiny synthetic framework smoke")
	outputPath := flags.String("output", "", "create-exclusive measured JSONL output")
	deploymentID := flags.String("deployment-id", "", "fresh deployment identity")
	adapterPath := flags.String("adapter", "", "exact executable implementing the JSONL adapter protocol")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return 2
	}
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "-config is required")
		return 2
	}
	config, configBytes, err := LoadConfig(*configPath, experimentID)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *validateOnly {
		digest, _ := CanonicalResultHash([][]any{{config.CampaignID, config.ExperimentID}})
		out := map[string]any{"status": "valid", "experiment_id": experimentID, "config_sha256": digest, "config_bytes": len(configBytes)}
		_ = json.NewEncoder(os.Stdout).Encode(out)
		return 0
	}
	if *smokeDir != "" {
		if *outputPath != "" || *deploymentID != "" || *adapterPath != "" {
			fmt.Fprintln(os.Stderr, "-smoke-output cannot be combined with formal runner flags")
			return 2
		}
		if config.CampaignClass != "pilot" {
			fmt.Fprintln(os.Stderr, "smoke requires campaign_class=pilot")
			return 1
		}
		if err := WriteSmoke(config, *smokeDir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	if *outputPath == "" || *deploymentID == "" || *adapterPath == "" {
		fmt.Fprintln(os.Stderr, "formal execution requires -output, -deployment-id, and -adapter")
		return 2
	}
	if err := ExecuteAdapterCampaign(config, *deploymentID, *adapterPath, *outputPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// ExecuteAdapterCampaign owns operation ordering, sample identity, overwrite
// protection, and publication gating. The private adapter owns only
// environment-specific provisioning and measurement.
func ExecuteAdapterCampaign(config Config, deploymentID, adapterPath, outputPath string) error {
	if err := config.Validate(config.ExperimentID); err != nil {
		return err
	}
	deploymentNumber, err := parseDeploymentID(deploymentID, config.Deployments)
	if err != nil {
		return err
	}
	if err := validateRunnerEnvironment(config); err != nil {
		return err
	}
	absAdapter, err := filepath.Abs(adapterPath)
	if err != nil {
		return err
	}
	info, err := os.Lstat(absAdapter)
	if err != nil {
		return fmt.Errorf("adapter: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return errors.New("adapter must be an executable regular file, not a symlink")
	}

	writer, err := NewJSONLWriter(outputPath)
	if err != nil {
		return err
	}
	completed := false
	defer func() {
		if !completed {
			_ = writer.Close()
		}
	}()

	processes := config.ProcessReplicates
	if processes == 0 {
		processes = 1
	}
	seenFreshRoots := map[string]string{}
	rootGroups := map[string]string{}
	orderPosition := 0
	for processReplicate := 1; processReplicate <= processes; processReplicate++ {
		operations := buildOperations(config, deploymentID, deploymentNumber, processReplicate, &orderPosition)
		samples, err := runAdapterProcess(absAdapter, operations)
		if err != nil {
			for _, operation := range operations {
				if !operation.Warmup {
					if writeErr := writer.Write(invalidAdapterSample(operation, "adapter_process_failure")); writeErr != nil {
						return writeErr
					}
				}
			}
			return fmt.Errorf("adapter process replicate %d: %w", processReplicate, err)
		}
		for index := range operations {
			op, sample := operations[index], samples[index]
			if err := validateAdapterSample(config, op, sample, seenFreshRoots, rootGroups); err != nil {
				return fmt.Errorf("adapter sample %s: %w", op.SampleID, err)
			}
			if op.Warmup {
				continue
			}
			if err := writer.Write(sample); err != nil {
				return err
			}
		}
	}
	if err := writer.Close(); err != nil {
		return err
	}
	completed = true
	return nil
}

func invalidAdapterSample(operation AdapterOperation, code string) Sample {
	zeroPhases := map[string]float64{
		"prepare": 0, "execute_and_derive": 0, "artifact_stage": 0,
		"control_settlement": 0, "artifact_publication": 0, "response_finalize": 0, "server_total": 0,
	}
	system := "taskgate"
	if operation.Mode == "direct" || operation.Mode == "rls" {
		system = "postgresql"
	} else if operation.Mode == "provsql" {
		system = "provsql"
	}
	return Sample{
		SchemaVersion:       1,
		CampaignID:          operation.CampaignID,
		DeploymentID:        operation.DeploymentID,
		ExperimentID:        operation.ExperimentID,
		CellID:              operation.CellID,
		SampleID:            operation.SampleID,
		Iteration:           operation.Iteration,
		ProcessReplicate:    operation.ProcessReplicate,
		OrderPosition:       operation.OrderPosition,
		RandomSeed:          operation.RandomSeed,
		System:              system,
		Mode:                operation.Mode,
		WorkloadID:          operation.WorkloadID,
		Scale:               operation.Scale,
		PipelineMS:          zeroPhases,
		DiagnosticMS:        map[string]float64{},
		Status:              "invalid",
		ErrorCode:           code,
		PublicationEligible: operation.CampaignClass == "publication",
		Reason:              "deployment adapter did not return a complete operation batch",
	}
}

func parseDeploymentID(value string, maximum int) (int, error) {
	match := deploymentIDPattern.FindStringSubmatch(value)
	if match == nil {
		return 0, errors.New("deployment ID must have the form deployment-01")
	}
	number, _ := strconv.Atoi(match[1])
	if number < 1 || number > maximum {
		return 0, errors.New("deployment ID is outside the configured deployment count")
	}
	return number, nil
}

func validateRunnerEnvironment(config Config) error {
	required := map[string]string{
		"TASKGATE_EXPERIMENT_CLASS": config.CampaignClass,
		"TASKGATE_CAMPAIGN_ID":      config.CampaignID,
	}
	if config.CampaignClass == "publication" {
		required["TASKGATE_SUBMISSION_COMMIT"] = config.SubmissionCommit
	}
	for name, expected := range required {
		if os.Getenv(name) != expected {
			return fmt.Errorf("%s does not exactly match the frozen config", name)
		}
	}
	return nil
}

func buildOperations(config Config, deploymentID string, deploymentNumber, processReplicate int, orderPosition *int) []AdapterOperation {
	seed := config.RandomSeed + int64(deploymentNumber*100_000+processReplicate*1_000)
	type cell struct {
		workload string
		scale    string
		groups   [][]string
	}
	var cells []cell
	for _, workload := range config.Workloads {
		for _, scale := range workload.Scales {
			cells = append(cells, cell{workload: workload.ID, scale: scale, groups: dependencyAwareModeGroups(workload.Modes)})
		}
	}
	var operations []AdapterOperation
	appendRound := func(iteration int, warmup bool, roundSeed int64) {
		for _, cellIndex := range DeterministicOrder(len(cells), roundSeed) {
			selected := cells[cellIndex]
			for _, groupIndex := range DeterministicOrder(len(selected.groups), roundSeed+int64(cellIndex+1)*97) {
				group := selected.groups[groupIndex]
				for modePosition, mode := range group {
					*orderPosition++
					sampleKind := "sample"
					if warmup {
						sampleKind = "warmup"
					}
					sampleID := fmt.Sprintf("%s-p%02d-%s-%04d", deploymentID, processReplicate, sampleKind, *orderPosition)
					operations = append(operations, AdapterOperation{
						SchemaVersion:     1,
						CampaignClass:     config.CampaignClass,
						CampaignID:        config.CampaignID,
						DeploymentID:      deploymentID,
						ExperimentID:      config.ExperimentID,
						CellID:            selected.workload + "/" + selected.scale + "/" + mode,
						SampleID:          sampleID,
						Iteration:         iteration,
						ProcessReplicate:  processReplicate,
						OrderPosition:     *orderPosition,
						RandomSeed:        config.RandomSeed,
						Warmup:            warmup,
						FreshRootRequired: config.FreshRootPerSample && modePosition == 0 && freshRootAnchor(mode),
						RootGroupID:       group[0],
						WorkloadID:        selected.workload,
						Scale:             selected.scale,
						Mode:              mode,
					})
				}
			}
		}
	}
	for warmup := 1; warmup <= config.Warmups; warmup++ {
		appendRound(warmup, true, seed+int64(warmup)*10_007)
	}
	for iteration := 1; iteration <= config.Samples; iteration++ {
		appendRound(iteration, false, seed+1_000_003+int64(iteration)*10_007)
	}
	return operations
}

func dependencyAwareModeGroups(modes []string) [][]string {
	present := map[string]bool{}
	for _, mode := range modes {
		present[mode] = true
	}
	used := map[string]bool{}
	var groups [][]string
	if present["novel"] {
		chain := []string{"novel"}
		used["novel"] = true
		for _, replay := range []string{"semantic_replay", "normalized_rewrite_replay", "idempotent_replay"} {
			if present[replay] {
				chain = append(chain, replay)
				used[replay] = true
			}
		}
		groups = append(groups, chain)
	}
	if present["build_verify_activate"] {
		chain := []string{"build_verify_activate"}
		used["build_verify_activate"] = true
		if present["retained_route"] {
			chain = append(chain, "retained_route")
			used["retained_route"] = true
		}
		groups = append(groups, chain)
	}
	for _, mode := range modes {
		if !used[mode] {
			groups = append(groups, []string{mode})
			used[mode] = true
		}
	}
	return groups
}

func freshRootAnchor(mode string) bool {
	switch mode {
	case "direct", "semantic_replay", "idempotent_replay", "normalized_rewrite_replay",
		"provsql", "rls", "compile", "structured_rejection", "retained_route":
		return false
	default:
		return true
	}
}

func runAdapterProcess(path string, operations []AdapterOperation) ([]Sample, error) {
	var input bytes.Buffer
	encoder := json.NewEncoder(&input)
	for _, operation := range operations {
		if err := encoder.Encode(operation); err != nil {
			return nil, err
		}
	}
	var output bytes.Buffer
	var stderr bytes.Buffer
	command := exec.Command(path)
	command.Stdin = &input
	command.Stdout = &output
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("exited unsuccessfully: %w", err)
	}
	if stderr.Len() != 0 {
		return nil, errors.New("adapter wrote stderr; content was suppressed by the evidence secret boundary")
	}
	scanner := bufio.NewScanner(&output)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	samples := make([]Sample, 0, len(operations))
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			return nil, errors.New("adapter emitted a blank line")
		}
		var sample Sample
		if err := StrictJSON(scanner.Bytes(), &sample); err != nil {
			return nil, fmt.Errorf("invalid JSONL output line %d: %w", len(samples)+1, err)
		}
		samples = append(samples, sample)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(samples) != len(operations) {
		return nil, fmt.Errorf("adapter returned %d samples for %d operations", len(samples), len(operations))
	}
	return samples, nil
}

func validateAdapterSample(config Config, operation AdapterOperation, sample Sample, seenFreshRoots, rootGroups map[string]string) error {
	if sample.CampaignID != operation.CampaignID || sample.DeploymentID != operation.DeploymentID ||
		sample.ExperimentID != operation.ExperimentID || sample.CellID != operation.CellID ||
		sample.SampleID != operation.SampleID || sample.Iteration != operation.Iteration ||
		sample.ProcessReplicate != operation.ProcessReplicate || sample.OrderPosition != operation.OrderPosition ||
		sample.RandomSeed != operation.RandomSeed || sample.WorkloadID != operation.WorkloadID ||
		sample.Scale != operation.Scale || sample.Mode != operation.Mode {
		return errors.New("identity fields do not exactly match the requested operation")
	}
	if err := sample.Validate(); err != nil {
		return err
	}
	if operation.Warmup {
		return nil
	}
	if sample.PublicationEligible != (config.CampaignClass == "publication") {
		return errors.New("publication eligibility does not match campaign class")
	}
	if sample.KernelOnly != config.KernelOnly {
		return errors.New("kernel-only label does not match the frozen config")
	}
	if operation.FreshRootRequired && sample.System == "taskgate" {
		if !validSHA256(sample.RootTaskIDHash) {
			return errors.New("fresh TaskGate operation lacks a salted root task SHA-256")
		}
		if prior, exists := seenFreshRoots[sample.RootTaskIDHash]; exists {
			return fmt.Errorf("fresh root hash reused by %s", prior)
		}
		seenFreshRoots[sample.RootTaskIDHash] = sample.SampleID
		rootGroups[rootGroupKey(operation)] = sample.RootTaskIDHash
	} else if sample.System == "taskgate" && sample.RootTaskIDHash != "" {
		if expected, exists := rootGroups[rootGroupKey(operation)]; exists && sample.RootTaskIDHash != expected {
			return errors.New("dependent operation did not reuse its root group")
		}
	}
	return nil
}

func rootGroupKey(operation AdapterOperation) string {
	return strings.Join([]string{operation.DeploymentID, operation.WorkloadID, operation.Scale, strconv.Itoa(operation.ProcessReplicate), strconv.Itoa(operation.Iteration), operation.RootGroupID}, "\x00")
}

func WriteSmoke(config Config, runDir string) error {
	if _, err := os.Stat(runDir); err == nil {
		return errors.New("smoke output already exists")
	}
	if err := os.MkdirAll(filepath.Join(runDir, "raw"), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(runDir, "PILOT-NOT-FOR-PUBLICATION"), []byte("publication_eligible=false\n"), 0o600); err != nil {
		return err
	}
	configBytes, _ := json.MarshalIndent(config, "", "  ")
	if err := os.WriteFile(filepath.Join(runDir, "config.json"), append(configBytes, '\n'), 0o600); err != nil {
		return err
	}
	w, err := NewJSONLWriter(filepath.Join(runDir, "raw", "samples.jsonl"))
	if err != nil {
		return err
	}
	digest, _ := CanonicalResultHash([][]any{{"tiny", int64(1)}})
	position := 0
	for _, workload := range config.Workloads {
		for _, scale := range workload.Scales {
			for _, mode := range workload.Modes {
				cell := workload.ID + "/" + scale + "/" + mode
				for iteration := 1; iteration <= config.Samples; iteration++ {
					position++
					phases := map[string]float64{"prepare": .01, "execute_and_derive": .02, "artifact_stage": .01, "control_settlement": .01, "artifact_publication": .01, "response_finalize": .01, "server_total": .08}
					sample := Sample{SchemaVersion: 1, CampaignID: config.CampaignID, DeploymentID: "deployment-01", ExperimentID: config.ExperimentID, CellID: cell, SampleID: fmt.Sprintf("smoke-%04d", position), Iteration: iteration, OrderPosition: position, RandomSeed: config.RandomSeed, System: "taskgate", Mode: mode, WorkloadID: workload.ID, Scale: scale, ClientAvailableMS: .07, ClientFullDrainMS: .08, PipelineMS: phases, DiagnosticMS: map[string]float64{}, RowCount: 1, ColumnCount: 2, ResultSHA256: digest, ReceiptVersion: "8", Status: "pass", PublicationEligible: false, KernelOnly: config.KernelOnly}
					if mode == "semantic_replay" {
						sample.SemanticReplay = true
					}
					if mode == "idempotent_replay" {
						sample.IdempotentReplay = true
					}
					if err := w.Write(sample); err != nil {
						w.Close()
						return err
					}
				}
			}
		}
	}
	if err := w.Close(); err != nil {
		return err
	}
	_, err = FinalizeRun(runDir)
	return err
}

func UTCNow() string { return time.Now().UTC().Format(time.RFC3339Nano) }
