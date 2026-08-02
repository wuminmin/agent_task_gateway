package experiment

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
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
	DeploymentID        string   `json:"deployment_id"`
	WorkloadID          string   `json:"workload_id"`
	Scale               string   `json:"scale"`
	NumeratorMode       string   `json:"numerator_mode"`
	DenominatorMode     string   `json:"denominator_mode"`
	N                   int      `json:"n"`
	MedianPairRatio     float64  `json:"median_pair_ratio"`
	P95PairRatio        float64  `json:"p95_pair_ratio"`
	MedianDifferenceMS  float64  `json:"median_difference_ms"`
	RatioBootstrap      Interval `json:"ratio_bootstrap"`
	DifferenceBootstrap Interval `json:"difference_bootstrap"`
}
type DeploymentManifest struct {
	SchemaVersion               int    `json:"schema_version"`
	CampaignID                  string `json:"campaign_id"`
	DeploymentID                string `json:"deployment_id"`
	FreshDeployment             bool   `json:"fresh_deployment"`
	FreshDeploymentProofSHA256  string `json:"fresh_deployment_proof_sha256"`
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

type DockerVolumeProof struct {
	Name          string `json:"name"`
	CreatedAt     string `json:"created_at"`
	Driver        string `json:"driver"`
	InspectSHA256 string `json:"inspect_sha256"`
}

type FreshDeploymentProof struct {
	SchemaVersion                int                 `json:"schema_version"`
	CampaignID                   string              `json:"campaign_id"`
	DeploymentID                 string              `json:"deployment_id"`
	CapturedAt                   string              `json:"captured_at"`
	ComposeProjectName           string              `json:"compose_project_name"`
	ComposeConfigSHA256          string              `json:"compose_config_sha256"`
	Volumes                      []DockerVolumeProof `json:"volumes"`
	VolumeSetSHA256              string              `json:"volume_set_sha256"`
	VolumeInspectSHA256          string              `json:"volume_inspect_sha256"`
	ControlPGSystemIdentifier    string              `json:"control_pg_system_identifier"`
	BusinessPGSystemIdentifier   string              `json:"business_pg_system_identifier"`
	ControlInitialCounts         map[string]int64    `json:"control_initial_counts"`
	DatasetFingerprintSHA256     string              `json:"dataset_fingerprint_sha256"`
	MinIOInitialObjectCount      int64               `json:"minio_initial_object_count"`
	SnapshotArtifactVolumeSHA256 string              `json:"snapshot_artifact_volume_sha256"`
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
	Notices             []string                `json:"notices,omitempty"`
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
	type pairObservation struct {
		deployment string
		workload   string
		scale      string
		order      string
		values     map[string]float64
	}
	pairValues := map[string]*pairObservation{}
	resultPairs := map[string]map[string]string{}
	factSetPairs := map[string]map[string]map[string]bool{}
	attackDirectionPairs := map[string]map[string]bool{}
	rlsStepResults := map[string]map[string]string{}
	deployments := map[string]bool{}
	seenFreshRoots := map[string]bool{}
	rootAnchors := map[string]string{}
	verifyRealEvidence := config.CampaignClass == "publication" || config.PilotKind == "real_system"
	for _, sample := range samples {
		if sample.CampaignID != config.CampaignID || sample.ExperimentID != config.ExperimentID {
			return summary, errors.New("raw sample campaign/experiment mismatch")
		}
		deployments[sample.DeploymentID] = true
		rootKey := sample.DeploymentID + "\x00" + sample.WorkloadID + "\x00" + sample.Scale + "\x00" + strconv.Itoa(sample.ProcessReplicate) + "\x00" + strconv.Itoa(sample.Iteration) + "\x00" + sample.RootGroupID
		if verifyRealEvidence && sample.Status == "pass" && sample.System == "taskgate" {
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
		if sample.Status == "pass" && sample.ExperimentID == "rls" {
			for _, step := range sample.Trace {
				if step.Rejected {
					continue
				}
				key := strings.Join([]string{sample.DeploymentID, sample.WorkloadID, sample.Scale, strconv.Itoa(sample.ProcessReplicate), strconv.Itoa(sample.Iteration), strconv.Itoa(step.Index)}, "\x00")
				if rlsStepResults[key] == nil {
					rlsStepResults[key] = map[string]string{}
				}
				rlsStepResults[key][sample.Mode] = step.ResultSHA256
			}
		}
		d := summary.Cells[sample.CellID]
		switch sample.Status {
		case "pass":
			byCell[sample.CellID] = append(byCell[sample.CellID], sample.ClientFullDrainMS)
			if byDeployment[sample.CellID] == nil {
				byDeployment[sample.CellID] = map[string][]float64{}
			}
			byDeployment[sample.CellID][sample.DeploymentID] = append(byDeployment[sample.CellID][sample.DeploymentID], sample.ClientFullDrainMS)
			pair := pairValues[sample.PairID]
			if pair == nil {
				pair = &pairObservation{deployment: sample.DeploymentID, workload: sample.WorkloadID, scale: sample.Scale, order: sample.PairedSystemOrder, values: map[string]float64{}}
				pairValues[sample.PairID] = pair
			}
			if pair.deployment != sample.DeploymentID || pair.workload != sample.WorkloadID || pair.scale != sample.Scale || pair.order != sample.PairedSystemOrder {
				summary.Status = "fail"
				summary.Reasons = append(summary.Reasons, "matched-pair identity metadata is inconsistent")
			} else if _, duplicate := pair.values[sample.Mode]; duplicate {
				summary.Status = "fail"
				summary.Reasons = append(summary.Reasons, "matched pair contains a duplicate mode")
			} else {
				pair.values[sample.Mode] = sample.ClientFullDrainMS
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
		if verifyRealEvidence && sample.System == "taskgate" && sample.Status == "pass" && requiresArtifactEvidence(sample) {
			if sample.Rejected {
				if !sample.RejectedNoResult || !sample.RejectedNoArtifact || !sample.RejectedNoSuccessfulAudit || sample.ArtifactSHA256 != "" || sample.ObjectSHA256 != "" || sample.AvailabilityAuditSHA256 != "" {
					summary.Status = "fail"
					summary.Reasons = append(summary.Reasons, "rejected request produced artifact evidence")
				}
			} else if sample.ReceiptVersion != "8" || !sample.ReceiptVerified || !sample.ArtifactAvailable || !validSHA256(sample.ReceiptSHA256) || !validSHA256(sample.ArtifactIntentSHA256) || !validSHA256(sample.AvailabilityAuditSHA256) {
				summary.Reasons = append(summary.Reasons, "TaskGate pass sample lacks verified V8/AVAILABLE evidence")
			}
		}
		if verifyRealEvidence && sample.Status == "pass" {
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
	for _, modes := range rlsStepResults {
		baseline, baselineOK := modes["rls"]
		unlimited, unlimitedOK := modes["unlimited"]
		if baselineOK && unlimitedOK && baseline != unlimited {
			summary.Status = "fail"
			summary.Reasons = append(summary.Reasons, "RLS and unlimited TaskGate prefix result mismatch")
		}
		if bounded, boundedOK := modes["bounded"]; boundedOK && (!baselineOK || bounded != baseline) {
			summary.Status = "fail"
			summary.Reasons = append(summary.Reasons, "bounded TaskGate prefix differs from RLS")
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
	type pairedSeries struct{ ratios, differences []float64 }
	paired := map[string]*pairedSeries{}
	for _, pair := range pairValues {
		direct, okDirect := pair.values["direct"]
		if !okDirect || direct <= 0 {
			continue
		}
		expectedModes := directPairModes(config, pair.workload)
		if len(expectedModes) == 0 || !sameStringSet(expectedModes, mapKeys(pair.values)) || !sameStringSet(expectedModes, strings.Split(pair.order, ",")) {
			summary.Status = "fail"
			summary.Reasons = append(summary.Reasons, "matched pair is incomplete or its recorded arm order is invalid")
			continue
		}
		for mode, value := range pair.values {
			if mode == "direct" {
				continue
			}
			key := strings.Join([]string{pair.deployment, pair.workload, pair.scale, mode}, "\x00")
			series := paired[key]
			if series == nil {
				series = &pairedSeries{}
				paired[key] = series
			}
			series.ratios = append(series.ratios, value/direct)
			series.differences = append(series.differences, value-direct)
		}
	}
	for key, series := range paired {
		parts := strings.Split(key, "\x00")
		statistics := summarizePairedSeries(series.ratios, series.differences)
		statistics.DeploymentID = parts[0]
		statistics.WorkloadID = parts[1]
		statistics.Scale = parts[2]
		statistics.NumeratorMode = parts[3]
		statistics.DenominatorMode = "direct"
		summary.PairedRatios = append(summary.PairedRatios, statistics)
	}
	sort.Slice(summary.PairedRatios, func(i, j int) bool {
		left := summary.PairedRatios[i].DeploymentID + summary.PairedRatios[i].WorkloadID + summary.PairedRatios[i].Scale + summary.PairedRatios[i].NumeratorMode
		right := summary.PairedRatios[j].DeploymentID + summary.PairedRatios[j].WorkloadID + summary.PairedRatios[j].Scale + summary.PairedRatios[j].NumeratorMode
		return left < right
	})
	if config.CampaignClass != "publication" {
		summary.Notices = append(summary.Notices, "pilot campaigns are never publication eligible")
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
	if len(summary.Reasons) == 0 {
		summary.Status = "pass"
		summary.PublicationEligible = config.CampaignClass == "publication"
	}
	if err := writeSummaryArtifacts(runDir, summary); err != nil {
		return summary, err
	}
	return summary, nil
}

func directPairModes(config Config, workloadID string) []string {
	for _, workload := range config.Workloads {
		if workload.ID != workloadID {
			continue
		}
		for _, group := range dependencyAwareModeGroups(workload.Modes) {
			if containsMode(group, "direct") {
				return append([]string(nil), group...)
			}
		}
	}
	return nil
}

func mapKeys(values map[string]float64) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	return result
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	counts := map[string]int{}
	for _, value := range left {
		counts[value]++
	}
	for _, value := range right {
		counts[value]--
	}
	for _, count := range counts {
		if count != 0 {
			return false
		}
	}
	return true
}

func summarizePairedSeries(ratios, differences []float64) PairedRatio {
	medianRatio, _ := Type7(ratios, .5)
	p95Ratio, _ := Type7(ratios, .95)
	medianDifference, _ := Type7(differences, .5)
	ratioCI, _ := BootstrapMedian(ratios, 20260801, 2000)
	differenceCI, _ := BootstrapMedian(differences, 20260801, 2000)
	return PairedRatio{
		N:                   len(ratios),
		MedianPairRatio:     medianRatio,
		P95PairRatio:        p95Ratio,
		MedianDifferenceMS:  medianDifference,
		RatioBootstrap:      ratioCI,
		DifferenceBootstrap: differenceCI,
	}
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
		if sample.System == "taskgate" {
			if err := validateBaselineVerification(sample); err != nil {
				fail("baseline independent verification failed: " + err.Error())
			}
			if sample.Mode == "pending_recovery" {
				if err := validateRecoveryVerification(sample); err != nil {
					fail("baseline recovery verification failed: " + err.Error())
				}
			}
		}
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
			if err := validateRLSVerification(sample, expectedSteps); err != nil {
				fail("RLS independent verification failed: " + err.Error())
			}
		} else if len(sample.Trace) == 0 {
			fail("adaptive attack sample lacks request trace evidence")
		}
		if sample.ExperimentID == "attack" {
			if err := validateAttackVerification(sample); err != nil {
				fail("adaptive attack independent verification failed: " + err.Error())
			}
		}
		validateTrace(sample, fail)
	case "provsql":
		if sample.System == "taskgate" && (sample.GenerationBoundaryMS <= 0 || sample.FullTaskGateMS <= 0) {
			fail("ProvSQL-paired TaskGate sample lacks both measurement boundaries")
		}
		if err := validateProvSQLVerification(sample); err != nil {
			fail("ProvSQL independent verification failed: " + err.Error())
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
		requireDiagnostics("total", "recursive_expansion", "parse_validation", "compile_semantic", "plan_materialization", "digest_generation")
		requireCounters("alloc_bytes", "alloc_objects")
		if err := validateCompilerVerification(sample); err != nil {
			fail("compiler independent verification failed: " + err.Error())
		}
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
		if sample.Mode != "serial" {
			if err := validateConcurrencyVerification(sample); err != nil {
				fail("concurrency independent verification failed: " + err.Error())
			}
		}
	case "rq5":
		if err := validateRQ5Verification(sample); err != nil {
			fail("RQ5 independent verification failed: " + err.Error())
		}
	}
	return reasons, failed
}

func validateRLSVerification(sample Sample, expectedSteps int) error {
	evidence := sample.RLSVerification
	if evidence == nil || !evidence.RelRowSecurity || evidence.BaselineRole == "" || evidence.TableOwnerRole == "" ||
		evidence.BaselineRoleIsOwner || evidence.BaselineRoleBypassRLS || evidence.BaselineRole == evidence.TableOwnerRole ||
		!json.Valid(evidence.PoliciesJSON) || sha256Hex(evidence.PoliciesJSON) != evidence.PoliciesSHA256 {
		return errors.New("PostgreSQL RLS role/policy evidence is absent or unsafe")
	}
	if len(evidence.OracleTrace) != expectedSteps {
		return errors.New("independent oracle trace length differs from preregistration")
	}
	recomputed, err := finalv5oracle.Evaluate(evidence.OracleTrace)
	if err != nil || recomputed != evidence.OracleResult {
		return errors.New("independent 70% oracle result does not recompute")
	}
	if sample.Mode == "bounded" {
		if !evidence.OracleComputedBefore || evidence.StopReason != "EXPOSURE_BUDGET_EXHAUSTED" {
			return errors.New("bounded arm was not stopped by a precomputed exposure budget")
		}
	} else if sample.Mode == "unlimited" {
		if sample.ReleaseSetSHA256 != recomputed.Release.SetSHA256 || sample.DependencySetSHA256 != recomputed.Dependency.SetSHA256 || sample.OutcomeSetSHA256 != recomputed.Outcome.SetSHA256 {
			return errors.New("unlimited TaskGate union differs from independent oracle")
		}
	}
	return nil
}

func validateAttackVerification(sample Sample) error {
	evidence := sample.AttackVerification
	if evidence == nil {
		return errors.New("raw attack verification evidence is absent")
	}
	switch {
	case strings.HasPrefix(sample.WorkloadID, "A-") || strings.HasPrefix(sample.WorkloadID, "D-"):
		complete, err := finalv5oracle.Evaluate([]finalv5oracle.Observation{evidence.CompleteObservation})
		if err != nil {
			return err
		}
		split, err := finalv5oracle.Evaluate(evidence.SplitObservations)
		if err != nil || complete.Release != split.Release || complete.Dependency != split.Dependency || complete.Outcome != split.Outcome {
			return errors.New("complete and split/page exact unions differ")
		}
	case strings.HasPrefix(sample.WorkloadID, "B-"):
		if len(evidence.NormalFormSHA256) < 2 {
			return errors.New("equivalent-SQL variants are absent")
		}
		for _, digest := range evidence.NormalFormSHA256 {
			if !validSHA256(digest) || digest != evidence.NormalFormSHA256[0] {
				return errors.New("equivalent-SQL normal forms differ")
			}
		}
	case strings.HasPrefix(sample.WorkloadID, "C-"):
		if evidence.QueryRecordsSameID != evidence.QueryRecordsBefore || evidence.SettlementsSameID != evidence.SettlementsBefore ||
			evidence.QueryRecordsDifferentID != evidence.QueryRecordsSameID+1 || evidence.SettlementsDifferentID != evidence.SettlementsSameID+1 {
			return errors.New("request-ID replay settlement counts are inconsistent")
		}
	case strings.HasPrefix(sample.WorkloadID, "E-"):
		if len(evidence.ExpectedThresholds) == 0 || !equalInt64s(evidence.ExpectedThresholds, evidence.ObservedThresholds) || evidence.OutcomeCeiling <= 0 || evidence.ObservedOutcome > evidence.OutcomeCeiling {
			return errors.New("threshold sequence or Outcome ceiling differs from preregistration")
		}
	default:
		return errors.New("unknown preregistered attack workload")
	}
	return nil
}

func validateProvSQLVerification(sample Sample) error {
	evidence := sample.ProvSQLVerification
	if evidence == nil || evidence.Version == "" || !fullSHA.MatchString(evidence.Commit) || evidence.AggTokenOID == 0 || evidence.GateType == "" ||
		len(evidence.Nonces) < 2 || len(evidence.Nonces) != len(evidence.GateCardinalities) || len(evidence.Nonces) != len(evidence.RepresentationSHA256) {
		return errors.New("ProvSQL version/type/gate sequence evidence is incomplete")
	}
	for _, digest := range []string{evidence.SQLSHA256, evidence.DatasetSHA256, evidence.CacheConditionSHA256, evidence.ExecutionOrderSHA256} {
		if !validSHA256(digest) {
			return errors.New("ProvSQL paired execution binding is invalid")
		}
	}
	nonces, representations := map[string]bool{}, map[string]bool{}
	for index := range evidence.Nonces {
		if evidence.Nonces[index] == "" || nonces[evidence.Nonces[index]] || !validSHA256(evidence.RepresentationSHA256[index]) || representations[evidence.RepresentationSHA256[index]] ||
			(index > 0 && evidence.GateCardinalities[index] <= evidence.GateCardinalities[index-1]) {
			return errors.New("ProvSQL nonce/gate growth/representation sequence is not unique")
		}
		nonces[evidence.Nonces[index]], representations[evidence.RepresentationSHA256[index]] = true, true
	}
	return nil
}

func validateCompilerVerification(sample Sample) error {
	evidence := sample.CompilerVerification
	if evidence == nil {
		return errors.New("compiler equivalence evidence is absent")
	}
	if sample.Mode == "structured_rejection" {
		if sample.Scale == "depth-17" && (evidence.StructuredErrorCode != "VIEW_DEPTH_LIMIT" || evidence.ObservedDepth != 17) {
			return errors.New("depth-17 did not return the preregistered structured error")
		}
		if sample.Scale == "sources-17" && (evidence.StructuredErrorCode != "VIEW_SOURCE_LIMIT" || evidence.ObservedSources != 17) {
			return errors.New("sources-17 did not return the preregistered structured error")
		}
		return nil
	}
	if !validSHA256(evidence.NestedResultSHA256) || evidence.NestedResultSHA256 != evidence.DirectResultSHA256 || len(evidence.CanonicalPlanSHA256) < 3 {
		return errors.New("nested/direct result equivalence is absent")
	}
	for _, digest := range evidence.CanonicalPlanSHA256 {
		if !validSHA256(digest) || digest != evidence.CanonicalPlanSHA256[0] {
			return errors.New("alias/join-order/parenthesis canonical plans differ")
		}
	}
	return nil
}

func validateConcurrencyVerification(sample Sample) error {
	evidence := sample.ConcurrencyVerification
	width, err := strconv.ParseInt(sample.Scale, 10, 64)
	if evidence == nil || err != nil || width < 2 || evidence.BudgetLimit <= 0 || evidence.UsageBefore != evidence.BudgetLimit-1 ||
		evidence.Accepted != 1 || evidence.Rejected != width-1 || evidence.UsageAfter != evidence.BudgetLimit || evidence.ChargedWinners != 1 {
		return errors.New("B-1 boundary did not yield exactly one charged winner")
	}
	if len(evidence.FinalRootFacts) == 0 || canonicalStringSetSHA256(evidence.FinalRootFacts) != evidence.FinalRootSetSHA256 {
		return errors.New("final root set digest does not recompute")
	}
	return nil
}

func validateRQ5Verification(sample Sample) error {
	evidence := sample.RQ5Verification
	if evidence == nil {
		return errors.New("raw publication transition evidence is absent")
	}
	for _, digest := range []string{evidence.OldPublicationSHA256, evidence.NewPublicationSHA256, evidence.OldTaskRouteSHA256, evidence.NewTaskRouteSHA256, evidence.OldLedgerBeforeSHA256, evidence.OldLedgerAfterSHA256, evidence.CrossReplaySourceSHA256, evidence.CrossReplayTargetSHA256, evidence.ChildPublicationSHA256, evidence.RootPublicationSHA256} {
		if !validSHA256(digest) {
			return errors.New("publication transition contains an invalid digest")
		}
	}
	if evidence.OldPublicationSHA256 == evidence.NewPublicationSHA256 || evidence.OldTaskRouteSHA256 != evidence.OldPublicationSHA256 || evidence.NewTaskRouteSHA256 != evidence.NewPublicationSHA256 ||
		evidence.OldLedgerBeforeSHA256 != evidence.OldLedgerAfterSHA256 || evidence.CrossReplaySourceSHA256 != evidence.OldPublicationSHA256 || evidence.CrossReplayTargetSHA256 != evidence.NewPublicationSHA256 || evidence.CrossPublicationReplayHit || evidence.ChildPublicationSHA256 != evidence.RootPublicationSHA256 {
		return errors.New("publication transition invariants do not hold")
	}
	return nil
}

func equalInt64s(left, right []int64) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func canonicalStringSetSHA256(values []string) string {
	unique := map[string]bool{}
	for _, value := range values {
		unique[value] = true
	}
	ordered := make([]string, 0, len(unique))
	for value := range unique {
		ordered = append(ordered, value)
	}
	sort.Strings(ordered)
	hash := sha256.New()
	_, _ = hash.Write([]byte("TASKGATE-FINAL-V5-ROOT-SET-V1\x00"))
	for _, value := range ordered {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func validateBaselineVerification(sample Sample) error {
	evidence := sample.BaselineVerification
	if evidence == nil {
		return errors.New("raw V8/audit verification evidence is absent")
	}
	verifier, err := evidence.KeyBundle.Verifier()
	if err != nil {
		return err
	}
	if err := verifier.Verify(evidence.Receipt); err != nil {
		return err
	}
	if err := queryreceipt.VerifyAuditInclusion(evidence.Receipt, evidence.AuditProof); err != nil {
		return err
	}
	if err := queryreceipt.VerifyArtifactIntentInclusion(evidence.Receipt, evidence.TerminalProof, evidence.RegistrationProof); err != nil {
		return err
	}
	if err := queryreceipt.VerifyArtifactAvailabilityInclusion(evidence.Receipt, evidence.AvailabilityProof); err != nil {
		return err
	}
	if evidence.ArtifactStatus != "AVAILABLE" || evidence.Receipt.Version != queryreceipt.VersionV8 || evidence.Receipt.ArtifactIntent == nil || evidence.Receipt.Exposure == nil {
		return errors.New("verified evidence is not AVAILABLE V8 exposure evidence")
	}
	receiptBytes, _ := json.Marshal(evidence.Receipt)
	availabilityBytes, _ := json.Marshal(evidence.AvailabilityProof)
	if sha256Hex(receiptBytes) != sample.ReceiptSHA256 || sha256Hex(availabilityBytes) != sample.AvailabilityAuditSHA256 {
		return errors.New("raw receipt/audit digest differs from sample")
	}
	intent, exposure := evidence.Receipt.ArtifactIntent, evidence.Receipt.Exposure
	if intent.IntentSHA256 != sample.ArtifactIntentSHA256 || intent.ParquetSHA256 != sample.ArtifactSHA256 || intent.ObjectSHA256 != sample.ObjectSHA256 || intent.ParquetSize != sample.ParquetBytes || intent.ObjectSize != sample.EncryptedObjectBytes ||
		evidence.DownloadedParquetSHA256 != intent.ParquetSHA256 || evidence.ParsedResultSHA256 != sample.ResultSHA256 || evidence.Receipt.ResultHash != intent.ParquetSHA256 || evidence.Receipt.RowCount != sample.RowCount {
		return errors.New("artifact/result evidence differs from signed receipt")
	}
	if exposure.ReleaseSetSHA256 != sample.ReleaseSetSHA256 || exposure.InfluenceSetSHA256 != sample.DependencySetSHA256 || exposure.OutcomeSetSHA256 != sample.OutcomeSetSHA256 || exposure.ActualReleaseFacts != sample.ActualReleaseFacts || exposure.ActualInfluenceFacts != sample.ActualDependencyFacts || exposure.ActualOutcomeFacts != sample.ActualOutcomeFacts || exposure.ChargedReleaseFacts != sample.ChargedReleaseFacts || exposure.ChargedInfluenceFacts != sample.ChargedDependencyFacts || exposure.ChargedOutcomeFacts != sample.ChargedOutcomeFacts || exposure.RootEpoch != sample.RootEpochAfter {
		return errors.New("exposure evidence differs from signed receipt")
	}
	if sha256Hex([]byte(sample.CampaignID+"\x00"+sample.DeploymentID+"\x00"+evidence.Receipt.TaskID)) != sample.RootTaskIDHash {
		return errors.New("salted root identity differs from receipt task")
	}
	return nil
}

func validateRecoveryVerification(sample Sample) error {
	evidence := sample.RecoveryVerification
	if evidence == nil {
		return errors.New("raw recovery counters are absent")
	}
	if !evidence.FailureObserved || !evidence.CanonicalObjectObserved ||
		evidence.ArtifactStatusBefore != "PENDING" || evidence.ArtifactStatusAfter != "AVAILABLE" {
		return errors.New("forced PENDING transition was not observed")
	}
	if evidence.BusinessCallsAtFailure <= evidence.BusinessCallsBefore ||
		evidence.BusinessCallsAfter != evidence.BusinessCallsAtFailure {
		return errors.New("artifact recovery re-executed Business PostgreSQL")
	}
	if evidence.QueryRecordsAtFailure-evidence.QueryRecordsBefore != 1 ||
		evidence.QueryRecordsAfter != evidence.QueryRecordsAtFailure {
		return errors.New("artifact recovery created another query record")
	}
	if evidence.SettlementsAtFailure != 1 || evidence.SettlementsAfter != evidence.SettlementsAtFailure {
		return errors.New("artifact recovery repeated exposure settlement")
	}
	if evidence.UsedQueriesAtFailure-evidence.UsedQueriesBefore != 1 ||
		evidence.UsedQueriesAfter != evidence.UsedQueriesAtFailure {
		return errors.New("artifact recovery charged the task twice")
	}
	if !validSHA256(evidence.ReceiptSHA256AtFailure) || evidence.ReceiptSHA256After != evidence.ReceiptSHA256AtFailure ||
		!validSHA256(evidence.IntentSHA256AtFailure) || evidence.IntentSHA256After != evidence.IntentSHA256AtFailure ||
		evidence.ReceiptSHA256After != sample.ReceiptSHA256 || evidence.IntentSHA256After != sample.ArtifactIntentSHA256 {
		return errors.New("artifact recovery changed the signed V8 intent")
	}
	return nil
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
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
		if step.Index != index+1 || strings.TrimSpace(step.ConcreteSQL) == "" || !validSHA256(step.PriorStateSHA256) || (!step.Rejected && !validSHA256(step.ResultSHA256)) || !validSHA256(step.NextSQLSHA256) {
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
	seenControlSystems := map[string]bool{}
	seenBusinessSystems := map[string]bool{}
	seenComposeProjects := map[string]bool{}
	windowsEnvironmentSHA256 := ""
	windowsHostPath := filepath.Join(runDir, "environment", "windows-host.json")
	windowsHostDigest, windowsHostErr := FileSHA256(windowsHostPath)
	if windowsHostErr != nil {
		reasons = append(reasons, "Windows host manifest bytes are missing")
	}
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
		if windowsHostErr == nil && windowsHostDigest != deployment.WindowsEnvironmentSHA256 {
			reasons = append(reasons, "Windows host manifest digest mismatch")
		}
		proofPath := filepath.Join(runDir, "environment", deployment.DeploymentID+".fresh.json")
		proofDigest, proofDigestErr := FileSHA256(proofPath)
		proof, proofReasons := readFreshDeploymentProof(proofPath, runDir, config, deployment.DeploymentID)
		if proofDigestErr != nil || proofDigest != deployment.FreshDeploymentProofSHA256 {
			reasons = append(reasons, "fresh-deployment proof digest mismatch: "+deployment.DeploymentID)
		}
		reasons = append(reasons, proofReasons...)
		if proof.SchemaVersion == 1 {
			if seenVolumes[proof.VolumeSetSHA256] || seenControlSystems[proof.ControlPGSystemIdentifier] || seenBusinessSystems[proof.BusinessPGSystemIdentifier] || seenComposeProjects[proof.ComposeProjectName] {
				reasons = append(reasons, "fresh-deployment identity reused: "+deployment.DeploymentID)
			}
			seenVolumes[proof.VolumeSetSHA256] = true
			seenControlSystems[proof.ControlPGSystemIdentifier] = true
			seenBusinessSystems[proof.BusinessPGSystemIdentifier] = true
			seenComposeProjects[proof.ComposeProjectName] = true
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
		if readErr != nil || StrictJSON(environmentValue, &environment) != nil || environment.CampaignID != config.CampaignID || environment.DeploymentID != deployment.DeploymentID || environment.GitCommit != config.SubmissionCommit || len(environment.GitStatus) != 0 || !environment.PublicationEligible || len(environment.Datasets) == 0 || !validEnvironmentSections(environment) {
			reasons = append(reasons, "environment acceptance failed: "+deployment.DeploymentID)
			continue
		}
		dataset, datasetOK := environment.Datasets["dataset_sha256"].(string)
		catalog, catalogOK := environment.Datasets["catalog_sha256"].(string)
		if !datasetOK || !catalogOK || !validSHA256(dataset) || !validSHA256(catalog) {
			reasons = append(reasons, "dataset/catalog digest acceptance failed: "+deployment.DeploymentID)
		}
	}
	for number := 1; number <= config.Deployments; number++ {
		if !seen[fmt.Sprintf("deployment-%02d", number)] {
			reasons = append(reasons, "required deployment identity missing")
		}
	}
	return reasons
}

func readFreshDeploymentProof(path, runDir string, config Config, deploymentID string) (FreshDeploymentProof, []string) {
	value, err := os.ReadFile(path)
	var proof FreshDeploymentProof
	if err != nil || StrictJSON(value, &proof) != nil {
		return proof, []string{"fresh-deployment proof is missing or invalid: " + deploymentID}
	}
	var reasons []string
	if proof.SchemaVersion != 1 || proof.CampaignID != config.CampaignID || proof.DeploymentID != deploymentID || proof.CapturedAt == "" || proof.ComposeProjectName == "" ||
		!validSHA256(proof.ComposeConfigSHA256) || !validSHA256(proof.VolumeSetSHA256) || !validSHA256(proof.VolumeInspectSHA256) ||
		proof.ControlPGSystemIdentifier == "" || proof.BusinessPGSystemIdentifier == "" || !validSHA256(proof.DatasetFingerprintSHA256) ||
		proof.MinIOInitialObjectCount != 0 || !validSHA256(proof.SnapshotArtifactVolumeSHA256) || len(proof.Volumes) < 5 {
		reasons = append(reasons, "fresh-deployment proof acceptance failed: "+deploymentID)
	}
	for _, name := range []string{"tasks", "query_records", "root_heads", "result_artifacts"} {
		if value, ok := proof.ControlInitialCounts[name]; !ok || value != 0 {
			reasons = append(reasons, "fresh Control state is not zero: "+deploymentID)
			break
		}
	}
	seenNames := map[string]bool{}
	volumeSetLines := make([]string, 0, len(proof.Volumes))
	for _, volume := range proof.Volumes {
		if volume.Name == "" || volume.CreatedAt == "" || volume.Driver == "" || !validSHA256(volume.InspectSHA256) || seenNames[volume.Name] {
			reasons = append(reasons, "fresh Docker volume evidence is invalid: "+deploymentID)
			break
		}
		seenNames[volume.Name] = true
		volumeSetLines = append(volumeSetLines, strings.Join([]string{volume.Name, volume.CreatedAt, volume.Driver, volume.InspectSHA256}, "\t")+"\n")
	}
	sort.Strings(volumeSetLines)
	if sha256Hex([]byte(strings.Join(volumeSetLines, ""))) != proof.VolumeSetSHA256 {
		reasons = append(reasons, "Docker volume-set digest does not match proof entries: "+deploymentID)
	}
	inspectPath := filepath.Join(runDir, "environment", deploymentID+".fresh.volume-inspect.json")
	if digest, err := FileSHA256(inspectPath); err != nil || digest != proof.VolumeInspectSHA256 {
		reasons = append(reasons, "Docker volume inspect bytes do not match proof: "+deploymentID)
	} else {
		inspectBytes, _ := os.ReadFile(inspectPath)
		var objects []map[string]any
		if StrictJSON(inspectBytes, &objects) != nil || len(objects) != len(proof.Volumes) {
			reasons = append(reasons, "Docker volume inspect set is invalid: "+deploymentID)
		} else {
			proofByName := make(map[string]DockerVolumeProof, len(proof.Volumes))
			for _, volume := range proof.Volumes {
				proofByName[volume.Name] = volume
			}
			for _, object := range objects {
				name, _ := object["Name"].(string)
				createdAt, _ := object["CreatedAt"].(string)
				driver, _ := object["Driver"].(string)
				canonical, _ := json.Marshal(object)
				expected, ok := proofByName[name]
				if !ok || createdAt != expected.CreatedAt || driver != expected.Driver || sha256Hex(canonical) != expected.InspectSHA256 {
					reasons = append(reasons, "Docker volume inspect identity differs from proof: "+deploymentID)
					break
				}
			}
		}
	}
	composePath := filepath.Join(runDir, "environment", deploymentID+".fresh.compose-config.yaml")
	if digest, err := FileSHA256(composePath); err != nil || digest != proof.ComposeConfigSHA256 {
		reasons = append(reasons, "Compose config bytes do not match proof: "+deploymentID)
	}
	datasetPath := filepath.Join(runDir, "environment", deploymentID+".fresh.dataset-fingerprint.txt")
	if digest, err := FileSHA256(datasetPath); err != nil || digest != proof.DatasetFingerprintSHA256 {
		reasons = append(reasons, "dataset fingerprint bytes do not match proof: "+deploymentID)
	}
	return proof, reasons
}

func validEnvironmentSections(environment EnvironmentManifest) bool {
	for _, section := range []map[string]any{environment.Host, environment.Software, environment.Storage} {
		if len(section) == 0 {
			return false
		}
		for _, value := range section {
			if text, ok := value.(string); ok && (strings.TrimSpace(text) == "" || strings.HasPrefix(text, "ERROR:")) {
				return false
			}
		}
	}
	return true
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
	if summary.Status == "pass" && summary.PublicationEligible {
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
	if summary.Status == "pass" && summary.PublicationEligible {
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
