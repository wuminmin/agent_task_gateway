package experiment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

const SampleSchemaVersion = 1

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

type Config struct {
	SchemaVersion      int        `json:"schema_version"`
	CampaignClass      string     `json:"campaign_class"`
	CampaignID         string     `json:"campaign_id"`
	ExperimentID       string     `json:"experiment_id"`
	SubmissionCommit   string     `json:"submission_commit"`
	Deployments        int        `json:"deployments"`
	ProcessReplicates  int        `json:"process_replicates,omitempty"`
	Warmups            int        `json:"warmups"`
	Samples            int        `json:"samples"`
	RandomSeed         int64      `json:"random_seed"`
	FreshRootPerSample bool       `json:"fresh_root_per_sample"`
	KernelOnly         bool       `json:"kernel_only,omitempty"`
	Workloads          []Workload `json:"workloads"`
}

type Workload struct {
	ID         string   `json:"id"`
	Scales     []string `json:"scales"`
	Modes      []string `json:"modes"`
	SQL        string   `json:"sql,omitempty"`
	PlanSHA256 string   `json:"plan_sha256,omitempty"`
}

func LoadConfig(path, expectedExperiment string) (Config, []byte, error) {
	value, err := os.ReadFile(path)
	if err != nil {
		return Config{}, nil, err
	}
	var config Config
	if err := StrictJSON(value, &config); err != nil {
		return Config{}, value, fmt.Errorf("decode config: %w", err)
	}
	if err := config.Validate(expectedExperiment); err != nil {
		return Config{}, value, err
	}
	return config, value, nil
}

func StrictJSON(value []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(value)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("multiple JSON values")
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("trailing JSON value")
	}
	return nil
}

func (config Config) Validate(expectedExperiment string) error {
	if config.SchemaVersion != 1 || strings.TrimSpace(config.CampaignID) == "" ||
		config.ExperimentID == "" || (expectedExperiment != "" && config.ExperimentID != expectedExperiment) ||
		(config.CampaignClass != "pilot" && config.CampaignClass != "publication") ||
		config.Deployments < 1 || config.Warmups < 0 || config.Samples < 1 || config.RandomSeed == 0 || len(config.Workloads) == 0 {
		return errors.New("invalid experiment configuration")
	}
	if config.CampaignClass == "pilot" {
		if config.Deployments != 1 || config.Samples > 3 {
			return errors.New("pilot must use one deployment and at most three samples per cell")
		}
	} else {
		if config.Deployments != 3 || !config.FreshRootPerSample || !fullSHA.MatchString(config.SubmissionCommit) {
			return errors.New("publication config requires three deployments, fresh roots, and a full commit SHA")
		}
		switch config.ExperimentID {
		case "compiler":
			if config.Samples != 100 || config.ProcessReplicates != 5 || config.Warmups < 1 {
				return errors.New("compiler publication config requires 100 samples, five fresh processes, and an untimed warmup")
			}
		case "rls", "attack":
			if config.Samples < 3 {
				return errors.New("adaptive publication config requires at least three paired replicates")
			}
		case "rq5":
			if config.Samples != 4 {
				return errors.New("RQ5 publication config requires four cycles per deployment")
			}
		default:
			if config.Warmups != 5 || config.Samples != 30 {
				return errors.New("publication config requires five warmups and 30 samples per cell")
			}
		}
	}
	seen := map[string]bool{}
	for _, workload := range config.Workloads {
		if workload.ID == "" || seen[workload.ID] || len(workload.Scales) == 0 || len(workload.Modes) == 0 {
			return fmt.Errorf("invalid or duplicate workload %q", workload.ID)
		}
		seen[workload.ID] = true
		scales, modes := map[string]bool{}, map[string]bool{}
		for _, scale := range workload.Scales {
			if scale == "" || scales[scale] {
				return fmt.Errorf("invalid or duplicate scale in workload %q", workload.ID)
			}
			scales[scale] = true
		}
		for _, mode := range workload.Modes {
			if mode == "" || modes[mode] {
				return fmt.Errorf("invalid or duplicate mode in workload %q", workload.ID)
			}
			modes[mode] = true
		}
		if (modes["semantic_replay"] || modes["idempotent_replay"] || modes["normalized_rewrite_replay"]) && !modes["novel"] {
			return fmt.Errorf("replay mode in workload %q requires a novel anchor", workload.ID)
		}
	}
	return nil
}

type Sample struct {
	SchemaVersion             int                `json:"schema_version"`
	CampaignID                string             `json:"campaign_id"`
	DeploymentID              string             `json:"deployment_id"`
	ExperimentID              string             `json:"experiment_id"`
	CellID                    string             `json:"cell_id"`
	SampleID                  string             `json:"sample_id"`
	Iteration                 int                `json:"iteration"`
	ProcessReplicate          int                `json:"process_replicate,omitempty"`
	OrderPosition             int                `json:"order_position"`
	RandomSeed                int64              `json:"random_seed"`
	System                    string             `json:"system"`
	Mode                      string             `json:"mode"`
	WorkloadID                string             `json:"workload_id"`
	Scale                     string             `json:"scale"`
	ClientAvailableMS         float64            `json:"client_available_ms"`
	ClientFullDrainMS         float64            `json:"client_full_drain_ms"`
	GenerationBoundaryMS      float64            `json:"generation_boundary_ms,omitempty"`
	FullTaskGateMS            float64            `json:"full_taskgate_ms,omitempty"`
	PipelineMS                map[string]float64 `json:"pipeline_ms"`
	DiagnosticMS              map[string]float64 `json:"diagnostic_ms"`
	Counters                  map[string]int64   `json:"counters,omitempty"`
	RowCount                  int64              `json:"row_count"`
	ColumnCount               int                `json:"column_count"`
	ResultSHA256              string             `json:"result_sha256"`
	PhysicalSQLSHA256         string             `json:"physical_sql_sha256,omitempty"`
	LogicalSQLSHA256          string             `json:"logical_sql_sha256,omitempty"`
	QueryPlanSHA256           string             `json:"query_plan_sha256,omitempty"`
	ReleaseSetSHA256          string             `json:"release_set_sha256,omitempty"`
	DependencySetSHA256       string             `json:"dependency_set_sha256,omitempty"`
	OutcomeSetSHA256          string             `json:"outcome_set_sha256,omitempty"`
	ArtifactSHA256            string             `json:"artifact_sha256"`
	ObjectSHA256              string             `json:"object_sha256"`
	ActualReleaseFacts        int64              `json:"actual_release_facts"`
	ChargedReleaseFacts       int64              `json:"charged_release_facts"`
	ActualDependencyFacts     int64              `json:"actual_dependency_facts"`
	ChargedDependencyFacts    int64              `json:"charged_dependency_facts"`
	ActualOutcomeFacts        int64              `json:"actual_outcome_facts"`
	ChargedOutcomeFacts       int64              `json:"charged_outcome_facts"`
	PredicateAtomCount        int64              `json:"predicate_atom_count"`
	CompositeCount            int64              `json:"composite_count"`
	SemanticReplay            bool               `json:"semantic_replay"`
	IdempotentReplay          bool               `json:"idempotent_replay"`
	BusinessSQLDelta          int64              `json:"business_sql_delta"`
	RootEpochBefore           int64              `json:"root_epoch_before"`
	RootEpochAfter            int64              `json:"root_epoch_after"`
	RootTaskIDHash            string             `json:"root_task_id_hash,omitempty"`
	RootSetSHA256Before       string             `json:"root_set_sha256_before"`
	RootSetSHA256After        string             `json:"root_set_sha256_after"`
	ParquetBytes              int64              `json:"parquet_bytes"`
	EncryptedObjectBytes      int64              `json:"encrypted_object_bytes"`
	ReceiptVersion            string             `json:"receipt_version"`
	ReceiptSHA256             string             `json:"receipt_sha256"`
	ArtifactIntentSHA256      string             `json:"artifact_intent_sha256"`
	AvailabilityAuditSHA256   string             `json:"availability_audit_sha256"`
	ReceiptVerified           bool               `json:"receipt_verified,omitempty"`
	ArtifactAvailable         bool               `json:"artifact_available,omitempty"`
	Rejected                  bool               `json:"rejected,omitempty"`
	RejectedNoResult          bool               `json:"rejected_no_result,omitempty"`
	RejectedNoArtifact        bool               `json:"rejected_no_artifact,omitempty"`
	RejectedNoSuccessfulAudit bool               `json:"rejected_no_successful_audit,omitempty"`
	CrossEpochReplay          bool               `json:"cross_epoch_replay,omitempty"`
	BudgetViolation           bool               `json:"budget_violation,omitempty"`
	GatewayMemoryPeakBytes    int64              `json:"gateway_memory_peak_bytes"`
	GatewayCPUUsecDelta       int64              `json:"gateway_cpu_usec_delta"`
	GatewayNetworkRXDelta     int64              `json:"gateway_network_rx_delta"`
	GatewayNetworkTXDelta     int64              `json:"gateway_network_tx_delta"`
	ControlWALBytesDelta      int64              `json:"control_wal_bytes_delta"`
	BusinessWALBytesDelta     int64              `json:"business_wal_bytes_delta"`
	Status                    string             `json:"status"`
	ErrorCode                 string             `json:"error_code"`
	PublicationEligible       bool               `json:"publication_eligible"`
	KernelOnly                bool               `json:"kernel_only,omitempty"`
	Reason                    string             `json:"reason,omitempty"`
	Trace                     []TraceStep        `json:"trace,omitempty"`
}

type TraceStep struct {
	Index               int    `json:"index"`
	ConcreteSQL         string `json:"concrete_sql"`
	PriorStateSHA256    string `json:"prior_state_sha256"`
	ResultSHA256        string `json:"result_sha256"`
	NextSQLSHA256       string `json:"next_sql_sha256"`
	PlanSHA256          string `json:"plan_sha256,omitempty"`
	ObservationSHA256   string `json:"observation_sha256,omitempty"`
	ReleaseSetSHA256    string `json:"release_set_sha256,omitempty"`
	DependencySetSHA256 string `json:"dependency_set_sha256,omitempty"`
	OutcomeSetSHA256    string `json:"outcome_set_sha256,omitempty"`
	Rejected            bool   `json:"rejected,omitempty"`
	NoResult            bool   `json:"no_result,omitempty"`
	NoAvailableArtifact bool   `json:"no_available_artifact,omitempty"`
	NoSuccessfulAudit   bool   `json:"no_successful_audit,omitempty"`
}

var requiredPipeline = []string{"prepare", "execute_and_derive", "artifact_stage", "control_settlement", "artifact_publication", "response_finalize", "server_total"}

func (sample Sample) Validate() error {
	if sample.SchemaVersion != SampleSchemaVersion || sample.CampaignID == "" || sample.DeploymentID == "" || sample.ExperimentID == "" ||
		sample.CellID == "" || sample.SampleID == "" || sample.Iteration < 1 || sample.OrderPosition < 1 || sample.RandomSeed == 0 ||
		sample.System == "" || sample.Mode == "" || sample.WorkloadID == "" || sample.Scale == "" ||
		(sample.Status != "pass" && sample.Status != "fail" && sample.Status != "invalid") {
		return errors.New("sample is missing required identity/status fields")
	}
	var sum float64
	for _, name := range requiredPipeline {
		value, present := sample.PipelineMS[name]
		if !present || value < 0 {
			return fmt.Errorf("pipeline_ms.%s is missing or negative", name)
		}
		if name != "server_total" {
			sum += value
		}
	}
	if sample.PipelineMS["server_total"]+0.001 < sum {
		return errors.New("server_total is smaller than the non-overlapping pipeline phase sum")
	}
	if sample.ClientAvailableMS < 0 || sample.ClientFullDrainMS < 0 || sample.RowCount < 0 || sample.ColumnCount < 0 ||
		sample.ActualReleaseFacts < 0 || sample.ActualDependencyFacts < 0 || sample.ActualOutcomeFacts < 0 ||
		sample.ChargedReleaseFacts < 0 || sample.ChargedDependencyFacts < 0 || sample.ChargedOutcomeFacts < 0 ||
		sample.ChargedReleaseFacts > sample.ActualReleaseFacts || sample.ChargedDependencyFacts > sample.ActualDependencyFacts || sample.ChargedOutcomeFacts > sample.ActualOutcomeFacts {
		return errors.New("sample contains invalid measurement values or FactSet cardinalities")
	}
	if sample.SemanticReplay && sample.BusinessSQLDelta != 0 {
		return errors.New("semantic replay executed Business PostgreSQL")
	}
	if sample.Mode == "semantic_replay" && !sample.SemanticReplay {
		return errors.New("semantic replay mode omitted its replay marker")
	}
	if sample.Mode == "idempotent_replay" && !sample.IdempotentReplay {
		return errors.New("idempotent replay mode omitted its replay marker")
	}
	if sample.Status == "pass" && sample.ResultSHA256 != "" && !validSHA256(sample.ResultSHA256) {
		return errors.New("invalid result SHA-256")
	}
	return nil
}

func validSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}

func CanonicalResultHash(rows [][]any) (string, error) {
	encoded := make([][]byte, len(rows))
	for index, row := range rows {
		value, err := json.Marshal(row)
		if err != nil {
			return "", err
		}
		encoded[index] = value
	}
	sort.Slice(encoded, func(i, j int) bool { return string(encoded[i]) < string(encoded[j]) })
	h := sha256.New()
	for _, value := range encoded {
		_, _ = fmt.Fprintf(h, "%d:", len(value))
		_, _ = h.Write(value)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
