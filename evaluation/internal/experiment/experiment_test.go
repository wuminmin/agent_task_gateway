package experiment

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("TASKGATE_TEST_ADAPTER") == "1" {
		runTestAdapter()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runTestAdapter() {
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var operation AdapterOperation
		if StrictJSON(scanner.Bytes(), &operation) != nil {
			os.Exit(1)
		}
		rootIdentity := strings.Join([]string{operation.DeploymentID, operation.WorkloadID, operation.Scale, strconv.Itoa(operation.ProcessReplicate), strconv.Itoa(operation.Iteration), operation.RootGroupID}, "\x00")
		digestBytes := sha256.Sum256([]byte(rootIdentity))
		digest := hex.EncodeToString(digestBytes[:])
		resultBytes := sha256.Sum256([]byte(operation.WorkloadID + "\x00" + operation.Scale + "\x00" + strconv.Itoa(operation.Iteration)))
		resultDigest := hex.EncodeToString(resultBytes[:])
		sample := Sample{
			SchemaVersion: 1, CampaignID: operation.CampaignID, DeploymentID: operation.DeploymentID,
			ExperimentID: operation.ExperimentID, CellID: operation.CellID, SampleID: operation.SampleID,
			Iteration: operation.Iteration, ProcessReplicate: operation.ProcessReplicate, OrderPosition: operation.OrderPosition,
			RandomSeed: operation.RandomSeed, System: "taskgate", Mode: operation.Mode, WorkloadID: operation.WorkloadID,
			Scale: operation.Scale, ClientAvailableMS: 1, ClientFullDrainMS: 2,
			PipelineMS:   map[string]float64{"prepare": .1, "execute_and_derive": .2, "artifact_stage": .1, "control_settlement": .1, "artifact_publication": .1, "response_finalize": .1, "server_total": .8},
			DiagnosticMS: map[string]float64{}, ResultSHA256: resultDigest, RootTaskIDHash: digest, ReceiptVersion: "8",
			ReceiptSHA256: digest, ArtifactIntentSHA256: digest, AvailabilityAuditSHA256: digest,
			ReceiptVerified: true, ArtifactAvailable: true, Status: "pass", PublicationEligible: true,
		}
		if encoder.Encode(sample) != nil {
			os.Exit(1)
		}
	}
	if scanner.Err() != nil {
		os.Exit(1)
	}
}

func TestType7AndDeterministicBootstrap(t *testing.T) {
	values := []float64{1, 2, 3, 4}
	median, err := Type7(values, .5)
	if err != nil || median != 2.5 {
		t.Fatalf("median=%v err=%v", median, err)
	}
	p95, _ := Type7(values, .95)
	if p95 != 3.8499999999999996 {
		t.Fatalf("p95=%v", p95)
	}
	first, err := BootstrapMedian(values, 20260801, 500)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := BootstrapMedian(values, 20260801, 500)
	if first != second {
		t.Fatalf("bootstrap is not deterministic: %+v %+v", first, second)
	}
}

func TestSampleValidationRejectsOverlappingPhaseSumAndReplaySQL(t *testing.T) {
	s := validTestSample()
	s.PipelineMS["server_total"] = 1
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "phase sum") {
		t.Fatalf("invalid phase sum accepted: %v", err)
	}
	s = validTestSample()
	s.SemanticReplay = true
	s.BusinessSQLDelta = 1
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "Business") {
		t.Fatalf("replay SQL accepted: %v", err)
	}
}

func TestSmokeCannotBecomePublicationEvidence(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	config := Config{SchemaVersion: 1, CampaignClass: "pilot", CampaignID: "pilot-test", ExperimentID: "baseline", Deployments: 1, Warmups: 1, Samples: 1, RandomSeed: 20260801, FreshRootPerSample: true, Workloads: []Workload{{ID: "S1", Scales: []string{"tiny"}, Modes: []string{"novel"}}}}
	if err := WriteSmoke(config, dir); err != nil {
		t.Fatal(err)
	}
	value, err := os.ReadFile(filepath.Join(dir, "generated", "latex", "evidence.tex"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(value), "newcommand") {
		t.Fatalf("pilot emitted numeric macros: %s", value)
	}
	if _, err := SealRun(dir); err == nil {
		t.Fatal("pilot evidence was sealable")
	}
}

func TestAdapterCampaignIsDeterministicAndCreateExclusive(t *testing.T) {
	config := Config{
		SchemaVersion: 1, CampaignClass: "publication", CampaignID: "campaign-test", ExperimentID: "baseline",
		SubmissionCommit: "0123456789abcdef0123456789abcdef01234567", Deployments: 3, Warmups: 5, Samples: 30,
		RandomSeed: 20260801, FreshRootPerSample: true,
		Workloads: []Workload{{ID: "S1", Scales: []string{"tiny"}, Modes: []string{"novel"}}},
	}
	t.Setenv("TASKGATE_TEST_ADAPTER", "1")
	t.Setenv("TASKGATE_EXPERIMENT_CLASS", "publication")
	t.Setenv("TASKGATE_SUBMISSION_COMMIT", config.SubmissionCommit)
	t.Setenv("TASKGATE_CAMPAIGN_ID", config.CampaignID)
	first := filepath.Join(t.TempDir(), "first.jsonl")
	second := filepath.Join(t.TempDir(), "second.jsonl")
	if err := ExecuteAdapterCampaign(config, "deployment-01", os.Args[0], first); err != nil {
		t.Fatal(err)
	}
	if err := ExecuteAdapterCampaign(config, "deployment-01", os.Args[0], second); err != nil {
		t.Fatal(err)
	}
	firstBytes, _ := os.ReadFile(first)
	secondBytes, _ := os.ReadFile(second)
	if string(firstBytes) != string(secondBytes) {
		t.Fatal("seeded adapter operation order is not deterministic")
	}
	samples, err := ReadSamples([]string{first})
	if err != nil || len(samples) != 30 {
		t.Fatalf("samples=%d err=%v", len(samples), err)
	}
	if err := ExecuteAdapterCampaign(config, "deployment-01", os.Args[0], first); err == nil {
		t.Fatal("runner overwrote existing raw JSONL")
	}
}

func TestDependencyAwareOrderKeepsReplayAfterNovel(t *testing.T) {
	config := Config{RandomSeed: 20260801, FreshRootPerSample: true, Samples: 1, Workloads: []Workload{{ID: "S1", Scales: []string{"tiny"}, Modes: []string{"direct", "idempotent_replay", "novel", "semantic_replay"}}}}
	position := 0
	operations := buildOperations(config, "deployment-01", 1, 1, &position)
	positions := map[string]int{}
	for index, operation := range operations {
		positions[operation.Mode] = index
	}
	if !(positions["novel"] < positions["semantic_replay"] && positions["semantic_replay"] < positions["idempotent_replay"]) {
		t.Fatalf("dependent replay order = %+v", operations)
	}
	if !operations[positions["novel"]].FreshRootRequired || operations[positions["semantic_replay"]].FreshRootRequired {
		t.Fatalf("fresh-root anchor flags = %+v", operations)
	}
}

func TestExperimentEvidenceRejectsRecomputedOverlapAndMissingConcurrencyProof(t *testing.T) {
	overlap := validTestSample()
	overlap.ExperimentID = "scale"
	overlap.WorkloadID = "dependency-e2e"
	overlap.Scale = "100k-overlap-50"
	overlap.ResultSHA256 = strings.Repeat("a", 64)
	overlap.ActualDependencyFacts = 100
	overlap.ChargedDependencyFacts = 60
	if reasons, failed := validateExperimentEvidence(overlap); !failed || len(reasons) == 0 {
		t.Fatal("mislabeled overlap passed final evidence validation")
	}
	concurrency := validTestSample()
	concurrency.ExperimentID = "concurrency"
	concurrency.Mode = "natural_contention"
	concurrency.Scale = "500"
	concurrency.ResultSHA256 = strings.Repeat("b", 64)
	concurrency.Counters = map[string]int64{"cas_attempts": 1, "cas_conflicts": 0, "cas_retries": 0, "barrier_clients": 499, "offered_concurrency_observed": 0}
	if reasons, failed := validateExperimentEvidence(concurrency); !failed || len(reasons) == 0 {
		t.Fatal("unobserved width-500 concurrency passed final evidence validation")
	}
}

func TestPublicationFinalizerSealsCompleteEvidenceAndRejectsRootReuse(t *testing.T) {
	passing := buildPublicationEvidence(t, false)
	summary, err := FinalizeRun(passing)
	if err != nil || summary.Status != "pass" || !summary.PublicationEligible {
		t.Fatalf("passing summary=%+v err=%v", summary, err)
	}
	if _, err := SealRun(passing); err != nil {
		t.Fatal(err)
	}
	failing := buildPublicationEvidence(t, true)
	summary, err = FinalizeRun(failing)
	if err != nil || summary.Status != "fail" || summary.PublicationEligible {
		t.Fatalf("mutated summary=%+v err=%v", summary, err)
	}
	if _, err := SealRun(failing); err == nil {
		t.Fatal("mutated publication evidence was sealable")
	}
}

func buildPublicationEvidence(t *testing.T, reuseRoot bool) string {
	t.Helper()
	runDir := t.TempDir()
	for _, directory := range []string{"environment", "deployments", "raw"} {
		if err := os.MkdirAll(filepath.Join(runDir, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	commit := "0123456789abcdef0123456789abcdef01234567"
	config := Config{SchemaVersion: 1, CampaignClass: "publication", CampaignID: "publication-test", ExperimentID: "rls", SubmissionCommit: commit, Deployments: 3, Samples: 3, RandomSeed: 20260801, FreshRootPerSample: true, Workloads: []Workload{{ID: "trace", Scales: []string{"100"}, Modes: []string{"rls", "unlimited", "bounded"}}}}
	configBytes, _ := json.Marshal(config)
	if err := os.WriteFile(filepath.Join(runDir, "config.json"), configBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "adapter.sha256"), []byte(strings.Repeat("a", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("b", 64)
	for deployment := 1; deployment <= 3; deployment++ {
		deploymentID := fmt.Sprintf("deployment-%02d", deployment)
		environment := EnvironmentManifest{SchemaVersion: 1, CampaignID: config.CampaignID, DeploymentID: deploymentID, CapturedAt: time.Now().UTC().Format(time.RFC3339Nano), GitCommit: commit, GitStatus: []string{}, PublicationEligible: true, Host: map[string]any{}, Software: map[string]any{}, Storage: map[string]any{}, Datasets: map[string]any{"dataset_sha256": strings.Repeat("c", 64), "catalog_sha256": strings.Repeat("d", 64), "deployment_volume_id_sha256": fmt.Sprintf("%064x", deployment)}}
		environmentPath := filepath.Join(runDir, "environment", deploymentID+".json")
		if err := WriteEnvironment(environmentPath, environment); err != nil {
			t.Fatal(err)
		}
		beforePath := filepath.Join(runDir, "environment", deploymentID+".vmstat-before.txt")
		afterPath := filepath.Join(runDir, "environment", deploymentID+".vmstat-after.txt")
		if err := os.WriteFile(beforePath, []byte("pswpin 0\npswpout 0\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(afterPath, []byte("pswpin 0\npswpout 0\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		environmentSHA, _ := FileSHA256(environmentPath)
		beforeSHA, _ := FileSHA256(beforePath)
		afterSHA, _ := FileSHA256(afterPath)
		manifest := DeploymentManifest{SchemaVersion: 1, CampaignID: config.CampaignID, DeploymentID: deploymentID, FreshDeployment: true, EnvironmentSHA256: environmentSHA, WindowsEnvironmentSHA256: strings.Repeat("e", 64), VMStatBeforeSHA256: beforeSHA, VMStatAfterSHA256: afterSHA, StartedAt: UTCNow(), FinishedAt: UTCNow()}
		if err := WriteDeployment(filepath.Join(runDir, "deployments", deploymentID+".json"), manifest); err != nil {
			t.Fatal(err)
		}
		writer, err := NewJSONLWriter(filepath.Join(runDir, "raw", deploymentID+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		position := 0
		for iteration := 1; iteration <= 3; iteration++ {
			for _, mode := range config.Workloads[0].Modes {
				position++
				sample := validTestSample()
				sample.CampaignID, sample.DeploymentID, sample.ExperimentID = config.CampaignID, deploymentID, "rls"
				sample.CellID, sample.SampleID, sample.Iteration, sample.OrderPosition, sample.RandomSeed = "trace/100/"+mode, fmt.Sprintf("%s-%d", deploymentID, position), iteration, position, config.RandomSeed
				sample.Mode, sample.WorkloadID, sample.Scale, sample.ResultSHA256, sample.PublicationEligible = mode, "trace", "100", digest, true
				if mode == "rls" {
					sample.System = "postgresql"
				} else {
					sample.System = "taskgate"
					rootIdentity := fmt.Sprintf("%s-%s-%d", deploymentID, mode, iteration)
					if reuseRoot && deployment == 1 && iteration == 2 && mode == "unlimited" {
						rootIdentity = fmt.Sprintf("%s-%s-%d", deploymentID, mode, 1)
					}
					rootBytes := sha256.Sum256([]byte(rootIdentity))
					sample.RootTaskIDHash = hex.EncodeToString(rootBytes[:])
					if mode == "bounded" {
						sample.Rejected, sample.RejectedNoResult, sample.RejectedNoArtifact, sample.RejectedNoSuccessfulAudit = true, true, true, true
					} else {
						sample.ReleaseSetSHA256, sample.DependencySetSHA256, sample.OutcomeSetSHA256 = digest, digest, digest
						sample.ReceiptVersion, sample.ReceiptSHA256, sample.ArtifactIntentSHA256, sample.AvailabilityAuditSHA256 = "8", digest, digest, digest
						sample.ReceiptVerified, sample.ArtifactAvailable = true, true
						sample.ArtifactSHA256, sample.ObjectSHA256 = digest, digest
					}
				}
				traceSteps := 100
				if mode == "bounded" {
					traceSteps = 2
				}
				for step := 1; step <= traceSteps; step++ {
					trace := TraceStep{Index: step, ConcreteSQL: fmt.Sprintf("SELECT %d", step), PriorStateSHA256: digest, ResultSHA256: digest, NextSQLSHA256: digest}
					if sample.System == "taskgate" {
						trace.PlanSHA256, trace.ObservationSHA256 = digest, digest
						trace.ReleaseSetSHA256, trace.DependencySetSHA256, trace.OutcomeSetSHA256 = digest, digest, digest
					}
					if mode == "bounded" && step == traceSteps {
						trace.Rejected, trace.NoResult, trace.NoAvailableArtifact, trace.NoSuccessfulAudit = true, true, true, true
					}
					sample.Trace = append(sample.Trace, trace)
				}
				if err := writer.Write(sample); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return runDir
}

func validTestSample() Sample {
	return Sample{SchemaVersion: 1, CampaignID: "c", DeploymentID: "deployment-01", ExperimentID: "baseline", CellID: "S1/tiny/novel", SampleID: "s1", Iteration: 1, OrderPosition: 1, RandomSeed: 1, System: "taskgate", Mode: "novel", WorkloadID: "S1", Scale: "tiny", PipelineMS: map[string]float64{"prepare": 1, "execute_and_derive": 1, "artifact_stage": 1, "control_settlement": 1, "artifact_publication": 1, "response_finalize": 1, "server_total": 7}, DiagnosticMS: map[string]float64{}, Status: "pass", PublicationEligible: false}
}
