package experiment

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"taskbound.local/agent-data-gateway/evaluation/internal/compilerfixture"
	"taskbound.local/agent-data-gateway/evaluation/internal/provsqlfixture"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
	"taskbound.local/agent-data-gateway/internal/viewcompiler"
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

type provSQLSequenceObservation struct {
	order, iteration                        int
	nonce                                   int64
	gatesBefore, gatesAfter                 int64
	artifactBytesBefore, artifactBytesAfter int64
	representation                          string
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
	DeploymentVolumeIDSHA256     string              `json:"deployment_volume_id_sha256"`
	CatalogSHA256                string              `json:"catalog_sha256"`
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

// profileBindingRequired says when a run must bind every arm to a deployment
// profile. Synthetic framework smoke stays exempt; anything that can become
// evidence about a real system does not.
func profileBindingRequired(config Config) bool {
	return config.CampaignClass == "publication" || config.PilotKind == "real_system" ||
		config.PilotKind == "profile_activation_smoke" || config.PilotKind == "artifact_targeted"
}

func appendUniqueReason(reasons []string, reason string) []string {
	for _, existing := range reasons {
		if existing == reason {
			return reasons
		}
	}
	return append(reasons, reason)
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
	expectedRQ5BuildManifestSHA256 := ""
	if config.ExperimentID == "rq5" {
		expectedRQ5BuildManifestSHA256, err = loadRQ5DriverBuildManifestSHA256(runDir)
		if err != nil {
			summary.Status = "fail"
			summary.Reasons = append(summary.Reasons, "RQ5 sealed driver build manifest is absent, non-regular, or unreadable")
		}
	}
	// Profile binding is enforced before any statistic is computed. A run that
	// mixes deployment profiles inside one cell produced an incomparable pair,
	// so the whole cell is invalidated first and the retained evidence keeps
	// every original arm.
	if profileBindingRequired(config) {
		for index := range samples {
			if samples[index].Status != "pass" {
				continue
			}
			if err := RequireProfileBinding(samples[index]); err != nil {
				samples[index].Status = "invalid"
				samples[index].ErrorCode = "profile_binding_missing"
				samples[index].Reason = "a profile-enabled run requires every arm to name the deployment profile it ran against"
				summary.Status = "fail"
				summary.Reasons = appendUniqueReason(summary.Reasons,
					fmt.Sprintf("cell %s sample %s has no deployment profile binding", samples[index].CellID, samples[index].SampleID))
			}
		}
	}
	if updated, affected := InvalidateMismatchedProfileCells(samples); len(affected) != 0 {
		samples = updated
		summary.Status = "fail"
		for _, cell := range affected {
			summary.Reasons = appendUniqueReason(summary.Reasons,
				fmt.Sprintf("cell %s arms ran against different deployment profiles; the whole cell is invalid", cell))
		}
	}
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
	provSQLSequences := map[string][]provSQLSequenceObservation{}
	provSQLPairs := map[string]map[string]*ProvSQLVerificationEvidence{}
	sampleBindingDigests := map[string]map[string]bool{}
	deployments := map[string]bool{}
	seenFreshRoots := map[string]bool{}
	rootAnchors := map[string]string{}
	verifyRealEvidence := config.CampaignClass == "publication" || config.PilotKind == "real_system"
	for _, sample := range samples {
		if sample.CampaignID != config.CampaignID || sample.ExperimentID != config.ExperimentID {
			return summary, errors.New("raw sample campaign/experiment mismatch")
		}
		deployments[sample.DeploymentID] = true
		if digest := sampleAdapterBindingSHA256(sample); digest != "" {
			if sampleBindingDigests[sample.DeploymentID] == nil {
				sampleBindingDigests[sample.DeploymentID] = map[string]bool{}
			}
			sampleBindingDigests[sample.DeploymentID][digest] = true
		}
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
		if sample.Status == "pass" && sample.ExperimentID == "provsql" && sample.ProvSQLVerification != nil {
			if provSQLPairs[resultKey] == nil {
				provSQLPairs[resultKey] = map[string]*ProvSQLVerificationEvidence{}
			}
			provSQLPairs[resultKey][sample.Mode] = sample.ProvSQLVerification
			if sample.Mode == "provsql" {
				sequenceKey := sample.DeploymentID + "\x00" + strconv.Itoa(sample.ProcessReplicate) + "\x00provsql"
				evidence := sample.ProvSQLVerification
				provSQLSequences[sequenceKey] = append(provSQLSequences[sequenceKey], provSQLSequenceObservation{
					order: sample.OrderPosition, iteration: sample.Iteration, nonce: evidence.Nonce,
					gatesBefore: evidence.GatesBefore, gatesAfter: evidence.GatesAfter,
					artifactBytesBefore: evidence.ArtifactBytesBefore, artifactBytesAfter: evidence.ArtifactBytesAfter,
					representation: evidence.RepresentationSHA256,
				})
			}
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
	if provSQLReasons := validateProvSQLCrossEvidence(provSQLPairs, provSQLSequences); len(provSQLReasons) != 0 {
		summary.Status = "fail"
		summary.Reasons = append(summary.Reasons, provSQLReasons...)
	}
	if rq5Reasons := validateRQ5RuntimeIdentityConsistency(samples, expectedRQ5BuildManifestSHA256); len(rq5Reasons) != 0 {
		summary.Status = "fail"
		summary.Reasons = append(summary.Reasons, rq5Reasons...)
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
		environmentBindings, bindingReasons := readRunEnvironmentBindingIdentities(runDir, config)
		summary.Reasons = append(summary.Reasons, bindingReasons...)
		summary.Reasons = append(summary.Reasons,
			validatePublicationSampleBindingDigests(config.ExperimentID, environmentBindings, sampleBindingDigests)...)
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

func validatePublicationSampleBindingDigests(experimentID string, expected map[string]string,
	observed map[string]map[string]bool) []string {
	if experimentID != "scale" && experimentID != "artifact" && experimentID != "provsql" {
		return nil
	}
	var reasons []string
	for deploymentID, digest := range expected {
		one := observed[deploymentID]
		if len(one) != 1 || !one[digest] {
			reasons = append(reasons, "sample/environment strict adapter binding mismatch: "+deploymentID)
		}
	}
	return reasons
}

func sampleAdapterBindingSHA256(sample Sample) string {
	switch sample.ExperimentID {
	case "scale":
		if sample.ScaleVerification != nil && sample.ScaleVerification.Boundary == "dependency_e2e" {
			return sample.ScaleVerification.BindingSHA256
		}
	case "artifact":
		if sample.ArtifactVerification != nil {
			return sample.ArtifactVerification.BindingSHA256
		}
	case "provsql":
		if sample.ProvSQLVerification != nil {
			return sample.ProvSQLVerification.BindingSHA256
		}
	}
	return ""
}

func readRunEnvironmentBindingIdentities(runDir string, config Config) (map[string]string, []string) {
	result := map[string]string{}
	var reasons []string
	for deployment := 1; deployment <= config.Deployments; deployment++ {
		deploymentID := fmt.Sprintf("deployment-%02d", deployment)
		value, err := os.ReadFile(filepath.Join(runDir, "environment", deploymentID+".json"))
		var manifest EnvironmentManifest
		if err != nil || StrictJSON(value, &manifest) != nil {
			reasons = append(reasons, "environment strict adapter binding is unreadable: "+deploymentID)
			continue
		}
		section, _, err := environmentBindingIdentity(manifest)
		if err != nil {
			reasons = append(reasons, "environment strict adapter binding is invalid: "+deploymentID)
			continue
		}
		result[deploymentID] = section
	}
	return result, reasons
}

func validateProvSQLCrossEvidence(pairs map[string]map[string]*ProvSQLVerificationEvidence,
	sequences map[string][]provSQLSequenceObservation) []string {
	var reasons []string
	for _, modes := range pairs {
		if len(modes) != 3 || modes["direct"] == nil || modes["provsql"] == nil || modes["taskgate"] == nil {
			reasons = append(reasons, "ProvSQL three-arm evidence pair is incomplete")
			continue
		}
		anchor := modes["direct"]
		for _, mode := range []string{"provsql", "taskgate"} {
			one := modes[mode]
			if one.Nonce != anchor.Nonce || one.NonceBindingSHA256 != anchor.NonceBindingSHA256 ||
				one.BindingSHA256 != anchor.BindingSHA256 || one.DatasetSHA256 != anchor.DatasetSHA256 ||
				one.LogicalSQLSHA256 != anchor.LogicalSQLSHA256 || one.ExpectedResultSHA256 != anchor.ExpectedResultSHA256 ||
				one.ObservedResultSHA256 != anchor.ObservedResultSHA256 ||
				one.ExpectedDependencyFacts != anchor.ExpectedDependencyFacts ||
				one.ExpectedDependencySHA256 != anchor.ExpectedDependencySHA256 {
				reasons = append(reasons, "ProvSQL three-arm nonce/query/result/FactSet binding mismatch")
			}
		}
		if modes["direct"].PostgreSQLVersion != modes["provsql"].PostgreSQLVersion ||
			modes["direct"].PostgreSQLVersionNum != modes["provsql"].PostgreSQLVersionNum ||
			modes["direct"].UUIDOID != modes["provsql"].UUIDOID {
			reasons = append(reasons, "direct and ProvSQL PostgreSQL builds differ")
		}
	}
	for _, observations := range sequences {
		observations = append([]provSQLSequenceObservation(nil), observations...)
		sort.Slice(observations, func(i, j int) bool { return observations[i].order < observations[j].order })
		seenNonces, seenRepresentations, seenOrders := map[int64]bool{}, map[string]bool{}, map[int]bool{}
		artifactBytesGrew := false
		for index, observation := range observations {
			if seenOrders[observation.order] || seenNonces[observation.nonce] ||
				!validSHA256(observation.representation) || seenRepresentations[observation.representation] ||
				observation.gatesAfter <= observation.gatesBefore || observation.artifactBytesBefore <= 0 ||
				observation.artifactBytesAfter < observation.artifactBytesBefore {
				reasons = append(reasons, "ProvSQL execution-order nonce/representation/growth evidence is not unique and strict")
			}
			artifactBytesGrew = artifactBytesGrew || observation.artifactBytesAfter > observation.artifactBytesBefore
			if index > 0 {
				previous := observations[index-1]
				if observation.gatesBefore < previous.gatesAfter || observation.gatesAfter <= previous.gatesAfter ||
					observation.artifactBytesBefore < previous.artifactBytesAfter || observation.artifactBytesAfter < previous.artifactBytesAfter {
					reasons = append(reasons, "ProvSQL gates or representation bytes regress in actual execution order")
				}
			}
			seenOrders[observation.order] = true
			seenNonces[observation.nonce] = true
			seenRepresentations[observation.representation] = true
		}
		// ProvSQL grows mmap files in allocation chunks, so an individual real
		// query may add gates while file length remains flat. Require monotone
		// bytes per operation and at least one observed allocation growth over
		// the complete retained sequence instead of inventing per-query growth.
		if len(observations) > 0 && !artifactBytesGrew {
			reasons = append(reasons, "ProvSQL representation bytes never grew over the retained sequence")
		}
	}
	return uniqueStrings(reasons)
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
			// Stage A.1 instrumentation is Pilot-only. Publication evidence keeps
			// its frozen protocol while real-system Pilot samples fail closed on
			// every newly required replay/verifier observation.
			if !sample.PublicationEligible {
				if err := validateRedactedVerifierManifest(sample); err != nil {
					fail("baseline redacted verifier manifest failed: " + err.Error())
				}
				switch sample.Mode {
				case "novel":
					if err := validateNovelVerification(sample); err != nil {
						fail("baseline novel execution verification failed: " + err.Error())
					}
				case "semantic_replay":
					if err := validateReplayVerification(sample); err != nil {
						fail("baseline semantic replay verification failed: " + err.Error())
					}
					if err := validateCrossBindingVerification(sample); err != nil {
						fail("baseline cross-binding verification failed: " + err.Error())
					}
				case "idempotent_replay":
					if err := validateIdempotentVerification(sample); err != nil {
						fail("baseline idempotent replay verification failed: " + err.Error())
					}
				}
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
				expectedSteps = 2
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
		if err := validateScaleVerification(sample); err != nil {
			fail("scale independent verification failed: " + err.Error())
		}
	case "artifact":
		if err := validateArtifactVerification(sample); err != nil {
			fail("artifact independent verification failed: " + err.Error())
		}
	case "compiler":
		requireDiagnostics("total", "recursive_expansion", "parse_validation", "compile_semantic", "plan_materialization", "digest_generation")
		requireCounters("alloc_bytes", "alloc_objects")
		if err := validateCompilerVerification(sample); err != nil {
			fail("compiler independent verification failed: " + err.Error())
		}
	case "concurrency":
		requireCounters("cas_attempts", "cas_conflicts", "cas_retries", "barrier_clients", "service_clients_observed", "offered_concurrency_observed", "forced_queue_waiters")
		if err := validateConcurrencyVerification(sample); err != nil {
			fail("concurrency independent verification failed: " + err.Error())
		}
	case "rq5":
		if err := validateRQ5Verification(sample); err != nil {
			fail("RQ5 independent verification failed: " + err.Error())
		}
	}
	return reasons, failed
}

func validateRLSVerification(sample Sample, expectedSteps int) error {
	return validateRLSVerificationStrict(sample, expectedSteps)
}

func validateAttackVerification(sample Sample) error {
	return validateAttackVerificationStrict(sample)
}

// ValidateAttackEvidence is the adapter-side fail-closed gate. It is exported
// only inside evaluation/internal so the source-controlled adapter can retain
// an observed invariant violation as status=fail instead of emitting a pass
// record that the campaign finalizer would reject later.
func ValidateAttackEvidence(sample Sample) error {
	if sample.ExperimentID != "attack" || sample.Status != "pass" {
		return errors.New("attack evidence validation requires a passing attack sample")
	}
	if err := validateAttackVerification(sample); err != nil {
		return err
	}
	failed := false
	validateTrace(sample, func(string) { failed = true })
	if failed {
		return errors.New("attack trace transition evidence is invalid")
	}
	return nil
}

func validateProvSQLVerification(sample Sample) error {
	return validateProvSQLVerificationForWarmup(sample, false)
}

func validateProvSQLVerificationForWarmup(sample Sample, warmup bool) error {
	evidence := sample.ProvSQLVerification
	if evidence == nil {
		return errors.New("ProvSQL verification evidence is absent")
	}
	spec, err := provsqlfixture.ParseScale(sample.Scale)
	if err != nil || sample.WorkloadID != "nonce-join-group" || sample.ProcessReplicate != 1 {
		return errors.New("ProvSQL sample is outside the frozen matrix")
	}
	nonce, err := provsqlfixture.Nonce(sample.Scale, sample.ProcessReplicate, sample.Iteration, warmup)
	if err != nil {
		return err
	}
	nonceBinding, _ := provsqlfixture.NonceBindingSHA256(sample.Scale, sample.ProcessReplicate, sample.Iteration, warmup)
	logical, _ := provsqlfixture.LogicalSQL(sample.Scale, nonce)
	expectedRows, err := provsqlfixture.ExpectedResultRows(sample.Scale)
	if err != nil {
		return err
	}
	expectedResult, err := CanonicalResultHash(expectedRows)
	if err != nil {
		return err
	}
	physical := provsqlfixture.PhysicalSQLSHA256()
	if sample.Mode == "taskgate" {
		physical = sha256Hex([]byte(logical))
	}
	if evidence.Version != "taskgate-final-v5-provsql-verification-v1" ||
		evidence.FixtureVersion != provsqlfixture.Version || evidence.FixtureSQLSHA256 != provsqlfixture.FixtureSQLSHA256() ||
		evidence.EnableSQLSHA256 != provsqlfixture.EnableSQLSHA256() || evidence.DatasetSHA256 != provsqlfixture.ExpectedDatasetSHA256() ||
		evidence.DatasetProbeSQLSHA256 != provsqlfixture.DatasetProbeSQLSHA256() ||
		evidence.BusinessDatasetProbeSQLSHA256 != provsqlfixture.BusinessDatasetProbeSQLSHA256() ||
		evidence.DatasetRows != provsqlfixture.DatasetRowCount ||
		evidence.ScaleLimit != spec.Limit || evidence.Nonce != nonce || evidence.Warmup != warmup || evidence.NonceBindingSHA256 != nonceBinding ||
		evidence.PhysicalSQLSHA256 != physical || evidence.LogicalSQLSHA256 != sha256Hex([]byte(logical)) ||
		evidence.ExpectedRows != provsqlfixture.ExpectedRows || evidence.ExpectedColumns != provsqlfixture.ExpectedColumns ||
		evidence.ExpectedResultSHA256 != expectedResult || evidence.ObservedResultSHA256 != expectedResult ||
		sample.RowCount != provsqlfixture.ExpectedRows || sample.ColumnCount != provsqlfixture.ExpectedColumns || sample.ResultSHA256 != expectedResult ||
		evidence.ExpectedDependencyFacts <= 0 || !validSHA256(evidence.ExpectedDependencySHA256) ||
		!validSHA256(evidence.TypedDrainSHA256) || evidence.FailureStage != "" {
		return errors.New("ProvSQL fixture/query/result/drain binding differs from the frozen oracle")
	}
	for _, digest := range []string{evidence.BindingSHA256, evidence.CacheConditionSHA256, evidence.ExecutionOrderSHA256} {
		if !validSHA256(digest) {
			return errors.New("ProvSQL paired execution binding is invalid")
		}
	}
	if evidence.CacheConditionSHA256 != sha256Hex([]byte(provsqlfixture.Version+"\x00warm-after-complete-typed-dataset-fingerprint")) ||
		evidence.ExecutionOrderSHA256 != sha256Hex([]byte(strings.Join([]string{provsqlfixture.Version, "execution-order-v2", sample.PairID,
			sample.PairedSystemOrder, strconv.Itoa(sample.OrderPosition), sample.Mode, strconv.FormatInt(nonce, 10)}, "\x00"))) ||
		sample.PhysicalSQLSHA256 != evidence.PhysicalSQLSHA256 || sample.LogicalSQLSHA256 != evidence.LogicalSQLSHA256 {
		return errors.New("ProvSQL cache/order/SQL evidence does not recompute")
	}
	switch sample.Mode {
	case "direct":
		if sample.System != "postgresql" || evidence.Boundary != "direct_complete_typed_drain" ||
			evidence.TypedDrainFields != provsqlfixture.ExpectedRows*4 ||
			!equalUint32s(evidence.FieldOIDs, []uint32{25, 1700, 20, 20}) ||
			!validExternalProvSQLSession(evidence) || evidence.ProvSQLVersion != "" || evidence.ProvSQLCommit != "" ||
			evidence.SharedPreload || evidence.AggTokenTextAsUUID || evidence.AggTokenOID != 0 ||
			evidence.CarrierGateType != "" || evidence.RowGateType != "" || evidence.RootTypesVerified || evidence.AggregateTokens != 0 ||
			evidence.RowTokens != 0 || evidence.GatesBefore != 0 || evidence.GatesAfter != 0 ||
			evidence.ArtifactBytesBefore != 0 || evidence.ArtifactBytesAfter != 0 || evidence.RepresentationSHA256 != "" ||
			!emptyProvSQLTaskGateSnapshots(evidence) {
			return errors.New("direct PostgreSQL arm contains invalid ProvSQL/system evidence")
		}
	case "provsql":
		if sample.System != "provsql" || evidence.Boundary != "provsql_complete_typed_drain" ||
			evidence.TypedDrainFields != provsqlfixture.ExpectedRows*5 ||
			!validExternalProvSQLSession(evidence) || evidence.ProvSQLVersion != provsqlfixture.ProvSQLVersion ||
			evidence.ProvSQLCommit != provsqlfixture.ProvSQLCommit || !evidence.SharedPreload || !evidence.AggTokenTextAsUUID ||
			evidence.AggTokenOID == 0 || evidence.UUIDOID == 0 ||
			!equalUint32s(evidence.FieldOIDs, []uint32{25, evidence.AggTokenOID, evidence.AggTokenOID, evidence.AggTokenOID, evidence.UUIDOID}) ||
			evidence.CarrierGateType != "agg" || evidence.RowGateType != "delta" || !evidence.RootTypesVerified ||
			evidence.AggregateTokens != provsqlfixture.ExpectedRows*provsqlfixture.CarrierColumns ||
			evidence.RowTokens != provsqlfixture.ExpectedRows || evidence.GatesAfter <= evidence.GatesBefore ||
			evidence.ArtifactBytesBefore <= 0 || evidence.ArtifactBytesAfter < evidence.ArtifactBytesBefore ||
			!validSHA256(evidence.RepresentationSHA256) || !emptyProvSQLTaskGateSnapshots(evidence) {
			return errors.New("ProvSQL arm lacks pinned agg_token/gate/representation evidence")
		}
	case "taskgate":
		if sample.System != "taskgate" || evidence.Boundary != "taskgate_released_parquet_v8" ||
			evidence.TypedDrainFields != provsqlfixture.ExpectedRows*4 || len(evidence.FieldOIDs) != 0 || evidence.RootTypesVerified ||
			evidence.TypedDrainSHA256 != sample.ResultSHA256 || evidence.PostgreSQLVersion != "" || evidence.PostgreSQLVersionNum != "" ||
			evidence.StatementTimeoutMS != 0 || evidence.MaxParallelWorkers != 0 || evidence.ClientMinMessages != "" ||
			evidence.LogMinMessages != "" || evidence.ProvSQLVersion != "" || evidence.ProvSQLCommit != "" || evidence.SharedPreload ||
			evidence.AggTokenTextAsUUID || evidence.AggTokenOID != 0 || evidence.UUIDOID != 0 ||
			evidence.CarrierGateType != "" || evidence.RowGateType != "" || evidence.AggregateTokens != 0 || evidence.RowTokens != 0 ||
			evidence.GatesBefore != 0 || evidence.GatesAfter != 0 || evidence.ArtifactBytesBefore != 0 ||
			evidence.ArtifactBytesAfter != 0 || evidence.RepresentationSHA256 != "" ||
			sample.ActualDependencyFacts != evidence.ExpectedDependencyFacts || sample.DependencySetSHA256 != evidence.ExpectedDependencySHA256 ||
			sample.GenerationBoundaryMS <= 0 || sample.FullTaskGateMS != sample.ClientFullDrainMS ||
			evidence.BusinessBefore == nil || evidence.BusinessAfter == nil || evidence.RootBefore == nil ||
			evidence.RootAfter == nil || evidence.ObserverBefore == nil || evidence.ObserverAfter == nil {
			return errors.New("TaskGate arm lacks exact V8/Parquet/FactSet boundary evidence")
		}
		if err := validateBusinessSQLTransition(*evidence.BusinessBefore, *evidence.BusinessAfter, 1, 1); err != nil {
			return err
		}
		if sample.BusinessSQLDelta != 2 || sample.SemanticReplay || sample.IdempotentReplay {
			return errors.New("TaskGate ProvSQL arm has inconsistent execution markers or targeted Business SQL delta")
		}
		if err := validateFreshRootLedgerSnapshot(*evidence.RootBefore); err != nil {
			return err
		}
		if err := validateRootLedgerSnapshot(*evidence.RootAfter); err != nil {
			return err
		}
		if err := validateRootMatchesSample(*evidence.RootAfter, sample); err != nil {
			return err
		}
		if sample.RootEpochBefore != evidence.RootBefore.Epoch || sample.RootEpochAfter != evidence.RootAfter.Epoch ||
			sample.RootSetSHA256Before != rootLedgerSetSHA256(*evidence.RootBefore) ||
			sample.RootSetSHA256After != rootLedgerSetSHA256(*evidence.RootAfter) {
			return errors.New("TaskGate ProvSQL root transition differs from independent snapshots")
		}
		if err := validateObserverTransition(sample, evidence.ObserverBefore, evidence.ObserverAfter); err != nil {
			return err
		}
	default:
		return errors.New("unknown ProvSQL paired mode")
	}
	return nil
}

func emptyProvSQLTaskGateSnapshots(evidence *ProvSQLVerificationEvidence) bool {
	return evidence.BusinessBefore == nil && evidence.BusinessAfter == nil && evidence.RootBefore == nil &&
		evidence.RootAfter == nil && evidence.ObserverBefore == nil && evidence.ObserverAfter == nil
}

func validExternalProvSQLSession(evidence *ProvSQLVerificationEvidence) bool {
	return evidence.PostgreSQLVersion != "" && evidence.PostgreSQLVersionNum != "" &&
		evidence.StatementTimeoutMS == provsqlfixture.StatementTimeout && evidence.MaxParallelWorkers == 0 &&
		evidence.ClientMinMessages == "error" && evidence.LogMinMessages == "error" && evidence.UUIDOID != 0
}

func equalUint32s(left, right []uint32) bool {
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

// ValidateProvSQLEvidence is the adapter-side fail-closed gate for a measured
// operation. Cross-sample nonce/representation/growth checks are intentionally
// performed by FinalizeRun, which sees the complete retained sequence.
func ValidateProvSQLEvidence(sample Sample) error {
	if sample.ExperimentID != "provsql" || sample.Status != "pass" {
		return errors.New("ProvSQL evidence validation requires a passing measured sample")
	}
	return validateProvSQLVerification(sample)
}

// ValidateProvSQLWarmupEvidence applies the same independent fixture, query,
// typed-drain, circuit, V8, and FactSet checks as the measured-sample gate,
// while recomputing the disjoint source-controlled warmup nonce domain. The
// runner still discards passing warmups after protocol validation; failed or
// invalid warmups are retained and fail the run instead of disappearing.
func ValidateProvSQLWarmupEvidence(sample Sample) error {
	if sample.ExperimentID != "provsql" || sample.Status != "pass" ||
		sample.ProvSQLVerification == nil || !sample.ProvSQLVerification.Warmup {
		return errors.New("ProvSQL warmup evidence validation requires a passing warmup sample")
	}
	return validateProvSQLVerificationForWarmup(sample, true)
}

type expectedCompilerEvidence struct {
	fixture             compilerfixture.Case
	datasetSHA256       string
	resultSHA256        string
	physicalSQLSHA256   string
	logicalSQLSHA256    string
	artifacts           map[string]CompilerArtifactEvidence
	errorCode           string
	errorRelationSHA256 string
}

var compilerEvidenceCache sync.Map

func validateCompilerVerification(sample Sample) error {
	evidence := sample.CompilerVerification
	if evidence == nil {
		return errors.New("compiler equivalence evidence is absent")
	}
	expected, err := expectedCompilerVerification(sample.WorkloadID, sample.Scale, sample.Mode)
	if err != nil {
		return err
	}
	if evidence.FixtureVersion != compilerfixture.Version ||
		evidence.RegistrySHA256 != compilerfixture.RegistrySHA256(expected.fixture.Registry) ||
		evidence.ProductsSHA256 != compilerfixture.ProductsSHA256(expected.fixture.Products) ||
		evidence.FixtureSQLSHA256 != compilerfixture.FixtureSQLSHA256 || evidence.DatasetSHA256 != expected.datasetSHA256 {
		return errors.New("compiler fixture/registry/product binding differs from the frozen source")
	}
	if evidence.ExpectedDepth != expected.fixture.ExpectedDepth || evidence.ObservedDepth != expected.fixture.ExpectedDepth ||
		evidence.ExpectedSources != expected.fixture.ExpectedSources || evidence.ObservedSources != expected.fixture.ExpectedSources {
		return errors.New("compiler scale label does not match the frozen registry complexity")
	}
	if sample.Counters["alloc_bytes"] <= 0 || sample.Counters["alloc_objects"] <= 0 {
		return errors.New("compiler allocation run is absent")
	}
	if err := validateCompilerTiming(sample); err != nil {
		return err
	}
	if sample.Mode == "structured_rejection" {
		if evidence.StructuredErrorCode != expected.errorCode || evidence.AllocationErrorCode != expected.errorCode ||
			evidence.StructuredErrorRelationSHA256 != expected.errorRelationSHA256 || sample.ErrorCode != expected.errorCode {
			return errors.New("compiler limit control did not return the exact production error in both isolated runs")
		}
		if !sample.Rejected || !sample.RejectedNoResult || !sample.RejectedNoArtifact || !sample.RejectedNoSuccessfulAudit ||
			sample.ResultSHA256 != "" || sample.ArtifactSHA256 != "" || sample.ObjectSHA256 != "" || sample.QueryPlanSHA256 != "" ||
			sample.PhysicalSQLSHA256 != "" || sample.LogicalSQLSHA256 != "" || evidence.DirectResultSHA256 != "" ||
			evidence.NestedResultSHA256 != "" || len(evidence.Artifacts) != 0 {
			return errors.New("compiler limit control returned result or artifact evidence")
		}
		return nil
	}
	if sample.Mode != "compile" || sample.Rejected || evidence.StructuredErrorCode != "" ||
		evidence.StructuredErrorRelationSHA256 != "" || evidence.AllocationErrorCode != "" {
		return errors.New("supported compiler cell is mislabeled as a rejection")
	}
	if evidence.DirectResultSHA256 != expected.resultSHA256 || evidence.NestedResultSHA256 != expected.resultSHA256 ||
		sample.ResultSHA256 != expected.resultSHA256 || sample.RowCount != int64(len(expected.fixture.ExpectedRows)) ||
		sample.ColumnCount != len(expected.fixture.ExpectedRows[0]) {
		return errors.New("live PostgreSQL direct/nested result oracle differs from the frozen expected multiset")
	}
	if sample.PhysicalSQLSHA256 != expected.physicalSQLSHA256 || sample.LogicalSQLSHA256 != expected.logicalSQLSHA256 {
		return errors.New("compiler PostgreSQL SQL binding differs from the independently generated oracle")
	}
	if len(evidence.Artifacts) != len(expected.artifacts) {
		return errors.New("compiler artifact matrix is incomplete")
	}
	actual := make(map[string]CompilerArtifactEvidence, len(evidence.Artifacts))
	for _, artifact := range evidence.Artifacts {
		if _, duplicate := actual[artifact.Name]; duplicate {
			return errors.New("compiler artifact matrix contains a duplicate variant")
		}
		want, present := expected.artifacts[artifact.Name]
		if !present || !equalCompilerArtifactEvidence(artifact, want) {
			return fmt.Errorf("compiler artifact %q differs from independent recompilation", artifact.Name)
		}
		actual[artifact.Name] = artifact
	}
	measured := actual["measured"]
	if sample.ArtifactSHA256 != measured.ArtifactSHA256 || sample.QueryPlanSHA256 != measured.CanonicalPlanSHA256 ||
		actual["repeat"].ArtifactSHA256 != measured.ArtifactSHA256 || actual["allocation"].ArtifactSHA256 != measured.ArtifactSHA256 {
		return errors.New("measured/repeat/allocation artifacts are not byte-identical")
	}
	direct := actual["direct"]
	for name, artifact := range actual {
		if name == "repeat" || name == "allocation" {
			continue
		}
		if artifact.CanonicalPlanSHA256 != direct.CanonicalPlanSHA256 || artifact.InterfaceSHA256 != direct.InterfaceSHA256 ||
			artifact.OutputsSHA256 != direct.OutputsSHA256 || artifact.BaseProductsSHA256 != direct.BaseProductsSHA256 {
			return errors.New("alias/join-order/parenthesis/nesting semantic artifacts differ")
		}
	}
	return nil
}

// ValidateCompilerEvidence is the adapter-side fail-closed gate. The exact
// same independent recompilation and live-oracle invariants are applied again
// by FinalizeRun to the retained sample stream.
func ValidateCompilerEvidence(sample Sample) error {
	if sample.ExperimentID != "compiler" || sample.Status != "pass" {
		return errors.New("compiler evidence validation requires a passing compiler sample")
	}
	return validateCompilerVerification(sample)
}

func expectedCompilerVerification(workloadID, scale, mode string) (*expectedCompilerEvidence, error) {
	key := workloadID + "\x00" + scale + "\x00" + mode
	if cached, present := compilerEvidenceCache.Load(key); present {
		return cached.(*expectedCompilerEvidence), nil
	}
	one, err := compilerfixture.Build(workloadID, scale, mode)
	if err != nil {
		return nil, fmt.Errorf("compiler sample is outside the frozen matrix: %w", err)
	}
	datasetSHA256, err := CanonicalResultHash(compilerfixture.DatasetRows())
	if err != nil {
		return nil, err
	}
	expected := &expectedCompilerEvidence{fixture: one, datasetSHA256: datasetSHA256, artifacts: map[string]CompilerArtifactEvidence{}}
	compiler, err := one.NewCompiler()
	if err != nil {
		return nil, err
	}
	if mode == "structured_rejection" {
		_, compileErr := compiler.Compile(one.MeasuredRoot)
		var structured *viewcompiler.Error
		if !errors.As(compileErr, &structured) || structured == nil {
			return nil, errors.New("frozen compiler control no longer returns a structured error")
		}
		expected.errorCode = string(structured.Code)
		expected.errorRelationSHA256 = compilerfixture.SHA256String(compilerfixture.Version + "\x00relation\x00" + structured.Relation.String())
		compilerEvidenceCache.Store(key, expected)
		return expected, nil
	}
	for name, root := range one.SemanticRoots {
		artifact, compileErr := compiler.Compile(root)
		if compileErr != nil {
			return nil, fmt.Errorf("recompile frozen compiler variant %s: %w", name, compileErr)
		}
		expected.artifacts[name] = compilerArtifactFromDescriptor(name, compilerfixture.DescribeArtifact(artifact, one.Registry))
	}
	measured := expected.artifacts["measured"]
	expected.artifacts["repeat"] = renamedCompilerArtifact(measured, "repeat")
	expected.artifacts["allocation"] = renamedCompilerArtifact(measured, "allocation")
	resultSHA256, err := CanonicalResultHash(one.ExpectedRows)
	if err != nil {
		return nil, err
	}
	nestedArtifact, compileErr := compiler.Compile(one.SemanticRoots["nested"])
	if compileErr != nil {
		return nil, compileErr
	}
	logical, err := queryplan.CompileRelational(nestedArtifact.Plan, one.Products)
	if err != nil {
		return nil, err
	}
	expected.resultSHA256 = resultSHA256
	expected.physicalSQLSHA256 = compilerfixture.SHA256String(one.DirectSQL)
	expected.logicalSQLSHA256 = compilerfixture.SHA256String(logical.VisibleSQL)
	compilerEvidenceCache.Store(key, expected)
	return expected, nil
}

func compilerArtifactFromDescriptor(name string, value compilerfixture.ArtifactDescriptor) CompilerArtifactEvidence {
	return CompilerArtifactEvidence{
		Name: name, ArtifactSHA256: value.ArtifactSHA256, DefinitionSHA256: value.DefinitionSHA256,
		DependencySHA256: value.DependencySHA256, InterfaceSHA256: value.InterfaceSHA256,
		CanonicalPlanSHA256: value.CanonicalPlanSHA256, BindingSHA256: value.BindingSHA256,
		OutputsSHA256: value.OutputsSHA256, BaseProductsSHA256: value.BaseProductsSHA256,
		ReachableRelations: value.ReachableRelations, DependencyEdges: value.DependencyEdges,
		ExpandedSources: value.ExpandedSources, DefinitionBytes: value.DefinitionBytes,
		CanonicalPlanBytes: value.CanonicalPlanBytes,
	}
}

func renamedCompilerArtifact(value CompilerArtifactEvidence, name string) CompilerArtifactEvidence {
	value.Name = name
	return value
}

func equalCompilerArtifactEvidence(left, right CompilerArtifactEvidence) bool {
	return left == right && validSHA256(left.ArtifactSHA256) && validSHA256(left.DefinitionSHA256) &&
		validSHA256(left.DependencySHA256) && validSHA256(left.InterfaceSHA256) && validSHA256(left.CanonicalPlanSHA256) &&
		validSHA256(left.BindingSHA256) && validSHA256(left.OutputsSHA256) && validSHA256(left.BaseProductsSHA256) &&
		left.ReachableRelations > 0 && left.ExpandedSources > 0 && left.DefinitionBytes > 0 && left.CanonicalPlanBytes > 0
}

func validateCompilerTiming(sample Sample) error {
	total, present := sample.DiagnosticMS["total"]
	if !present || total <= 0 || math.IsNaN(total) || math.IsInf(total, 0) {
		return errors.New("compiler total timing is absent or non-finite")
	}
	var stages float64
	for _, name := range []string{"recursive_expansion", "parse_validation", "compile_semantic", "plan_materialization", "digest_generation"} {
		value, ok := sample.DiagnosticMS[name]
		if !ok || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return errors.New("compiler stage timing is absent or non-finite")
		}
		stages += value
	}
	if stages > total+0.001 || math.Abs(sample.PipelineMS["execute_and_derive"]-total) > 0.001 ||
		math.Abs(sample.PipelineMS["server_total"]-total) > 0.001 || math.Abs(sample.ClientAvailableMS-total) > 0.001 ||
		math.Abs(sample.ClientFullDrainMS-total) > 0.001 {
		return errors.New("compiler timing boundary is internally inconsistent")
	}
	for _, name := range []string{"prepare", "artifact_stage", "control_settlement", "artifact_publication", "response_finalize"} {
		if sample.PipelineMS[name] != 0 {
			return errors.New("compiler-only timing incorrectly includes a Gateway pipeline phase")
		}
	}
	return nil
}

func validateConcurrencyVerification(sample Sample) error {
	return validateConcurrencyVerificationStrict(sample)
}

func validateRQ5Verification(sample Sample) error {
	return validateRQ5VerificationStrict(sample)
}

type rq5RuntimeIdentity struct {
	BuildManifestSHA256 string
	PhaseImageID        string
	OnlineImageID       string
	OAImageID           string
	PhaseBinarySHA256   string
	OnlineBinarySHA256  string
	OABinarySHA256      string
	PhaseBinaryMTime    int64
	OnlineBinaryMTime   int64
	OABinaryMTime       int64
}

const (
	rq5RuntimeIdentityChangedReason = "RQ5 build manifest or runtime image identity changed across cycles/deployments"
	rq5BuildManifestMismatchReason  = "RQ5 sample build-manifest digest differs from sealed driver build manifest"
)

func loadRQ5DriverBuildManifestSHA256(runDir string) (string, error) {
	path := filepath.Join(runDir, "rq5-driver-build.json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("RQ5 driver build manifest must be a regular non-symlink file")
	}
	digest, err := FileSHA256(path)
	if err != nil || !validSHA256(digest) {
		return "", errors.New("RQ5 driver build manifest cannot be hashed")
	}
	return digest, nil
}

func validateRQ5RuntimeIdentityConsistency(samples []Sample, expectedBuildManifestSHA256 string) []string {
	var locked *rq5RuntimeIdentity
	for _, sample := range samples {
		if sample.ExperimentID != "rq5" || sample.Status != "pass" || sample.RQ5Verification == nil {
			continue
		}
		identity := rq5RuntimeIdentity{
			BuildManifestSHA256: sample.RQ5Verification.BuildManifestSHA256,
			PhaseImageID:        sample.RQ5Verification.PhaseImageID,
			OnlineImageID:       sample.RQ5Verification.OnlineImageID,
			OAImageID:           sample.RQ5Verification.OAImageID,
			PhaseBinarySHA256:   sample.RQ5Verification.PhaseBinarySHA256,
			OnlineBinarySHA256:  sample.RQ5Verification.OnlineBinarySHA256,
			OABinarySHA256:      sample.RQ5Verification.OABinarySHA256,
			PhaseBinaryMTime:    sample.RQ5Verification.PhaseBinaryMTimeUnix,
			OnlineBinaryMTime:   sample.RQ5Verification.OnlineBinaryMTimeUnix,
			OABinaryMTime:       sample.RQ5Verification.OABinaryMTimeUnix,
		}
		if expectedBuildManifestSHA256 != "" && identity.BuildManifestSHA256 != expectedBuildManifestSHA256 {
			return []string{rq5BuildManifestMismatchReason}
		}
		if locked == nil {
			copy := identity
			locked = &copy
			continue
		}
		if identity != *locked {
			return []string{rq5RuntimeIdentityChangedReason}
		}
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
	if evidence.ArtifactStatus != "AVAILABLE" || !queryreceipt.VersionAtLeast(evidence.Receipt.Version, queryreceipt.VersionV8) ||
		evidence.Receipt.ArtifactIntent == nil || evidence.Receipt.Exposure == nil {
		return errors.New("verified evidence is not AVAILABLE V8 exposure evidence")
	}
	receiptBytes, _ := json.Marshal(evidence.Receipt)
	availabilityBytes, _ := json.Marshal(evidence.AvailabilityProof)
	if sha256Hex(receiptBytes) != sample.ReceiptSHA256 || sha256Hex(availabilityBytes) != sample.AvailabilityAuditSHA256 {
		return errors.New("raw receipt/audit digest differs from sample")
	}
	intent, exposure := evidence.Receipt.ArtifactIntent, evidence.Receipt.Exposure
	if err := validateBaselineSignedSample(intent, exposure, sample); err != nil {
		return err
	}
	if evidence.DownloadedParquetSHA256 != intent.ParquetSHA256 || evidence.ParsedResultSHA256 != sample.ResultSHA256 ||
		evidence.Receipt.ResultHash != intent.ParquetSHA256 || evidence.Receipt.RowCount != intent.RowCount {
		return errors.New("artifact/result evidence differs from signed receipt")
	}
	if err := validateBaselineRootIdentity(sample, evidence.Receipt); err != nil {
		return err
	}
	return nil
}

func validateBaselineRootIdentity(sample Sample, receipt queryreceipt.QueryReceiptV1) error {
	if receipt.Exposure == nil || strings.TrimSpace(receipt.Exposure.RootTaskID) == "" {
		return errors.New("signed exposure root identity is absent")
	}
	if sha256Hex([]byte(sample.CampaignID+"\x00"+sample.DeploymentID+"\x00"+receipt.Exposure.RootTaskID)) != sample.RootTaskIDHash {
		return errors.New("salted root identity differs from signed exposure root")
	}
	return nil
}

func validateBaselineSignedSample(intent *queryreceipt.ArtifactIntentEvidenceV1, exposure *queryreceipt.ExposureEvidenceV1, sample Sample) error {
	if intent == nil || exposure == nil {
		return errors.New("signed artifact or exposure evidence is absent")
	}
	if intent.IntentSHA256 != sample.ArtifactIntentSHA256 || intent.ParquetSHA256 != sample.ArtifactSHA256 ||
		intent.ObjectSHA256 != sample.ObjectSHA256 || intent.ParquetSize != sample.ParquetBytes ||
		intent.ObjectSize != sample.EncryptedObjectBytes || intent.RowCount != sample.RowCount ||
		intent.ColumnCount != int64(sample.ColumnCount) {
		return errors.New("artifact/result evidence differs from signed receipt")
	}
	if exposure.ReleaseSetSHA256 != sample.ReleaseSetSHA256 || exposure.InfluenceSetSHA256 != sample.DependencySetSHA256 ||
		exposure.OutcomeSetSHA256 != sample.OutcomeSetSHA256 || exposure.ActualReleaseFacts != sample.ActualReleaseFacts ||
		exposure.ActualInfluenceFacts != sample.ActualDependencyFacts || exposure.ActualOutcomeFacts != sample.ActualOutcomeFacts ||
		exposure.ChargedReleaseFacts != sample.ChargedReleaseFacts || exposure.ChargedInfluenceFacts != sample.ChargedDependencyFacts ||
		exposure.ChargedOutcomeFacts != sample.ChargedOutcomeFacts || exposure.ActualPredicateAtomCount != sample.PredicateAtomCount ||
		exposure.ActualCompositeCount != sample.CompositeCount || exposure.RootEpoch != sample.RootEpochAfter {
		return errors.New("exposure evidence differs from signed receipt")
	}
	return nil
}

const redactedVerifierVersion = "taskgate-final-v5-composite-verifier-v1"

func validateBusinessSQLSnapshot(snapshot BusinessSQLSnapshot) error {
	if snapshot.StatsResetUnixMicro <= 0 || snapshot.Dealloc < 0 || snapshot.VisibleCalls < 0 || snapshot.CompanionCalls < 0 {
		return errors.New("Business SQL counter snapshot is absent or invalid")
	}
	return nil
}

func validateBusinessSQLTransition(before, after BusinessSQLSnapshot, visibleDelta, companionDelta int64) error {
	if err := validateBusinessSQLSnapshot(before); err != nil {
		return err
	}
	if err := validateBusinessSQLSnapshot(after); err != nil {
		return err
	}
	if before.StatsResetUnixMicro != after.StatsResetUnixMicro || before.Dealloc != after.Dealloc {
		return errors.New("Business SQL statistics reset or deallocation changed between snapshots")
	}
	if after.VisibleCalls < before.VisibleCalls || after.CompanionCalls < before.CompanionCalls {
		return errors.New("Business SQL counters regressed between snapshots")
	}
	if after.VisibleCalls-before.VisibleCalls != visibleDelta || after.CompanionCalls-before.CompanionCalls != companionDelta {
		return fmt.Errorf("Business SQL deltas differ from required visible=%d companion=%d", visibleDelta, companionDelta)
	}
	return nil
}

func validateRootLedgerSnapshot(snapshot RootLedgerSnapshot) error {
	if snapshot.Epoch <= 0 || snapshot.ReleaseCardinality < 0 || snapshot.DependencyCardinality < 0 || snapshot.OutcomeCardinality < 0 || snapshot.RootObservationCount <= 0 {
		return errors.New("root ledger epoch/cardinality evidence is absent or invalid")
	}
	for _, digest := range []string{snapshot.DictionarySetSHA256, snapshot.ReleaseSetSHA256, snapshot.DependencySetSHA256, snapshot.OutcomeSetSHA256, snapshot.RootObservationSetSHA256} {
		if !validSHA256(digest) {
			return errors.New("root ledger contains an invalid digest")
		}
	}
	return nil
}

func validateFreshRootLedgerSnapshot(snapshot RootLedgerSnapshot) error {
	if snapshot.Epoch != 0 || snapshot.DictionarySetSHA256 != "" || snapshot.ReleaseSetSHA256 != "" ||
		snapshot.DependencySetSHA256 != "" || snapshot.OutcomeSetSHA256 != "" || snapshot.ReleaseCardinality != 0 ||
		snapshot.DependencyCardinality != 0 || snapshot.OutcomeCardinality != 0 || snapshot.RootObservationCount != 0 ||
		snapshot.RootObservationSetSHA256 != emptyRootObservationSetSHA256() {
		return errors.New("novel request did not begin from a valid fresh zero root")
	}
	return nil
}

func emptyRootObservationSetSHA256() string {
	return sha256Hex([]byte("TASKGATE-FINAL-V5-ROOT-OBSERVATION-SET-V1\x00"))
}

func validateRootMatchesSample(snapshot RootLedgerSnapshot, sample Sample) error {
	if snapshot.Epoch != sample.RootEpochAfter || snapshot.ReleaseSetSHA256 != sample.ReleaseSetSHA256 ||
		snapshot.DependencySetSHA256 != sample.DependencySetSHA256 || snapshot.OutcomeSetSHA256 != sample.OutcomeSetSHA256 ||
		snapshot.ReleaseCardinality != sample.ActualReleaseFacts || snapshot.DependencyCardinality != sample.ActualDependencyFacts ||
		snapshot.OutcomeCardinality != sample.ActualOutcomeFacts {
		return errors.New("root ledger snapshot differs from the sample exposure")
	}
	return nil
}

func validateNovelVerification(sample Sample) error {
	evidence := sample.ReplayVerification
	if evidence == nil {
		return errors.New("raw novel execution snapshots are absent")
	}
	if err := validateBusinessSQLTransition(evidence.BusinessBefore, evidence.BusinessAfter, 1, 1); err != nil {
		return err
	}
	if err := validateFreshRootLedgerSnapshot(evidence.RootBefore); err != nil {
		return err
	}
	if err := validateRootLedgerSnapshot(evidence.RootAfter); err != nil {
		return err
	}
	if err := validateRootMatchesSample(evidence.RootAfter, sample); err != nil {
		return err
	}
	if sample.RootEpochBefore != evidence.RootBefore.Epoch || sample.RootEpochAfter != evidence.RootAfter.Epoch ||
		sample.RootSetSHA256Before != rootLedgerSetSHA256(evidence.RootBefore) || sample.RootSetSHA256After != rootLedgerSetSHA256(evidence.RootAfter) {
		return errors.New("novel top-level root transition differs from independent snapshots")
	}
	observedBusinessDelta := evidence.BusinessAfter.VisibleCalls - evidence.BusinessBefore.VisibleCalls +
		evidence.BusinessAfter.CompanionCalls - evidence.BusinessBefore.CompanionCalls
	if sample.BusinessSQLDelta != 2 || sample.BusinessSQLDelta != observedBusinessDelta || sample.SemanticReplay || sample.IdempotentReplay {
		return errors.New("novel execution markers or top-level Business SQL delta are inconsistent")
	}
	return nil
}

func validateReplayVerification(sample Sample) error {
	evidence := sample.ReplayVerification
	if evidence == nil {
		return errors.New("raw semantic replay snapshots are absent")
	}
	if err := validateBusinessSQLTransition(evidence.BusinessBefore, evidence.BusinessAfter, 0, 0); err != nil {
		return err
	}
	if err := validateRootLedgerSnapshot(evidence.RootBefore); err != nil {
		return err
	}
	if err := validateRootLedgerSnapshot(evidence.RootAfter); err != nil {
		return err
	}
	if evidence.RootBefore != evidence.RootAfter {
		return errors.New("semantic replay changed the complete root ledger snapshot")
	}
	if err := validateRootMatchesSample(evidence.RootAfter, sample); err != nil {
		return err
	}
	if sample.RootEpochBefore != evidence.RootBefore.Epoch || sample.RootSetSHA256Before != rootLedgerSetSHA256(evidence.RootBefore) ||
		sample.RootSetSHA256After != rootLedgerSetSHA256(evidence.RootAfter) {
		return errors.New("semantic replay top-level root transition differs from the independent snapshot")
	}
	if !validSHA256(evidence.SourceObservationSHA256) || evidence.SourceObservationSHA256 != evidence.ReplayObservationSHA256 {
		return errors.New("semantic replay did not retain the source observation identity")
	}
	if sample.BaselineVerification == nil || sample.BaselineVerification.Receipt.Exposure == nil ||
		evidence.ReplayObservationSHA256 != sample.BaselineVerification.Receipt.Exposure.ObservationSHA256 {
		return errors.New("semantic replay observation differs from the signed receipt")
	}
	if !sample.SemanticReplay || sample.IdempotentReplay || sample.BusinessSQLDelta != 0 {
		return errors.New("semantic replay markers are inconsistent")
	}
	return nil
}

func validateRedactedManifestStructure(manifest *RedactedVerifierManifest) error {
	if manifest == nil || manifest.VerifierVersion != redactedVerifierVersion || manifest.VerificationResult != "pass" ||
		manifest.CanonicalCiphertextSize <= 0 || manifest.ReleasedParquetSize <= 0 ||
		manifest.TerminalAuditSequence <= 0 || manifest.RegistrationAuditSequence <= 0 ||
		manifest.AvailabilityAuditSequence <= manifest.RegistrationAuditSequence {
		return errors.New("redacted composite verifier manifest is absent or incomplete")
	}
	for _, digest := range []string{
		manifest.QueryIDHash, manifest.ResultIDHash, manifest.RootTaskIDHash, manifest.ReceiptSHA256,
		manifest.ObservationSHA256, manifest.ReleaseSetSHA256, manifest.DependencySetSHA256,
		manifest.OutcomeSetSHA256, manifest.ArtifactIntentSHA256, manifest.ObjectKeySHA256,
		manifest.CanonicalCiphertextSHA256, manifest.ReleasedParquetSHA256, manifest.SchemaSHA256,
	} {
		if !validSHA256(digest) {
			return errors.New("redacted composite verifier manifest contains an invalid digest")
		}
	}
	return nil
}

func validateRedactedVerifierManifest(sample Sample) error {
	evidence := sample.BaselineVerification
	if evidence == nil || evidence.Receipt.ArtifactIntent == nil || evidence.Receipt.Exposure == nil {
		return errors.New("raw V8 evidence required by the verifier manifest is absent")
	}
	manifest := evidence.VerifierManifest
	if err := validateRedactedManifestStructure(manifest); err != nil {
		return err
	}
	receipt, intent, exposure := evidence.Receipt, evidence.Receipt.ArtifactIntent, evidence.Receipt.Exposure
	receiptBytes, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	queryIDHash := redactedIdentitySHA256(sample, "query", receipt.QueryID)
	resultIDHash := redactedIdentitySHA256(sample, "result", intent.ResultID)
	rootTaskIDHash := redactedTaskSHA256(sample, exposure.RootTaskID)
	if manifest.QueryIDHash != queryIDHash || manifest.ResultIDHash != resultIDHash || manifest.RootTaskIDHash != rootTaskIDHash ||
		manifest.RootTaskIDHash != sample.RootTaskIDHash || manifest.ReceiptSHA256 != sha256Hex(receiptBytes) || manifest.ReceiptSHA256 != sample.ReceiptSHA256 ||
		manifest.ObservationSHA256 != exposure.ObservationSHA256 || manifest.ReleaseSetSHA256 != exposure.ReleaseSetSHA256 ||
		manifest.DependencySetSHA256 != exposure.InfluenceSetSHA256 || manifest.OutcomeSetSHA256 != exposure.OutcomeSetSHA256 ||
		manifest.ArtifactIntentSHA256 != intent.IntentSHA256 || manifest.ArtifactIntentSHA256 != sample.ArtifactIntentSHA256 ||
		manifest.ObjectKeySHA256 != intent.ObjectKeySHA256 || manifest.CanonicalCiphertextSHA256 != intent.ObjectSHA256 ||
		manifest.CanonicalCiphertextSHA256 != sample.ObjectSHA256 || manifest.CanonicalCiphertextSize != intent.ObjectSize ||
		manifest.CanonicalCiphertextSize != sample.EncryptedObjectBytes || manifest.ReleasedParquetSHA256 != intent.ParquetSHA256 ||
		manifest.ReleasedParquetSHA256 != sample.ArtifactSHA256 || manifest.ReleasedParquetSize != intent.ParquetSize ||
		manifest.ReleasedParquetSize != sample.ParquetBytes || manifest.SchemaSHA256 != intent.SchemaSHA256 {
		return errors.New("redacted composite verifier manifest differs from signed result evidence")
	}
	if manifest.TerminalAuditSequence != receipt.AuditSequence || manifest.TerminalAuditSequence != evidence.TerminalProof.TerminalEvent.Sequence ||
		manifest.RegistrationAuditSequence != intent.RegistrationAuditSequence || manifest.RegistrationAuditSequence != evidence.RegistrationProof.TerminalEvent.Sequence ||
		manifest.AvailabilityAuditSequence != evidence.AvailabilityProof.TerminalEvent.Sequence {
		return errors.New("redacted composite verifier audit sequence differs from inclusion proofs")
	}
	return nil
}

func redactedIdentitySHA256(sample Sample, kind, rawID string) string {
	return sha256Hex([]byte("TASKGATE-FINAL-V5-PILOT-IDENTITY-V1\x00" + sample.CampaignID + "\x00" + sample.DeploymentID + "\x00" + kind + "\x00" + rawID))
}

func redactedTaskSHA256(sample Sample, rawID string) string {
	return sha256Hex([]byte(sample.CampaignID + "\x00" + sample.DeploymentID + "\x00" + rawID))
}

func rootLedgerSetSHA256(snapshot RootLedgerSnapshot) string {
	return sha256Hex([]byte(strings.Join([]string{snapshot.ReleaseSetSHA256, snapshot.DependencySetSHA256, snapshot.OutcomeSetSHA256}, "\x00")))
}

func rootObservationBindingSHA256(rootTaskIDHash, observationSHA256 string) string {
	return sha256Hex([]byte("TASKGATE-FINAL-V5-ROOT-OBSERVATION-BINDING-V1\x00" + rootTaskIDHash + "\x00" + observationSHA256))
}

func validateCrossBindingVerification(sample Sample) error {
	evidence := sample.CrossBindingVerification
	if evidence == nil {
		return errors.New("cross-binding negative-control evidence is absent")
	}
	for _, digest := range []string{
		evidence.FirstTaskIDHash, evidence.SecondTaskIDHash, evidence.FirstRootTaskIDHash, evidence.SecondRootTaskIDHash,
		evidence.FirstQueryIDHash, evidence.SecondQueryIDHash, evidence.FirstGrantSHA256, evidence.SecondGrantSHA256,
		evidence.FirstCacheKeySHA256, evidence.SecondCacheKeySHA256, evidence.FirstSQLFingerprintSHA256, evidence.SecondSQLFingerprintSHA256,
		evidence.FirstCatalogSHA256, evidence.SecondCatalogSHA256, evidence.FirstSchemaSHA256, evidence.SecondSchemaSHA256,
		evidence.FirstDatasourceIDHash, evidence.SecondDatasourceIDHash, evidence.FirstObservationSHA256,
		evidence.SecondObservationSHA256, evidence.FirstObservationBindingSHA256, evidence.SecondObservationBindingSHA256,
		evidence.FirstSourceQueryIDHash, evidence.SecondSourceQueryIDHash, evidence.SecondRootFirstQueryIDHash,
	} {
		if !validSHA256(digest) {
			return errors.New("cross-binding negative control contains an invalid identity digest")
		}
	}
	if evidence.FirstTaskIDHash != evidence.FirstRootTaskIDHash || evidence.SecondTaskIDHash != evidence.SecondRootTaskIDHash ||
		evidence.FirstTaskIDHash == evidence.SecondTaskIDHash || evidence.FirstRootTaskIDHash == evidence.SecondRootTaskIDHash ||
		evidence.FirstQueryIDHash == evidence.SecondQueryIDHash || evidence.FirstGrantSHA256 == evidence.SecondGrantSHA256 ||
		evidence.FirstCacheKeySHA256 == evidence.SecondCacheKeySHA256 || evidence.FirstObservationBindingSHA256 == evidence.SecondObservationBindingSHA256 {
		return errors.New("cross-binding negative control did not use distinct task/root/query/grant/cache/observation bindings")
	}
	if evidence.FirstSQLFingerprintSHA256 != evidence.SecondSQLFingerprintSHA256 ||
		evidence.FirstCatalogSHA256 != evidence.SecondCatalogSHA256 || evidence.FirstSchemaSHA256 != evidence.SecondSchemaSHA256 ||
		evidence.FirstDatasourceIDHash != evidence.SecondDatasourceIDHash {
		return errors.New("cross-binding negative control did not hold SQL/product/catalog/schema/datasource constant")
	}
	if evidence.FirstObservationBindingSHA256 != rootObservationBindingSHA256(evidence.FirstRootTaskIDHash, evidence.FirstObservationSHA256) ||
		evidence.SecondObservationBindingSHA256 != rootObservationBindingSHA256(evidence.SecondRootTaskIDHash, evidence.SecondObservationSHA256) {
		return errors.New("cross-binding observation identities do not recompute from their root bindings")
	}
	if evidence.FirstRootTaskIDHash != sample.RootTaskIDHash || evidence.FirstSourceQueryIDHash != evidence.FirstQueryIDHash ||
		evidence.SecondSourceQueryIDHash != evidence.SecondQueryIDHash || evidence.SecondRootFirstQueryIDHash != evidence.SecondQueryIDHash {
		return errors.New("cross-binding negative control is not bound to a first query on the second root")
	}
	if sample.BaselineVerification == nil || sample.BaselineVerification.Receipt.Exposure == nil ||
		evidence.FirstGrantSHA256 != sample.BaselineVerification.Receipt.GrantDigest ||
		evidence.FirstObservationSHA256 != sample.BaselineVerification.Receipt.Exposure.ObservationSHA256 {
		return errors.New("cross-binding source identity differs from the semantic replay receipt")
	}
	receipt := sample.BaselineVerification.Receipt
	if evidence.FirstSQLFingerprintSHA256 != receipt.SQLFingerprint || evidence.FirstCatalogSHA256 != receipt.CatalogDigest ||
		evidence.FirstSchemaSHA256 != receipt.SchemaDigest ||
		evidence.FirstDatasourceIDHash != redactedIdentitySHA256(sample, "datasource", receipt.DatasourceID) {
		return errors.New("cross-binding SQL/product identity differs from the signed semantic receipt")
	}
	if err := validateBusinessSQLTransition(evidence.BusinessBefore, evidence.BusinessAfter, 1, 1); err != nil {
		return err
	}
	if evidence.SemanticReplayAudits != 0 || evidence.SettlementAudits != 1 || evidence.SemanticReplay || evidence.IdempotentReplay {
		return errors.New("cross-binding negative control did not take one fresh-settlement path")
	}
	if err := validateRedactedManifestStructure(evidence.VerifierManifest); err != nil {
		return err
	}
	if evidence.VerifierManifest.QueryIDHash != evidence.SecondQueryIDHash || evidence.VerifierManifest.RootTaskIDHash != evidence.SecondRootTaskIDHash ||
		evidence.VerifierManifest.ObservationSHA256 != evidence.SecondObservationSHA256 {
		return errors.New("cross-binding verifier manifest is not bound to the second root/query/observation")
	}
	return nil
}

func validateTerminalIdentitySnapshot(snapshot TerminalIdentitySnapshot) error {
	if !snapshot.Found || snapshot.ArtifactStatus != "AVAILABLE" || snapshot.CanonicalCiphertextSize <= 0 {
		return errors.New("terminal identity snapshot is absent or not AVAILABLE")
	}
	for _, digest := range []string{snapshot.QueryIDHash, snapshot.ResultIDHash, snapshot.ReceiptSHA256, snapshot.IntentSHA256,
		snapshot.ObjectKeySHA256, snapshot.CommittedObjectSHA256, snapshot.CanonicalCiphertextSHA256, snapshot.ObservationSHA256} {
		if !validSHA256(digest) {
			return errors.New("terminal identity snapshot contains an invalid digest")
		}
	}
	if snapshot.CommittedObjectSHA256 != snapshot.CanonicalCiphertextSHA256 {
		return errors.New("Control committed-object digest differs from canonical ciphertext")
	}
	return nil
}

func validateIdempotentControlSnapshot(snapshot IdempotentControlSnapshot) error {
	if err := validateBusinessSQLSnapshot(snapshot.Business); err != nil {
		return err
	}
	if err := validateRootLedgerSnapshot(snapshot.Root); err != nil {
		return err
	}
	if snapshot.QueryRecords < 1 || snapshot.ExposureCharges < 1 || snapshot.Observations < 1 || snapshot.Receipts < 1 ||
		snapshot.Artifacts < 1 || snapshot.AvailableArtifacts < 1 || snapshot.AvailableArtifacts > snapshot.Artifacts ||
		snapshot.TerminalAudits < 1 || snapshot.RegistrationAudits < 1 || snapshot.AvailabilityAudits < 1 || snapshot.CanonicalObjects < 1 {
		return errors.New("idempotent Control/object counters are absent or incoherent")
	}
	return validateTerminalIdentitySnapshot(snapshot.Target)
}

func validateIdempotentVerification(sample Sample) error {
	evidence := sample.IdempotentVerification
	if evidence == nil {
		return errors.New("raw idempotent replay snapshots are absent")
	}
	if err := validateIdempotentControlSnapshot(evidence.Before); err != nil {
		return err
	}
	if err := validateIdempotentControlSnapshot(evidence.After); err != nil {
		return err
	}
	if err := validateBusinessSQLTransition(evidence.Before.Business, evidence.After.Business, 0, 0); err != nil {
		return err
	}
	if evidence.Before != evidence.After {
		return errors.New("idempotent replay changed Business/Control/root/audit/object state")
	}
	if err := validateTerminalIdentitySnapshot(evidence.Returned); err != nil {
		return err
	}
	if evidence.Returned != evidence.Before.Target {
		return errors.New("idempotent replay returned a different terminal identity")
	}
	if err := validateRootMatchesSample(evidence.After.Root, sample); err != nil {
		return err
	}
	if sample.RootEpochBefore != evidence.Before.Root.Epoch || sample.RootSetSHA256Before != rootLedgerSetSHA256(evidence.Before.Root) ||
		sample.RootSetSHA256After != rootLedgerSetSHA256(evidence.After.Root) ||
		!sample.IdempotentReplay || sample.SemanticReplay || sample.BusinessSQLDelta != 0 {
		return errors.New("idempotent replay markers or top-level root transition are inconsistent")
	}
	baseline := sample.BaselineVerification
	if baseline == nil || baseline.Receipt.ArtifactIntent == nil || baseline.Receipt.Exposure == nil {
		return errors.New("idempotent replay lacks signed result evidence")
	}
	intent, exposure := baseline.Receipt.ArtifactIntent, baseline.Receipt.Exposure
	receiptBytes, err := json.Marshal(baseline.Receipt)
	if err != nil {
		return err
	}
	want := TerminalIdentitySnapshot{
		Found: true, QueryIDHash: redactedIdentitySHA256(sample, "query", baseline.Receipt.QueryID),
		ResultIDHash: redactedIdentitySHA256(sample, "result", intent.ResultID), ReceiptSHA256: sha256Hex(receiptBytes),
		IntentSHA256: intent.IntentSHA256, ObjectKeySHA256: intent.ObjectKeySHA256,
		CommittedObjectSHA256: intent.ObjectSHA256, CanonicalCiphertextSHA256: intent.ObjectSHA256,
		CanonicalCiphertextSize: intent.ObjectSize, ArtifactStatus: "AVAILABLE", ObservationSHA256: exposure.ObservationSHA256,
	}
	if evidence.Returned != want {
		return errors.New("idempotent terminal identity differs from signed receipt/object evidence")
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
	if !sample.IdempotentReplay || sample.SemanticReplay {
		return errors.New("recovery response did not take the idempotent resume path")
	}
	if evidence.BusinessCallsAtFailure <= evidence.BusinessCallsBefore ||
		evidence.BusinessCallsAfter != evidence.BusinessCallsAtFailure {
		return errors.New("artifact recovery re-executed Business PostgreSQL")
	}
	if err := validateBusinessSQLTransition(evidence.BusinessBeforeSnapshot, evidence.BusinessAtFailureSnapshot, 1, 1); err != nil {
		return fmt.Errorf("forced query Business SQL evidence: %w", err)
	}
	if err := validateBusinessSQLTransition(evidence.BusinessAtFailureSnapshot, evidence.BusinessAfterSnapshot, 0, 0); err != nil {
		return fmt.Errorf("recovery Business SQL evidence: %w", err)
	}
	if evidence.BusinessCallsBefore != evidence.BusinessBeforeSnapshot.VisibleCalls+evidence.BusinessBeforeSnapshot.CompanionCalls ||
		evidence.BusinessCallsAtFailure != evidence.BusinessAtFailureSnapshot.VisibleCalls+evidence.BusinessAtFailureSnapshot.CompanionCalls ||
		evidence.BusinessCallsAfter != evidence.BusinessAfterSnapshot.VisibleCalls+evidence.BusinessAfterSnapshot.CompanionCalls {
		return errors.New("aggregate and separated Business SQL counters differ")
	}
	observedBusinessDelta := evidence.BusinessAtFailureSnapshot.VisibleCalls - evidence.BusinessBeforeSnapshot.VisibleCalls +
		evidence.BusinessAtFailureSnapshot.CompanionCalls - evidence.BusinessBeforeSnapshot.CompanionCalls
	if observedBusinessDelta != 2 || sample.BusinessSQLDelta != observedBusinessDelta {
		return errors.New("recovery top-level Business SQL delta differs from the independent visible/companion counters")
	}
	if evidence.QueryRecordsAtFailure-evidence.QueryRecordsBefore != 1 ||
		evidence.QueryRecordsAfter != evidence.QueryRecordsAtFailure {
		return errors.New("artifact recovery created another query record")
	}
	if evidence.SettlementsAtFailure != 1 || evidence.SettlementsAfter != evidence.SettlementsAtFailure {
		return errors.New("artifact recovery repeated exposure settlement")
	}
	if int64(len(evidence.SettlementAuditSequencesAtFailure)) != evidence.SettlementsAtFailure ||
		!equalInt64s(evidence.SettlementAuditSequencesAtFailure, evidence.SettlementAuditSequencesAfter) {
		return errors.New("artifact recovery changed the exposure-settlement audit sequence")
	}
	for index, sequence := range evidence.SettlementAuditSequencesAtFailure {
		if sequence <= 0 || (index > 0 && sequence <= evidence.SettlementAuditSequencesAtFailure[index-1]) {
			return errors.New("exposure-settlement audit sequence is invalid")
		}
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
	if err := validateRootLedgerSnapshot(evidence.RootAtFailure); err != nil {
		return err
	}
	if err := validateRootLedgerSnapshot(evidence.RootAfter); err != nil {
		return err
	}
	if evidence.RootAtFailure != evidence.RootAfter {
		return errors.New("artifact recovery changed the complete root ledger snapshot")
	}
	if err := validateRootMatchesSample(evidence.RootAfter, sample); err != nil {
		return err
	}
	if sample.RootEpochBefore != 0 || sample.RootEpochAfter != evidence.RootAfter.Epoch ||
		sample.RootSetSHA256Before != rootLedgerSetSHA256(RootLedgerSnapshot{}) ||
		sample.RootSetSHA256After != rootLedgerSetSHA256(evidence.RootAfter) {
		return errors.New("recovery top-level root transition differs from the failure/after snapshots")
	}
	if err := validateRecoveryExposureSnapshot(evidence.ExposureAtFailure, evidence.RootAtFailure, sample); err != nil {
		return err
	}
	if err := validateRecoveryExposureSnapshot(evidence.ExposureAfter, evidence.RootAfter, sample); err != nil {
		return err
	}
	if evidence.ExposureAtFailure != evidence.ExposureAfter {
		return errors.New("artifact recovery changed the signed exposure component sets")
	}
	if sample.BaselineVerification == nil || sample.BaselineVerification.Receipt.Exposure == nil || sample.BaselineVerification.Receipt.ArtifactIntent == nil ||
		evidence.ExposureAfter != recoveryExposureSnapshot(sample, *sample.BaselineVerification.Receipt.Exposure) {
		return errors.New("recovery exposure snapshot differs from the signed receipt")
	}
	if err := validateCanonicalObjectSnapshot(evidence.ObjectAtFailure); err != nil {
		return err
	}
	if err := validateCanonicalObjectSnapshot(evidence.ObjectAfter); err != nil {
		return err
	}
	if evidence.ObjectAtFailure != evidence.ObjectAfter {
		return errors.New("artifact recovery changed the canonical object or intent")
	}
	if evidence.ObjectAfter.ObjectKeySHA256 != sample.BaselineVerification.Receipt.ArtifactIntent.ObjectKeySHA256 ||
		evidence.ObjectAfter.CanonicalCiphertextSHA256 != sample.ObjectSHA256 || evidence.ObjectAfter.CanonicalCiphertextSize != sample.EncryptedObjectBytes ||
		evidence.ObjectAfter.IntentSHA256 != sample.ArtifactIntentSHA256 {
		return errors.New("recovery canonical object snapshot differs from the signed intent")
	}
	if evidence.TerminalAuditsAtFailure != 1 || evidence.TerminalAuditsAfter != 1 ||
		evidence.RegistrationAuditsAtFailure != 1 || evidence.RegistrationAuditsAfter != 1 ||
		evidence.AvailabilityAuditsAtFailure != 0 || evidence.AvailabilityAuditsAfter != 1 {
		return errors.New("artifact recovery did not preserve terminal/registration audits and append exactly one availability audit")
	}
	baseline := sample.BaselineVerification
	if evidence.TerminalAuditSequenceAtFailure <= 0 || evidence.TerminalAuditSequenceAtFailure != evidence.TerminalAuditSequenceAfter ||
		evidence.TerminalAuditSequenceAfter != baseline.Receipt.AuditSequence || evidence.TerminalAuditSequenceAfter != baseline.TerminalProof.TerminalEvent.Sequence ||
		evidence.RegistrationAuditSequenceAtFailure <= 0 || evidence.RegistrationAuditSequenceAtFailure != evidence.RegistrationAuditSequenceAfter ||
		evidence.RegistrationAuditSequenceAfter != baseline.Receipt.ArtifactIntent.RegistrationAuditSequence ||
		evidence.RegistrationAuditSequenceAfter != baseline.RegistrationProof.TerminalEvent.Sequence ||
		evidence.AvailabilityAuditSequenceAtFailure != 0 || evidence.AvailabilityAuditSequenceAfter <= evidence.RegistrationAuditSequenceAfter ||
		evidence.AvailabilityAuditSequenceAfter != baseline.AvailabilityProof.TerminalEvent.Sequence {
		return errors.New("artifact recovery audit sequences differ from signed inclusion proofs")
	}
	return nil
}

func validateRecoveryExposureSnapshot(exposure RecoveryExposureSnapshot, root RootLedgerSnapshot, sample Sample) error {
	if exposure.RootTaskIDHash != sample.RootTaskIDHash || !validSHA256(exposure.RootTaskIDHash) || exposure.ProfileVersion == "" || exposure.PredicateProfileVersion == "" || exposure.RootEpoch != root.Epoch ||
		exposure.DictionarySetSHA256 != root.DictionarySetSHA256 || exposure.ReleaseSetSHA256 != root.ReleaseSetSHA256 ||
		exposure.InfluenceSetSHA256 != root.DependencySetSHA256 || exposure.OutcomeSetSHA256 != root.OutcomeSetSHA256 ||
		exposure.ActualReleaseFacts != root.ReleaseCardinality || exposure.ActualInfluenceFacts != root.DependencyCardinality ||
		exposure.ActualOutcomeFacts != root.OutcomeCardinality {
		return errors.New("recovery exposure component sets differ from the root snapshot")
	}
	for _, digest := range []string{exposure.ObservationSHA256, exposure.DictionarySetSHA256, exposure.ReleaseSetSHA256,
		exposure.InfluenceSetSHA256, exposure.OutcomeSetSHA256, exposure.PredicateContextSHA256, exposure.PredicateSetSHA256,
		exposure.CompositeOutcomeSHA256} {
		if !validSHA256(digest) {
			return errors.New("recovery exposure component evidence contains an invalid digest")
		}
	}
	if exposure.ActualReleaseFacts < 0 || exposure.ActualInfluenceFacts < 0 || exposure.ActualOutcomeFacts < 0 ||
		exposure.ChargedReleaseFacts < 0 || exposure.ChargedInfluenceFacts < 0 || exposure.ChargedOutcomeFacts < 0 ||
		exposure.ChargedReleaseFacts > exposure.ActualReleaseFacts || exposure.ChargedInfluenceFacts > exposure.ActualInfluenceFacts ||
		exposure.ChargedOutcomeFacts > exposure.ActualOutcomeFacts || exposure.ActualPredicateAtomCount < 0 || exposure.ChargedPredicateAtomCount < 0 ||
		exposure.ChargedPredicateAtomCount > exposure.ActualPredicateAtomCount || exposure.ActualCompositeCount < 0 || exposure.ChargedCompositeCount < 0 ||
		exposure.ChargedCompositeCount > exposure.ActualCompositeCount {
		return errors.New("recovery exposure component cardinalities are invalid")
	}
	return nil
}

func recoveryExposureSnapshot(sample Sample, exposure queryreceipt.ExposureEvidenceV1) RecoveryExposureSnapshot {
	return RecoveryExposureSnapshot{
		RootTaskIDHash: redactedTaskSHA256(sample, exposure.RootTaskID), ProfileVersion: exposure.ProfileVersion,
		ActualReleaseFacts: exposure.ActualReleaseFacts, ActualInfluenceFacts: exposure.ActualInfluenceFacts,
		ActualOutcomeFacts: exposure.ActualOutcomeFacts, ChargedReleaseFacts: exposure.ChargedReleaseFacts,
		ChargedInfluenceFacts: exposure.ChargedInfluenceFacts, ChargedOutcomeFacts: exposure.ChargedOutcomeFacts,
		ObservationSHA256: exposure.ObservationSHA256, DictionarySetSHA256: exposure.DictionarySetSHA256,
		ReleaseSetSHA256: exposure.ReleaseSetSHA256, InfluenceSetSHA256: exposure.InfluenceSetSHA256,
		OutcomeSetSHA256: exposure.OutcomeSetSHA256, RootEpoch: exposure.RootEpoch,
		PredicateProfileVersion: exposure.PredicateProfileVersion, PredicateContextSHA256: exposure.PredicateContextSHA256,
		PredicateSetSHA256: exposure.PredicateSetSHA256, ActualPredicateAtomCount: exposure.ActualPredicateAtomCount,
		ChargedPredicateAtomCount: exposure.ChargedPredicateAtomCount, CompositeOutcomeSHA256: exposure.CompositeOutcomeSHA256,
		ActualCompositeCount: exposure.ActualCompositeCount, ChargedCompositeCount: exposure.ChargedCompositeCount,
	}
}

func validateCanonicalObjectSnapshot(snapshot CanonicalObjectSnapshot) error {
	if !snapshot.Exists || snapshot.CanonicalCiphertextSize <= 0 {
		return errors.New("canonical object snapshot is absent")
	}
	for _, digest := range []string{snapshot.ObjectKeySHA256, snapshot.CanonicalCiphertextSHA256, snapshot.IntentSHA256} {
		if !validSHA256(digest) {
			return errors.New("canonical object snapshot contains an invalid digest")
		}
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
	if config.CampaignClass == "publication" {
		campaignRoot := filepath.Dir(filepath.Clean(runDir))
		if _, err := verifySourceBuildBinding(
			filepath.Join(runDir, "observer.sha256"), filepath.Join(runDir, "observer-build.json"),
			filepath.Join(campaignRoot, "source-adapter", "final-v5-observer"),
			config.SubmissionCommit, observerBuildCommand, observerRequiredSources); err != nil {
			reasons = append(reasons, "source-built observer identity or build manifest is invalid")
		}
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
	seenDeploymentVolumeIDs := map[string]bool{}
	frozenDatasetSHA256 := ""
	frozenCatalogSHA256 := ""
	frozenAdapterBindingSHA256 := ""
	frozenBindingFileSHA256 := ""
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
			if seenVolumes[proof.VolumeSetSHA256] || seenControlSystems[proof.ControlPGSystemIdentifier] || seenBusinessSystems[proof.BusinessPGSystemIdentifier] || seenComposeProjects[proof.ComposeProjectName] || seenDeploymentVolumeIDs[proof.DeploymentVolumeIDSHA256] {
				reasons = append(reasons, "fresh-deployment identity reused: "+deployment.DeploymentID)
			}
			seenVolumes[proof.VolumeSetSHA256] = true
			seenControlSystems[proof.ControlPGSystemIdentifier] = true
			seenBusinessSystems[proof.BusinessPGSystemIdentifier] = true
			seenComposeProjects[proof.ComposeProjectName] = true
			seenDeploymentVolumeIDs[proof.DeploymentVolumeIDSHA256] = true
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
		volumeID, volumeOK := environment.Datasets["deployment_volume_id_sha256"].(string)
		adapterBindingSHA, bindingFileSHA, bindingIdentityErr := environmentBindingIdentity(environment)
		if !datasetOK || !catalogOK || !volumeOK || !validEnvironmentDatasetBindings(environment.Datasets) {
			reasons = append(reasons, "dataset/catalog digest acceptance failed: "+deployment.DeploymentID)
			continue
		}
		if bindingIdentityErr != nil {
			reasons = append(reasons, "strict adapter binding identity failed: "+deployment.DeploymentID)
			continue
		}
		if dataset != proof.DatasetFingerprintSHA256 || catalog != proof.CatalogSHA256 || volumeID != proof.DeploymentVolumeIDSHA256 {
			reasons = append(reasons, "environment/fresh-deployment binding mismatch: "+deployment.DeploymentID)
			continue
		}
		if frozenDatasetSHA256 == "" {
			frozenDatasetSHA256 = dataset
		} else if dataset != frozenDatasetSHA256 {
			reasons = append(reasons, "dataset digest changed across deployments")
		}
		if frozenCatalogSHA256 == "" {
			frozenCatalogSHA256 = catalog
		} else if catalog != frozenCatalogSHA256 {
			reasons = append(reasons, "Catalog digest changed across deployments")
		}
		if frozenAdapterBindingSHA256 == "" {
			frozenAdapterBindingSHA256 = adapterBindingSHA
		} else if adapterBindingSHA != frozenAdapterBindingSHA256 {
			reasons = append(reasons, "strict adapter section changed across deployments")
		}
		if frozenBindingFileSHA256 == "" {
			frozenBindingFileSHA256 = bindingFileSHA
		} else if bindingFileSHA != frozenBindingFileSHA256 {
			reasons = append(reasons, "dataset binding bytes changed across deployments")
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
		!validPostgresSystemIdentifier(proof.ControlPGSystemIdentifier) || !validPostgresSystemIdentifier(proof.BusinessPGSystemIdentifier) || proof.ControlPGSystemIdentifier == proof.BusinessPGSystemIdentifier ||
		!validSHA256(proof.DeploymentVolumeIDSHA256) || proof.DeploymentVolumeIDSHA256 != deriveDeploymentVolumeID(proof) ||
		!validSHA256(proof.CatalogSHA256) || !validSHA256(proof.DatasetFingerprintSHA256) ||
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
	catalogPath := filepath.Join(runDir, "environment", deploymentID+".fresh.catalog.yaml")
	if digest, err := FileSHA256(catalogPath); err != nil || digest != proof.CatalogSHA256 {
		reasons = append(reasons, "live Catalog bytes do not match proof: "+deploymentID)
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
