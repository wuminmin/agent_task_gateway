package experiment

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type DeploymentDistribution struct {
	N   int     `json:"n"`
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
}
type Distribution struct {
	N                   int                               `json:"n"`
	P50                 float64                           `json:"p50"`
	P95                 float64                           `json:"p95"`
	P50Bootstrap        Interval                          `json:"p50_bootstrap"`
	Failed              int                               `json:"failed"`
	Invalid             int                               `json:"invalid"`
	Deployments         map[string]DeploymentDistribution `json:"deployments,omitempty"`
	DeploymentP50Median float64                           `json:"deployment_p50_median,omitempty"`
	DeploymentP50Min    float64                           `json:"deployment_p50_min,omitempty"`
	DeploymentP50Max    float64                           `json:"deployment_p50_max,omitempty"`
}
type PairedRatio struct {
	DeploymentID    string  `json:"deployment_id"`
	WorkloadID      string  `json:"workload_id"`
	Scale           string  `json:"scale"`
	NumeratorMode   string  `json:"numerator_mode"`
	DenominatorMode string  `json:"denominator_mode"`
	P50Ratio        float64 `json:"p50_ratio"`
	P95Ratio        float64 `json:"p95_ratio"`
}
type DeploymentManifest struct {
	SchemaVersion               int    `json:"schema_version"`
	CampaignID                  string `json:"campaign_id"`
	DeploymentID                string `json:"deployment_id"`
	FreshDeployment             bool   `json:"fresh_deployment"`
	EnvironmentSHA256           string `json:"environment_sha256"`
	WindowsEnvironmentSHA256    string `json:"windows_environment_sha256"`
	VMStatBeforeSHA256          string `json:"vmstat_before_sha256"`
	VMStatAfterSHA256           string `json:"vmstat_after_sha256"`
	StartedAt                   string `json:"started_at"`
	FinishedAt                  string `json:"finished_at"`
	ExitStatus                  int    `json:"exit_status"`
	SwapInDelta                 int64  `json:"swap_in_delta"`
	SwapOutDelta                int64  `json:"swap_out_delta"`
	OOM                         bool   `json:"oom"`
	UnexpectedContainerRestarts int64  `json:"unexpected_container_restarts"`
}
type Summary struct {
	SchemaVersion       int                     `json:"schema_version"`
	CampaignID          string                  `json:"campaign_id"`
	CampaignClass       string                  `json:"campaign_class"`
	SubmissionCommit    string                  `json:"submission_commit"`
	Status              string                  `json:"status"`
	PublicationEligible bool                    `json:"publication_eligible"`
	Cells               map[string]Distribution `json:"cells"`
	PairedRatios        []PairedRatio           `json:"paired_ratios,omitempty"`
	Reasons             []string                `json:"reasons,omitempty"`
}

func FinalizeRun(runDir string) (Summary, error) {
	config, _, err := LoadConfig(filepath.Join(runDir, "config.json"), "")
	if err != nil {
		return Summary{}, err
	}
	paths, err := filepath.Glob(filepath.Join(runDir, "raw", "*.jsonl"))
	if err != nil || len(paths) == 0 {
		return Summary{}, errors.New("no raw JSONL samples")
	}
	samples, err := ReadSamples(paths)
	if err != nil {
		return Summary{}, err
	}
	summary := Summary{SchemaVersion: 1, CampaignID: config.CampaignID, CampaignClass: config.CampaignClass, SubmissionCommit: config.SubmissionCommit, Status: "incomplete", Cells: map[string]Distribution{}}
	byCell := map[string][]float64{}
	byDeployment := map[string]map[string][]float64{}
	pairValues := map[string]map[string][]float64{}
	resultPairs := map[string]map[string]string{}
	factSetPairs := map[string]map[string]map[string]bool{}
	attackDirectionPairs := map[string]map[string]bool{}
	deployments := map[string]bool{}
	seenFreshRoots := map[string]bool{}
	rootAnchors := map[string]string{}
	for _, sample := range samples {
		if sample.CampaignID != config.CampaignID || sample.ExperimentID != config.ExperimentID {
			return summary, errors.New("raw sample campaign/experiment mismatch")
		}
		deployments[sample.DeploymentID] = true
		rootKey := sample.DeploymentID + "\x00" + sample.WorkloadID + "\x00" + sample.Scale + "\x00" + strconv.Itoa(sample.ProcessReplicate) + "\x00" + strconv.Itoa(sample.Iteration)
		if config.CampaignClass == "publication" && sample.Status == "pass" && sample.System == "taskgate" {
			if freshRootAnchor(sample.Mode) {
				if !validSHA256(sample.RootTaskIDHash) || seenFreshRoots[sample.RootTaskIDHash] {
					summary.Status = "fail"
					summary.Reasons = append(summary.Reasons, "missing or reused fresh TaskGate root hash")
				} else {
					seenFreshRoots[sample.RootTaskIDHash] = true
					rootAnchors[rootKey] = sample.RootTaskIDHash
				}
			} else if sample.Mode == "semantic_replay" || sample.Mode == "normalized_rewrite_replay" || sample.Mode == "idempotent_replay" || sample.Mode == "retained_route" {
				if !validSHA256(sample.RootTaskIDHash) || rootAnchors[rootKey] != sample.RootTaskIDHash {
					summary.Status = "fail"
					summary.Reasons = append(summary.Reasons, "dependent replay root binding mismatch")
				}
			}
		}
		resultKey := sample.DeploymentID + "\x00" + sample.WorkloadID + "\x00" + sample.Scale + "\x00" + strconv.Itoa(sample.ProcessReplicate) + "\x00" + strconv.Itoa(sample.Iteration)
		if sample.Status == "pass" && !sample.Rejected && sample.ResultSHA256 != "" {
			if resultPairs[resultKey] == nil {
				resultPairs[resultKey] = map[string]string{}
			}
			resultPairs[resultKey][sample.System+"/"+sample.Mode] = sample.ResultSHA256
		}
		if sample.Status == "pass" && !sample.Rejected {
			for dimension, digest := range map[string]string{"release": sample.ReleaseSetSHA256, "dependency": sample.DependencySetSHA256, "outcome": sample.OutcomeSetSHA256} {
				if digest == "" {
					continue
				}
				if !validSHA256(digest) {
					summary.Status = "fail"
					summary.Reasons = append(summary.Reasons, "invalid FactSet digest")
					continue
				}
				if factSetPairs[resultKey] == nil {
					factSetPairs[resultKey] = map[string]map[string]bool{}
				}
				if factSetPairs[resultKey][dimension] == nil {
					factSetPairs[resultKey][dimension] = map[string]bool{}
				}
				factSetPairs[resultKey][dimension][digest] = true
			}
			if sample.ExperimentID == "attack" && sample.System == "taskgate" && (strings.HasPrefix(sample.WorkloadID, "A-") || strings.HasPrefix(sample.WorkloadID, "D-")) {
				attackKey := sample.DeploymentID + "\x00" + sample.WorkloadID + "\x00" + strconv.Itoa(sample.ProcessReplicate) + "\x00" + strconv.Itoa(sample.Iteration)
				if attackDirectionPairs[attackKey] == nil {
					attackDirectionPairs[attackKey] = map[string]bool{}
				}
				attackDirectionPairs[attackKey][sample.ResultSHA256+sample.ReleaseSetSHA256+sample.DependencySetSHA256+sample.OutcomeSetSHA256] = true
			}
		}
		d := summary.Cells[sample.CellID]
		switch sample.Status {
		case "pass":
			if sample.PublicationEligible {
				byCell[sample.CellID] = append(byCell[sample.CellID], sample.ClientFullDrainMS)
				if byDeployment[sample.CellID] == nil {
					byDeployment[sample.CellID] = map[string][]float64{}
				}
				byDeployment[sample.CellID][sample.DeploymentID] = append(byDeployment[sample.CellID][sample.DeploymentID], sample.ClientFullDrainMS)
				pairKey := sample.DeploymentID + "\x00" + sample.WorkloadID + "\x00" + sample.Scale
				if pairValues[pairKey] == nil {
					pairValues[pairKey] = map[string][]float64{}
				}
				pairValues[pairKey][sample.Mode] = append(pairValues[pairKey][sample.Mode], sample.ClientFullDrainMS)
			}
		case "fail":
			d.Failed++
		case "invalid":
			d.Invalid++
		}
		summary.Cells[sample.CellID] = d
		if config.CampaignClass == "publication" && !sample.PublicationEligible {
			summary.Reasons = append(summary.Reasons, "publication campaign contains ineligible samples")
		}
		if sample.SemanticReplay && sample.BusinessSQLDelta != 0 {
			summary.Reasons = append(summary.Reasons, "semantic replay executed Business SQL")
		}
		if sample.CrossEpochReplay {
			summary.Status = "fail"
			summary.Reasons = append(summary.Reasons, "cross-epoch replay detected")
		}
		if sample.BudgetViolation {
			summary.Status = "fail"
			summary.Reasons = append(summary.Reasons, "budget violation detected")
		}
		if config.CampaignClass == "publication" && sample.System == "taskgate" && sample.Status == "pass" && requiresArtifactEvidence(sample) {
			if sample.Rejected {
				if !sample.RejectedNoResult || !sample.RejectedNoArtifact || !sample.RejectedNoSuccessfulAudit || sample.ArtifactSHA256 != "" || sample.ObjectSHA256 != "" || sample.AvailabilityAuditSHA256 != "" {
					summary.Status = "fail"
					summary.Reasons = append(summary.Reasons, "rejected request produced artifact evidence")
				}
			} else if sample.ReceiptVersion != "8" || !sample.ReceiptVerified || !sample.ArtifactAvailable || !validSHA256(sample.ReceiptSHA256) || !validSHA256(sample.ArtifactIntentSHA256) || !validSHA256(sample.AvailabilityAuditSHA256) {
				summary.Reasons = append(summary.Reasons, "TaskGate pass sample lacks verified V8/AVAILABLE evidence")
			}
		}
		if config.CampaignClass == "publication" && sample.Status == "pass" {
			experimentReasons, experimentFailed := validateExperimentEvidence(sample)
			summary.Reasons = append(summary.Reasons, experimentReasons...)
			if experimentFailed {
				summary.Status = "fail"
			}
		}
	}
	for _, dimensions := range factSetPairs {
		for _, digests := range dimensions {
			if len(digests) > 1 {
				summary.Status = "fail"
				summary.Reasons = append(summary.Reasons, "paired exact FactSet mismatch")
			}
		}
	}
	for _, evidence := range attackDirectionPairs {
		if len(evidence) > 1 {
			summary.Status = "fail"
			summary.Reasons = append(summary.Reasons, "bidirectional attack exact result/FactSet mismatch")
		}
	}
	for _, systems := range resultPairs {
		direct := ""
		for key, value := range systems {
			if strings.HasPrefix(key, "postgresql/") {
				direct = value
				break
			}
		}
		if direct == "" {
			continue
		}
		for key, value := range systems {
			if !strings.HasPrefix(key, "postgresql/") && value != direct {
				summary.Status = "fail"
				summary.Reasons = append(summary.Reasons, "paired canonical result mismatch")
			}
		}
	}
	for cell, values := range byCell {
		d := summary.Cells[cell]
		d.N = len(values)
		d.P50, _ = Type7(values, .5)
		d.P95, _ = Type7(values, .95)
		d.P50Bootstrap, _ = BootstrapMedian(values, 20260801, 2000)
		d.Deployments = map[string]DeploymentDistribution{}
		var deploymentP50 []float64
		for deployment, one := range byDeployment[cell] {
			p50, _ := Type7(one, .5)
			p95, _ := Type7(one, .95)
			d.Deployments[deployment] = DeploymentDistribution{N: len(one), P50: p50, P95: p95}
			deploymentP50 = append(deploymentP50, p50)
		}
		if len(deploymentP50) > 0 {
			d.DeploymentP50Median, _ = Type7(deploymentP50, .5)
			sort.Float64s(deploymentP50)
			d.DeploymentP50Min = deploymentP50[0]
			d.DeploymentP50Max = deploymentP50[len(deploymentP50)-1]
		}
		summary.Cells[cell] = d
	}
	for key, modes := range pairValues {
		direct, okDirect := modes["direct"]
		if !okDirect {
			continue
		}
		direct50, _ := Type7(direct, .5)
		direct95, _ := Type7(direct, .95)
		if direct50 <= 0 || direct95 <= 0 {
			continue
		}
		parts := strings.Split(key, "\x00")
		for mode, values := range modes {
			if mode == "direct" {
				continue
			}
			mode50, _ := Type7(values, .5)
			mode95, _ := Type7(values, .95)
			summary.PairedRatios = append(summary.PairedRatios, PairedRatio{DeploymentID: parts[0], WorkloadID: parts[1], Scale: parts[2], NumeratorMode: mode, DenominatorMode: "direct", P50Ratio: mode50 / direct50, P95Ratio: mode95 / direct95})
		}
	}
	sort.Slice(summary.PairedRatios, func(i, j int) bool {
		left := summary.PairedRatios[i].DeploymentID + summary.PairedRatios[i].WorkloadID + summary.PairedRatios[i].Scale + summary.PairedRatios[i].NumeratorMode
		right := summary.PairedRatios[j].DeploymentID + summary.PairedRatios[j].WorkloadID + summary.PairedRatios[j].Scale + summary.PairedRatios[j].NumeratorMode
		return left < right
	})
	if config.CampaignClass != "publication" {
		summary.Reasons = append(summary.Reasons, "pilot campaigns are never publication eligible")
	} else if len(deployments) != 3 {
		summary.Reasons = append(summary.Reasons, "publication campaign requires exactly three deployments")
	}
	if config.CampaignClass == "publication" {
		summary.Reasons = append(summary.Reasons, validateDeploymentEvidence(runDir, config)...)
	}
	for _, workload := range config.Workloads {
		for _, scale := range workload.Scales {
			for _, mode := range workload.Modes {
				cell := workload.ID + "/" + scale + "/" + mode
				d, ok := summary.Cells[cell]
				processes := config.ProcessReplicates
				if processes == 0 {
					processes = 1
				}
				if !ok || d.N != config.Deployments*config.Samples*processes || d.Failed != 0 || d.Invalid != 0 {
					summary.Reasons = append(summary.Reasons, "required cell incomplete: "+cell)
				}
			}
		}
	}
	for _, d := range summary.Cells {
		if d.Failed > 0 {
			summary.Status = "fail"
			summary.Reasons = append(summary.Reasons, "one or more measured samples failed")
		}
	}
	summary.Reasons = uniqueStrings(summary.Reasons)
	if config.CampaignClass == "publication" && len(summary.Reasons) == 0 {
		summary.Status = "pass"
		summary.PublicationEligible = true
	}
	if err := writeSummaryArtifacts(runDir, summary); err != nil {
		return summary, err
	}
	return summary, nil
}

func requiresArtifactEvidence(sample Sample) bool {
	if sample.ExperimentID == "compiler" || sample.KernelOnly {
		return false
	}
	return !(sample.ExperimentID == "scale" && sample.WorkloadID == "outcome-merkle")
}

func validateExperimentEvidence(sample Sample) ([]string, bool) {
	var reasons []string
	failed := false
	fail := func(reason string) {
		reasons = append(reasons, reason)
		failed = true
	}
	requireDigests := func(names map[string]string) {
		for name, digest := range names {
			if !validSHA256(digest) {
				fail("missing or invalid " + name + " digest")
			}
		}
	}
	requireDiagnostics := func(names ...string) {
		for _, name := range names {
			if value, ok := sample.DiagnosticMS[name]; !ok || value < 0 {
				fail("missing diagnostic: " + name)
			}
		}
	}
	requireCounters := func(names ...string) {
		for _, name := range names {
			if value, ok := sample.Counters[name]; !ok || value < 0 {
				fail("missing counter: " + name)
			}
		}
	}
	if !sample.Rejected && sample.ExperimentID != "compiler" && !validSHA256(sample.ResultSHA256) {
		fail("non-rejected pass sample lacks canonical result digest")
	}
	switch sample.ExperimentID {
	case "baseline":
		requireDigests(map[string]string{"physical SQL": sample.PhysicalSQLSHA256, "logical SQL": sample.LogicalSQLSHA256, "query plan": sample.QueryPlanSHA256})
	case "rls", "attack":
		if sample.System == "taskgate" && !sample.Rejected {
			requireDigests(map[string]string{"release set": sample.ReleaseSetSHA256, "dependency set": sample.DependencySetSHA256, "outcome set": sample.OutcomeSetSHA256})
		}
		if sample.ExperimentID == "rls" {
			expectedSteps := 100
			if sample.WorkloadID == "policy-denied-control" {
				expectedSteps = 1
			}
			if sample.Mode == "bounded" {
				if len(sample.Trace) < 1 || len(sample.Trace) > expectedSteps || !sample.Trace[len(sample.Trace)-1].Rejected {
					fail("bounded RLS trace lacks its terminal preregistered rejection")
				}
			} else if len(sample.Trace) != expectedSteps {
				fail("RLS/unlimited trace is incomplete")
			}
		} else if len(sample.Trace) == 0 {
			fail("adaptive attack sample lacks request trace evidence")
		}
		validateTrace(sample, fail)
	case "provsql":
		if sample.System == "taskgate" && (sample.GenerationBoundaryMS <= 0 || sample.FullTaskGateMS <= 0) {
			fail("ProvSQL-paired TaskGate sample lacks both measurement boundaries")
		}
	case "scale":
		if sample.WorkloadID == "dependency-e2e" {
			expected, ok := overlapFromScale(sample.Scale, "overlap-")
			if !ok || sample.ActualDependencyFacts <= 0 || (sample.ActualDependencyFacts-sample.ChargedDependencyFacts)*100 != sample.ActualDependencyFacts*int64(expected) {
				fail("dependency overlap label does not match actual/charged facts")
			}
		} else if sample.WorkloadID == "outcome-merkle" {
			requireCounters("blocks_loaded", "leaves_loaded", "hashes_loaded", "blocks_reused", "leaves_changed", "novelty", "storage_bytes", "peak_heap_bytes", "replay_changed_objects")
			requireDiagnostics("outcome_radix_load", "outcome_radix_difference_union", "outcome_radix_persist")
		}
	case "artifact":
		requireDiagnostics("parquet_encode_encrypt", "local_staging_sync", "staging_object_put", "receipt_signing", "canonical_object_stat", "canonical_object_copy", "canonical_object_hash_verify", "mark_available")
		if sample.ParquetBytes <= 0 || sample.EncryptedObjectBytes <= 0 || sample.GatewayMemoryPeakBytes <= 0 {
			fail("artifact sample lacks byte or memory measurements")
		}
	case "compiler":
		requireDiagnostics("total", "recursive_expansion", "parse_validation", "join_graph_canonicalization", "plan_materialization", "digest_generation")
		requireCounters("alloc_bytes", "alloc_objects")
	case "concurrency":
		if sample.Mode == "natural_contention" {
			requireCounters("cas_attempts", "cas_conflicts", "cas_retries")
			if sample.Counters["cas_attempts"] < 1 || sample.Counters["cas_retries"] != sample.Counters["cas_conflicts"] {
				fail("natural contention CAS counters are incoherent")
			}
		}
		if sample.Scale == "500" && (sample.Counters["barrier_clients"] != 500 || sample.Counters["offered_concurrency_observed"] != 1) {
			fail("width 500 passed without offered concurrency evidence")
		}
	}
	return reasons, failed
}

func overlapFromScale(scale, marker string) (int, bool) {
	index := strings.LastIndex(scale, marker)
	if index < 0 {
		return 0, false
	}
	value := scale[index+len(marker):]
	parsed, err := strconv.Atoi(value)
	return parsed, err == nil && (parsed == 0 || parsed == 50 || parsed == 90 || parsed == 100)
}

func validateTrace(sample Sample, fail func(string)) {
	transitions := map[string]string{}
	for index, step := range sample.Trace {
		if step.Index != index+1 || strings.TrimSpace(step.ConcreteSQL) == "" || !validSHA256(step.PriorStateSHA256) || !validSHA256(step.ResultSHA256) || !validSHA256(step.NextSQLSHA256) {
			fail("adaptive trace contains an invalid or non-sequential transition")
			continue
		}
		if prior, exists := transitions[step.PriorStateSHA256]; exists && prior != step.NextSQLSHA256 {
			fail("adaptive policy selected different next SQL for the same prior state")
		}
		transitions[step.PriorStateSHA256] = step.NextSQLSHA256
		if sample.System == "taskgate" && !step.Rejected {
			for _, digest := range []string{step.PlanSHA256, step.ObservationSHA256, step.ReleaseSetSHA256, step.DependencySetSHA256, step.OutcomeSetSHA256} {
				if !validSHA256(digest) {
					fail("TaskGate adaptive trace step lacks plan/observation/FactSet digest")
					break
				}
			}
		}
		if step.Rejected && (!step.NoResult || !step.NoAvailableArtifact || !step.NoSuccessfulAudit) {
			fail("rejected adaptive trace step lacks negative artifact/audit proof")
		}
	}
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func validateDeploymentEvidence(runDir string, config Config) []string {
	var reasons []string
	adapterDigest, adapterErr := os.ReadFile(filepath.Join(runDir, "adapter.sha256"))
	if adapterErr != nil || !validSHA256(strings.TrimSpace(string(adapterDigest))) {
		reasons = append(reasons, "measured adapter SHA-256 is missing or invalid")
	}
	paths, _ := filepath.Glob(filepath.Join(runDir, "deployments", "deployment-*.json"))
	if len(paths) != 3 {
		return append(reasons, "exactly three deployment manifests are required")
	}
	seen := map[string]bool{}
	seenVolumes := map[string]bool{}
	windowsEnvironmentSHA256 := ""
	for _, path := range paths {
		value, err := os.ReadFile(path)
		if err != nil {
			reasons = append(reasons, "deployment manifest unreadable")
			continue
		}
		var deployment DeploymentManifest
		if err := StrictJSON(value, &deployment); err != nil {
			reasons = append(reasons, "deployment manifest invalid")
			continue
		}
		if deployment.SchemaVersion != 1 || deployment.CampaignID != config.CampaignID || seen[deployment.DeploymentID] || !deployment.FreshDeployment || !validSHA256(deployment.WindowsEnvironmentSHA256) || deployment.StartedAt == "" || deployment.FinishedAt == "" || deployment.ExitStatus != 0 || deployment.SwapInDelta != 0 || deployment.SwapOutDelta != 0 || deployment.OOM || deployment.UnexpectedContainerRestarts != 0 {
			reasons = append(reasons, "deployment acceptance failed: "+deployment.DeploymentID)
		}
		seen[deployment.DeploymentID] = true
		if windowsEnvironmentSHA256 == "" {
			windowsEnvironmentSHA256 = deployment.WindowsEnvironmentSHA256
		} else if deployment.WindowsEnvironmentSHA256 != windowsEnvironmentSHA256 {
			reasons = append(reasons, "Windows environment digest changed across deployments")
		}
		for suffix, expected := range map[string]string{"vmstat-before.txt": deployment.VMStatBeforeSHA256, "vmstat-after.txt": deployment.VMStatAfterSHA256} {
			actual, hashErr := FileSHA256(filepath.Join(runDir, "environment", deployment.DeploymentID+"."+suffix))
			if hashErr != nil || !validSHA256(expected) || actual != expected {
				reasons = append(reasons, "vmstat evidence mismatch: "+deployment.DeploymentID)
			}
		}
		environmentPath := filepath.Join(runDir, "environment", deployment.DeploymentID+".json")
		digest, digestErr := FileSHA256(environmentPath)
		if digestErr != nil || digest != deployment.EnvironmentSHA256 {
			reasons = append(reasons, "environment digest mismatch: "+deployment.DeploymentID)
			continue
		}
		environmentValue, readErr := os.ReadFile(environmentPath)
		var environment EnvironmentManifest
		if readErr != nil || StrictJSON(environmentValue, &environment) != nil || environment.CampaignID != config.CampaignID || environment.DeploymentID != deployment.DeploymentID || environment.GitCommit != config.SubmissionCommit || len(environment.GitStatus) != 0 || !environment.PublicationEligible || len(environment.Datasets) == 0 {
			reasons = append(reasons, "environment acceptance failed: "+deployment.DeploymentID)
			continue
		}
		volume, volumeOK := environment.Datasets["deployment_volume_id_sha256"].(string)
		dataset, datasetOK := environment.Datasets["dataset_sha256"].(string)
		catalog, catalogOK := environment.Datasets["catalog_sha256"].(string)
		if !volumeOK || !datasetOK || !catalogOK || !validSHA256(volume) || !validSHA256(dataset) || !validSHA256(catalog) || seenVolumes[volume] {
			reasons = append(reasons, "dataset/volume digest acceptance failed: "+deployment.DeploymentID)
		} else {
			seenVolumes[volume] = true
		}
	}
	for number := 1; number <= config.Deployments; number++ {
		if !seen[fmt.Sprintf("deployment-%02d", number)] {
			reasons = append(reasons, "required deployment identity missing")
		}
	}
	return reasons
}

func writeSummaryArtifacts(runDir string, summary Summary) error {
	generated := filepath.Join(runDir, "generated")
	if err := os.MkdirAll(filepath.Join(generated, "latex"), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(generated, "figures"), 0o700); err != nil {
		return err
	}
	value, _ := json.MarshalIndent(summary, "", "  ")
	value = append(value, '\n')
	for _, name := range []string{"summary.json", "paper-results.json"} {
		if err := writeExclusive(filepath.Join(generated, name), value); err != nil {
			return err
		}
	}
	file, err := os.OpenFile(filepath.Join(generated, "summary.csv"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w := csv.NewWriter(file)
	_ = w.Write([]string{"cell_id", "n", "p50_ms", "p95_ms", "failed", "invalid"})
	keys := make([]string, 0, len(summary.Cells))
	for key := range summary.Cells {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		d := summary.Cells[key]
		_ = w.Write([]string{key, strconv.Itoa(d.N), fmt.Sprintf("%.6f", d.P50), fmt.Sprintf("%.6f", d.P95), strconv.Itoa(d.Failed), strconv.Itoa(d.Invalid)})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	latex := []byte("% TaskGate final V5 evidence is incomplete; no paper-ready numeric macros were generated.\n")
	if summary.Status == "pass" {
		latex = []byte("% Validated publication campaign. Transfer to the manuscript remains an explicit author step.\n")
		for _, key := range keys {
			d := summary.Cells[key]
			macro := "TaskGateFinal"
			for _, r := range key {
				if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
					macro += string(r)
				}
			}
			latex = append(latex, []byte(fmt.Sprintf("\\newcommand{\\%sN}{%d}\n\\newcommand{\\%sPFTwo}{%.6f}\n\\newcommand{\\%sPFNinetyFive}{%.6f}\n", macro, d.N, macro, d.P50, macro, d.P95))...)
		}
	}
	if err := writeExclusive(filepath.Join(generated, "latex", "evidence.tex"), latex); err != nil {
		return err
	}
	if summary.Status == "pass" {
		var svg strings.Builder
		svg.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" width="960" height="` + strconv.Itoa(40+len(keys)*28) + `" role="img"><title>TaskGate final V5 p95 latency by cell</title><style>text{font:12px sans-serif}.bar{fill:#3569b8}</style>`)
		max := 0.0
		for _, key := range keys {
			if summary.Cells[key].P95 > max {
				max = summary.Cells[key].P95
			}
		}
		if max <= 0 {
			max = 1
		}
		for i, key := range keys {
			d := summary.Cells[key]
			y := 30 + i*28
			width := int(600 * d.P95 / max)
			svg.WriteString(fmt.Sprintf(`<text x="5" y="%d">%s</text><rect class="bar" x="310" y="%d" width="%d" height="14"/><text x="%d" y="%d">%.3f ms</text>`, y, xmlEscape(key), y-12, width, 318+width, y, d.P95))
		}
		svg.WriteString(`</svg>`)
		if err := writeExclusive(filepath.Join(generated, "figures", "latency-p95.svg"), []byte(svg.String())); err != nil {
			return err
		}
	}
	return nil
}

func xmlEscape(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return replacer.Replace(value)
}

func SealRun(runDir string) (EvidenceManifest, error) {
	value, err := os.ReadFile(filepath.Join(runDir, "generated", "summary.json"))
	if err != nil {
		return EvidenceManifest{}, err
	}
	var summary Summary
	if err := StrictJSON(value, &summary); err != nil {
		return EvidenceManifest{}, err
	}
	if summary.Status != "pass" || !summary.PublicationEligible {
		return EvidenceManifest{}, errors.New("only a passing publication campaign can be sealed")
	}
	var paths []string
	err = filepath.WalkDir(runDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(runDir, path)
		if rel == filepath.Join("evidence", "manifest.json") {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return EvidenceManifest{}, err
	}
	manifest, err := BuildEvidenceManifest(runDir, summary.CampaignID, summary.SubmissionCommit, paths)
	if err != nil {
		return manifest, err
	}
	encoded, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.MkdirAll(filepath.Join(runDir, "evidence"), 0o700); err != nil {
		return manifest, err
	}
	err = writeExclusive(filepath.Join(runDir, "evidence", "manifest.json"), append(encoded, '\n'))
	return manifest, err
}

func writeExclusive(path string, value []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = file.Write(value); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
