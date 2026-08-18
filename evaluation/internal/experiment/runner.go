package experiment

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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
	PairID            string `json:"pair_id"`
	PairedSystemOrder string `json:"paired_system_order"`
	Warmup            bool   `json:"warmup"`
	KernelOnly        bool   `json:"kernel_only"`
	FreshRootRequired bool   `json:"fresh_root_required"`
	RootGroupID       string `json:"root_group_id"`
	WorkloadID        string `json:"workload_id"`
	// ProfileBinding is the complete deployment profile the orchestrator
	// activated for this operation. The orchestrator derives it from the Profile
	// Registry, the profile Catalog, the deployment Dataset Binding and the
	// canonical Publication closure; an Adapter only echoes it back and proves
	// it actually ran against those bytes. An Adapter never chooses any member.
	ProfileBinding *ProfileBinding `json:"profile_binding,omitempty"`
	Scale          string          `json:"scale"`
	Mode           string          `json:"mode"`
}

func RunCommand(experimentID string) int {
	flags := flag.NewFlagSet(experimentID, flag.ContinueOnError)
	configPath := flags.String("config", "", "strict experiment config")
	validateOnly := flags.Bool("validate-only", false, "validate without executing")
	smokeDir := flags.String("smoke-output", "", "write a tiny synthetic framework smoke")
	outputPath := flags.String("output", "", "create-exclusive measured JSONL output")
	deploymentID := flags.String("deployment-id", "", "fresh deployment identity")
	adapterPath := flags.String("adapter", "", "exact executable implementing the JSONL adapter protocol")
	adapterStderrPath := flags.String("adapter-stderr-output", "",
		"create-exclusive private file retaining exact adapter stderr; credential-gated pilot diagnostics only")
	profilePath := flags.String("profile-binding", "",
		"activated deployment profile binding JSON; required for publication and profile-bound pilot runs")
	selectedCellsPath := flags.String("selected-cells", "",
		"strict JSON array of experiment/cell identities assigned to this profile deployment")
	deploymentRepetition := flags.Int("deployment-repetition", 0,
		"fresh profile-deployment repetition bound into sample and round identities")
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
	selectedCells, err := loadSelectedCells(*selectedCellsPath, experimentID, config)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *validateOnly {
		digest := sha256Hex(configBytes)
		out := map[string]any{"status": "valid", "experiment_id": experimentID, "config_sha256": digest,
			"config_bytes": len(configBytes), "selected_cells": len(selectedCells)}
		_ = json.NewEncoder(os.Stdout).Encode(out)
		return 0
	}
	if (*deploymentRepetition == 0) != (selectedCells == nil) || *deploymentRepetition < 0 {
		fmt.Fprintln(os.Stderr, "-deployment-repetition must be positive exactly for selected profile campaigns")
		return 1
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
	profile, err := loadRunnerProfileBinding(*profilePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := executeAdapterCampaignWithProfileSelection(config, *deploymentID, *adapterPath, *outputPath,
		profile, *adapterStderrPath, selectedCells, *deploymentRepetition); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// ExecuteAdapterCampaign owns operation ordering, sample identity, overwrite
// protection, and publication gating. The checked-in adapter owns only
// experiment-specific provisioning and measurement.
func ExecuteAdapterCampaign(config Config, deploymentID, adapterPath, outputPath string) error {
	return ExecuteAdapterCampaignWithProfile(config, deploymentID, adapterPath, outputPath, nil)
}

// ExecuteAdapterCampaignWithProfile runs a campaign against an already
// activated deployment profile. The orchestrator owns the binding: it is
// derived from the Profile Registry, the profile Catalog, the deployment
// Dataset Binding and the canonical Publication closure, then handed to every
// operation. An Adapter only echoes it back and proves it really ran against
// those bytes, so the two halves have to agree or the sample is refused.
//
// Passing no binding stays legal here so the many existing pilot paths keep
// working unchanged; FinalizeRun remains the gate that refuses an unbound run
// whose class requires a profile. What the Runner will never do is invent a
// binding, or let a sample claim one the operation did not carry.
func ExecuteAdapterCampaignWithProfile(config Config, deploymentID, adapterPath, outputPath string,
	profile *ProfileBinding) error {
	return executeAdapterCampaignWithProfile(config, deploymentID, adapterPath, outputPath, profile, "")
}

func executeAdapterCampaignWithProfile(config Config, deploymentID, adapterPath, outputPath string,
	profile *ProfileBinding, adapterStderrPath string) error {
	return executeAdapterCampaignWithProfileSelection(config, deploymentID, adapterPath, outputPath,
		profile, adapterStderrPath, nil, 0)
}

func executeAdapterCampaignWithProfileSelection(config Config, deploymentID, adapterPath, outputPath string,
	profile *ProfileBinding, adapterStderrPath string, selectedCells map[string]bool, deploymentRepetition int) error {
	if err := config.Validate(config.ExperimentID); err != nil {
		return err
	}
	if profile != nil {
		if !profileBindingRequired(config) {
			// Synthetic framework smoke runs are never publication evidence and
			// must not acquire a deployment identity they did not run against.
			return errors.New("this run class must not carry a deployment profile binding")
		}
		if err := profile.Validate(); err != nil {
			return fmt.Errorf("activated profile binding: %w", err)
		}
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

	var adapterStderr io.WriteCloser
	if adapterStderrPath != "" {
		// Adapter stderr is a private diagnostic channel, never publication
		// evidence. Targeted pilots and profile-bound real-system pilots may open
		// it; the latter applies a credential gate before its campaign manifest
		// retains the file. Publication campaigns remain closed.
		targetedDiagnostic := (config.PilotKind == "artifact_targeted" && config.ExperimentID == "artifact") ||
			(config.PilotKind == "baseline_targeted" && config.ExperimentID == "baseline")
		profileCampaignDiagnostic := config.PilotKind == "real_system" && profile != nil && selectedCells != nil
		if config.CampaignClass != "pilot" || (!targetedDiagnostic && !profileCampaignDiagnostic) {
			return errors.New("adapter stderr output is restricted to targeted diagnostics")
		}
		adapterStderr, err = os.OpenFile(adapterStderrPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("create adapter stderr output: %w", err)
		}
		defer adapterStderr.Close()
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
	writeRetained := writer.Write
	if config.PilotKind == "real_system" {
		writeRetained = func(sample Sample) error {
			return writer.WriteProfileCampaignSample(config.CampaignClass, sample)
		}
	}

	processes := config.ProcessReplicates
	if processes == 0 {
		processes = 1
	}
	seenFreshRoots := map[string]string{}
	rootGroups := map[string]string{}
	orderPosition := 0
	var campaignErrors []error
	for processReplicate := 1; processReplicate <= processes; processReplicate++ {
		operations := buildOperationsSelectedWithDeploymentRepetition(config, deploymentID, deploymentNumber,
			processReplicate, &orderPosition, profile, selectedCells, deploymentRepetition)
		samples, processErr := runAdapterProcess(absAdapter, config.ExperimentID, operations, adapterStderr)
		for index := range operations {
			op := operations[index]
			if samples[index] == nil {
				if writeErr := writeRetained(invalidAdapterSample(op, "adapter_process_failure")); writeErr != nil {
					return writeErr
				}
				continue
			}
			sample := *samples[index]
			if err := validateAdapterSample(config, op, sample, seenFreshRoots, rootGroups); err != nil {
				campaignErrors = append(campaignErrors, fmt.Errorf("adapter sample %s: %w", op.SampleID, err))
				if writeErr := writeRetained(invalidAdapterSample(op, "adapter_sample_validation_failure")); writeErr != nil {
					return writeErr
				}
				continue
			}
			if op.Warmup {
				// Passing warmups remain untimed and excluded from raw measurements.
				// A failed/invalid warmup is an experiment failure in its own right:
				// retain it in JSONL so the requested operation is never erased.
				if sample.Status != "pass" {
					campaignErrors = append(campaignErrors, fmt.Errorf("adapter warmup %s returned status %s", op.SampleID, sample.Status))
					if err := writeRetained(sample); err != nil {
						return err
					}
				}
				continue
			}
			if err := writeRetained(sample); err != nil {
				return err
			}
		}
		if processErr != nil {
			campaignErrors = append(campaignErrors, fmt.Errorf("adapter process replicate %d: %w", processReplicate, processErr))
		}
	}
	if err := writer.Close(); err != nil {
		return err
	}
	completed = true
	return errors.Join(campaignErrors...)
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
		Warmup:              operation.Warmup,
		KernelOnly:          operation.KernelOnly,
		OrderPosition:       operation.OrderPosition,
		RandomSeed:          operation.RandomSeed,
		PairID:              operation.PairID,
		PairedSystemOrder:   operation.PairedSystemOrder,
		RootGroupID:         operation.RootGroupID,
		System:              system,
		Mode:                operation.Mode,
		WorkloadID:          operation.WorkloadID,
		Scale:               operation.Scale,
		PipelineMS:          zeroPhases,
		DiagnosticMS:        map[string]float64{},
		Status:              "invalid",
		ErrorCode:           code,
		PublicationEligible: operation.CampaignClass == "publication",
		Reason:              "runner retained a schema-safe invalid outcome after an adapter protocol failure",
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

func loadSelectedCells(path, experimentID string, config Config) (map[string]bool, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read selected cells: %w", err)
	}
	var identities []string
	if err := StrictJSON(payload, &identities); err != nil {
		return nil, fmt.Errorf("decode selected cells: %w", err)
	}
	if len(identities) == 0 {
		return nil, errors.New("selected cells must not be empty")
	}
	available := make(map[string]bool)
	for _, workload := range config.Workloads {
		for _, scale := range workload.Scales {
			for _, mode := range workload.Modes {
				available[experimentID+"/"+workload.ID+"/"+scale+"/"+mode] = true
			}
		}
	}
	selected := make(map[string]bool, len(identities))
	for _, identity := range identities {
		if identity == "" || identity != strings.TrimSpace(identity) || !available[identity] {
			return nil, fmt.Errorf("selected cell %q is not in experiment %s config", identity, experimentID)
		}
		if selected[identity] {
			return nil, fmt.Errorf("selected cell %q is duplicated", identity)
		}
		selected[identity] = true
	}
	return selected, nil
}

func buildOperations(config Config, deploymentID string, deploymentNumber, processReplicate int, orderPosition *int,
	profile *ProfileBinding) []AdapterOperation {
	return buildOperationsSelected(config, deploymentID, deploymentNumber, processReplicate, orderPosition,
		profile, nil)
}

func buildOperationsSelected(config Config, deploymentID string, deploymentNumber, processReplicate int,
	orderPosition *int, profile *ProfileBinding, selectedCells map[string]bool) []AdapterOperation {
	return buildOperationsSelectedWithDeploymentRepetition(config, deploymentID, deploymentNumber,
		processReplicate, orderPosition, profile, selectedCells, 0)
}

func buildOperationsSelectedWithDeploymentRepetition(config Config, deploymentID string, deploymentNumber,
	processReplicate int, orderPosition *int, profile *ProfileBinding, selectedCells map[string]bool,
	deploymentRepetition int) []AdapterOperation {
	seed := config.RandomSeed + int64(deploymentNumber*100_000+processReplicate*1_000)
	operationIdentityPrefix := deploymentID
	if deploymentRepetition > 0 {
		operationIdentityPrefix = fmt.Sprintf("%s-r%03d", deploymentID, deploymentRepetition)
	}
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
				declaredGroup := selected.groups[groupIndex]
				if selectedCells != nil {
					filtered := make([]string, 0, len(declaredGroup))
					for _, mode := range declaredGroup {
						identity := config.ExperimentID + "/" + selected.workload + "/" + selected.scale + "/" + mode
						if selectedCells[identity] {
							filtered = append(filtered, mode)
						}
					}
					declaredGroup = filtered
				}
				if len(declaredGroup) == 0 {
					continue
				}
				group := append([]string(nil), declaredGroup...)
				if containsMode(group, "direct") && containsMode(group, "novel") {
					if DeterministicOrder(2, roundSeed+int64(cellIndex+1)*193)[0] == 0 {
					} else {
						group = taskgateBeforeDirect(group)
					}
				} else if independentPairedGroup(group) {
					ordered := DeterministicOrder(len(group), roundSeed+int64(cellIndex+1)*193)
					shuffled := make([]string, len(group))
					for index, source := range ordered {
						shuffled[index] = group[source]
					}
					group = shuffled
				}
				pairOrder := strings.Join(group, ",")
				pairKind := "sample"
				if warmup {
					pairKind = "warmup"
				}
				pairID := fmt.Sprintf("%s-p%02d-%s-%04d-%s-%s-g%02d", operationIdentityPrefix, processReplicate, pairKind, iteration, selected.workload, selected.scale, groupIndex+1)
				for _, mode := range group {
					rootGroupID := strings.Join(declaredGroup, ",")
					// RLS, unlimited TaskGate, and bounded TaskGate are paired arms
					// over one frozen trace, but the two TaskGate arms require
					// independent exposure roots. Pair identity must not imply root
					// reuse across those systems.
					if config.ExperimentID == "rls" {
						rootGroupID = mode
					}
					*orderPosition++
					sampleKind := "sample"
					if warmup {
						sampleKind = "warmup"
					}
					sampleID := fmt.Sprintf("%s-p%02d-%s-%04d", operationIdentityPrefix, processReplicate, sampleKind, *orderPosition)
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
						PairID:            pairID,
						PairedSystemOrder: pairOrder,
						Warmup:            warmup,
						KernelOnly:        config.KernelOnly,
						FreshRootRequired: config.FreshRootPerSample && freshRootAnchor(mode),
						RootGroupID:       rootGroupID,
						WorkloadID:        selected.workload,
						Scale:             selected.scale,
						Mode:              mode,
						ProfileBinding:    profile,
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
		chain := make([]string, 0, 5)
		if present["direct"] {
			chain = append(chain, "direct")
			used["direct"] = true
		}
		chain = append(chain, "novel")
		used["novel"] = true
		for _, replay := range []string{"semantic_replay", "normalized_rewrite_replay", "idempotent_replay"} {
			if present[replay] {
				chain = append(chain, replay)
				used[replay] = true
			}
		}
		groups = append(groups, chain)
	}
	if present["pending_recovery"] {
		// Recovery is itself a novel execution followed by an exact request-ID
		// replay. Keep it on an independent fresh root so it never depends on a
		// successful novel arm from the ordinary matched baseline chain.
		groups = append(groups, []string{"pending_recovery"})
		used["pending_recovery"] = true
	}
	if present["direct"] && present["provsql"] && present["taskgate"] && !present["novel"] {
		group := make([]string, 0, 3)
		for _, mode := range modes {
			if mode == "direct" || mode == "provsql" || mode == "taskgate" {
				group = append(group, mode)
				used[mode] = true
			}
		}
		groups = append(groups, group)
	}
	if present["rls"] && present["unlimited"] && present["bounded"] {
		group := make([]string, 0, 3)
		for _, mode := range modes {
			if mode == "rls" || mode == "unlimited" || mode == "bounded" {
				group = append(group, mode)
				used[mode] = true
			}
		}
		groups = append(groups, group)
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

func containsMode(modes []string, wanted string) bool {
	for _, mode := range modes {
		if mode == wanted {
			return true
		}
	}
	return false
}

func independentPairedGroup(modes []string) bool {
	return len(modes) > 1 && ((containsMode(modes, "direct") && containsMode(modes, "provsql") && containsMode(modes, "taskgate")) ||
		(containsMode(modes, "rls") && containsMode(modes, "unlimited") && containsMode(modes, "bounded")))
}

func taskgateBeforeDirect(group []string) []string {
	result := make([]string, 0, len(group))
	for _, mode := range group {
		if mode != "direct" {
			result = append(result, mode)
		}
	}
	return append(result, "direct")
}

func freshRootAnchor(mode string) bool {
	switch mode {
	case "direct", "semantic_replay", "idempotent_replay", "normalized_rewrite_replay",
		"provsql", "rls", "compile", "structured_rejection", "retained_route",
		"merkle_control", "kernel_storage_only":
		return false
	default:
		return true
	}
}

func runAdapterProcess(path, experimentID string, operations []AdapterOperation, adapterStderr io.Writer) ([]*Sample, error) {
	var input bytes.Buffer
	encoder := json.NewEncoder(&input)
	for _, operation := range operations {
		if err := encoder.Encode(operation); err != nil {
			return nil, err
		}
	}
	var output bytes.Buffer
	var stderr bytes.Buffer
	command := exec.Command(path, "--experiment", experimentID)
	command.Stdin = &input
	command.Stdout = &output
	command.Stderr = &stderr
	var processErrors []error
	if err := command.Run(); err != nil {
		processErrors = append(processErrors, fmt.Errorf("exited unsuccessfully: %w", err))
	}
	if stderr.Len() != 0 {
		if adapterStderr != nil {
			if _, err := adapterStderr.Write(stderr.Bytes()); err != nil {
				processErrors = append(processErrors, fmt.Errorf("retain adapter stderr: %w", err))
			}
		}
	}
	scanner := bufio.NewScanner(&output)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	// Preserve every independently decodable line even if another line is
	// missing/malformed or the adapter exits non-zero. Responses are placed by
	// their requested sample identity rather than their physical line number:
	// otherwise omitting an early response would shift every later, valid
	// fail/invalid outcome into the wrong slot and erase it during validation.
	// Protocol ordering and cardinality remain fail-closed errors.
	samples := make([]*Sample, len(operations))
	expectedBySampleID := make(map[string]int, len(operations))
	for index, operation := range operations {
		if _, exists := expectedBySampleID[operation.SampleID]; exists {
			return nil, fmt.Errorf("duplicate requested sample identity %q", operation.SampleID)
		}
		expectedBySampleID[operation.SampleID] = index
	}
	line := 0
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			processErrors = append(processErrors, fmt.Errorf("adapter output line %d is blank", line+1))
			line++
			continue
		}
		var sample Sample
		if err := StrictJSON(scanner.Bytes(), &sample); err != nil {
			processErrors = append(processErrors, fmt.Errorf("invalid JSONL output line %d: %w", line+1, err))
			line++
			continue
		}
		index, requested := expectedBySampleID[sample.SampleID]
		if !requested {
			processErrors = append(processErrors, fmt.Errorf("adapter output line %d has an unrequested sample identity", line+1))
			line++
			continue
		}
		if samples[index] != nil {
			processErrors = append(processErrors, fmt.Errorf("adapter output line %d duplicates sample %q", line+1, sample.SampleID))
			line++
			continue
		}
		if line >= len(operations) || operations[line].SampleID != sample.SampleID {
			processErrors = append(processErrors, fmt.Errorf("adapter output line %d is out of requested order", line+1))
		}
		samples[index] = &sample
		line++
	}
	if err := scanner.Err(); err != nil {
		processErrors = append(processErrors, err)
	}
	if line != len(operations) {
		processErrors = append(processErrors, fmt.Errorf("adapter returned %d lines for %d operations", line, len(operations)))
	}
	if stderr.Len() != 0 {
		if adapterStderr == nil || validatePreregisteredConcurrencyMissDiagnostics(experimentID, operations, samples, stderr.Bytes()) != nil {
			processErrors = append(processErrors, errors.New("adapter wrote stderr; content was suppressed by the evidence secret boundary"))
		}
	}
	return samples, errors.Join(processErrors...)
}

func validatePreregisteredConcurrencyMissDiagnostics(experimentID string, operations []AdapterOperation,
	samples []*Sample, payload []byte) error {
	if experimentID != "concurrency" || len(operations) != len(samples) || len(payload) == 0 {
		return errors.New("adapter diagnostics are outside the preregistered concurrency path")
	}
	exactMisses := make(map[string]Sample)
	for index, sample := range samples {
		if sample == nil || sample.SampleID != operations[index].SampleID {
			continue
		}
		observedPass, err := ValidatePreregisteredConcurrencyRound(*sample)
		if err == nil && !observedPass {
			exactMisses[sample.SampleID] = *sample
		}
	}
	if len(exactMisses) == 0 {
		return errors.New("adapter diagnostics have no exact preregistered miss")
	}
	seen := make(map[string]bool, len(exactMisses))
	scanner := bufio.NewScanner(bytes.NewReader(payload))
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		var diagnostic PreregisteredConcurrencyMissDiagnosticV1
		if err := StrictJSON(scanner.Bytes(), &diagnostic); err != nil {
			return err
		}
		if err := diagnostic.Validate(); err != nil {
			return err
		}
		sample, exists := exactMisses[diagnostic.SampleID]
		if !exists || seen[diagnostic.SampleID] || diagnostic.ExperimentID != sample.ExperimentID ||
			diagnostic.CellID != sample.CellID {
			return errors.New("preregistered miss diagnostic differs from its retained sample")
		}
		seen[diagnostic.SampleID] = true
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if len(seen) != len(exactMisses) {
		return errors.New("preregistered miss diagnostic count differs from retained exact misses")
	}
	return nil
}

// validateOperationProfileBinding is fail-closed per sample. It never accepts a
// partial comparison: an equal profile_id with a different Catalog digest is a
// different deployment and must not become a pass sample.
func validateOperationProfileBinding(operation AdapterOperation, sample Sample) error {
	if operation.ProfileBinding == nil {
		// Synthetic framework smoke runs deliberately carry no profile. They are
		// never publication evidence, and the Runner refuses to invent a binding
		// on their behalf.
		if sample.ProfileBinding != nil {
			return errors.New("sample carries a profile binding the operation did not request")
		}
		return nil
	}
	if err := RequireProfileBinding(sample); err != nil {
		return err
	}
	if err := operation.ProfileBinding.Validate(); err != nil {
		return fmt.Errorf("requested profile binding is invalid: %w", err)
	}
	if fields := operation.ProfileBinding.MismatchedFields(*sample.ProfileBinding); len(fields) != 0 {
		return fmt.Errorf("sample profile binding differs from the activated profile on %s",
			strings.Join(fields, ", "))
	}
	return nil
}

func validateAdapterSample(config Config, operation AdapterOperation, sample Sample, seenFreshRoots, rootGroups map[string]string) error {
	if sample.CampaignID != operation.CampaignID || sample.DeploymentID != operation.DeploymentID ||
		sample.ExperimentID != operation.ExperimentID || sample.CellID != operation.CellID ||
		sample.SampleID != operation.SampleID || sample.Iteration != operation.Iteration ||
		sample.ProcessReplicate != operation.ProcessReplicate || sample.Warmup != operation.Warmup || sample.KernelOnly != operation.KernelOnly || sample.OrderPosition != operation.OrderPosition ||
		sample.RandomSeed != operation.RandomSeed || sample.WorkloadID != operation.WorkloadID ||
		sample.Scale != operation.Scale || sample.Mode != operation.Mode || sample.PairID != operation.PairID ||
		sample.PairedSystemOrder != operation.PairedSystemOrder || sample.RootGroupID != operation.RootGroupID {
		return errors.New("identity fields do not exactly match the requested operation")
	}
	if err := sample.Validate(); err != nil {
		return err
	}
	// A profile-enabled run binds every arm, including a Direct arm that never
	// reaches the Gateway: the pair is only comparable when both read the same
	// Dataset, Catalog and Publication set. Warmups are bound too, because a
	// warmup executed against the wrong profile would prime the wrong caches.
	if err := validateOperationProfileBinding(operation, sample); err != nil {
		return err
	}
	if operation.Warmup {
		return nil
	}
	if sample.PublicationEligible != (config.CampaignClass == "publication") {
		return errors.New("publication eligibility does not match campaign class")
	}
	if operation.KernelOnly != config.KernelOnly {
		return errors.New("kernel-only label does not match the frozen config")
	}
	// A fail/invalid operation may stop before provisioning a TaskGate root.
	// Its schema-safe outcome must still be retained in raw evidence; fresh-root
	// uniqueness is a correctness gate for completed measurements, not a reason
	// to erase a failed attempt.
	if sample.Status != "pass" {
		return nil
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
					system := "taskgate"
					if mode == "direct" {
						system = "postgresql"
					}
					sample := Sample{SchemaVersion: 1, CampaignID: config.CampaignID, DeploymentID: "deployment-01", ExperimentID: config.ExperimentID, CellID: cell, SampleID: fmt.Sprintf("smoke-%04d", position), Iteration: iteration, ProcessReplicate: 1, Warmup: false, OrderPosition: position, RandomSeed: config.RandomSeed, PairID: fmt.Sprintf("smoke-pair-%s-%s-%04d", workload.ID, scale, iteration), PairedSystemOrder: strings.Join(workload.Modes, ","), RootGroupID: strings.Join(workload.Modes, ","), System: system, Mode: mode, WorkloadID: workload.ID, Scale: scale, ClientAvailableMS: .07, ClientFullDrainMS: .08, PipelineMS: phases, DiagnosticMS: map[string]float64{}, RowCount: 1, ColumnCount: 2, ResultSHA256: digest, ReceiptVersion: "8", Status: "pass", PublicationEligible: false, KernelOnly: config.KernelOnly}
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

// loadRunnerProfileBinding reads the activated deployment profile the
// orchestrator bound before starting the run. It is strict: an unreadable,
// unknown-field or incomplete binding stops the run rather than degrading to an
// unbound one.
func loadRunnerProfileBinding(path string) (*ProfileBinding, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read activated profile binding: %w", err)
	}
	var binding ProfileBinding
	if err := StrictJSON(payload, &binding); err != nil {
		return nil, fmt.Errorf("decode activated profile binding: %w", err)
	}
	if err := binding.Validate(); err != nil {
		return nil, fmt.Errorf("activated profile binding: %w", err)
	}
	return &binding, nil
}
