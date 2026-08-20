package experiment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/internal/auditchain"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

const (
	// SampleSchemaVersion is the original retained sample contract. It remains
	// the default for passes and failures that never reached v3 finalization.
	SampleSchemaVersion = 1
	// TaskGateRejectionSampleSchemaVersion is the explicit, rejection-only wire
	// revision. A v2 sample proves FinalizeTaskGateObservationV3 was reached and
	// refused the operation; it can carry neither a pass nor an acceptance.
	TaskGateRejectionSampleSchemaVersion = 2
	// FinalizedSampleSchemaVersion is the explicit current post-acceptance wire.
	// It retains both a completed PASS and a failure found by the independent
	// evidence gate after finalizer acceptance. It keeps typed Dataset identity
	// distinct from the live deployment probe and carries the decision-18 root
	// model. Historical sample-v1 and rejection-only v2 are always decoded and
	// validated under their original meanings.
	FinalizedSampleSchemaVersion = 3
)

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)
var campaignIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

const p68CliffDiagnosisEnv = "TASKGATE_P68_CLIFF_DIAGNOSIS"
const p68CliffDiagnosisMarker = "DIAGNOSIS-NOT-FOR-PUBLICATION"

type Config struct {
	SchemaVersion      int        `json:"schema_version"`
	ProtocolVersion    string     `json:"protocol_version,omitempty"`
	ProtocolProfile    string     `json:"protocol_profile,omitempty"`
	ProtocolSHA256     string     `json:"protocol_sha256,omitempty"`
	WorkloadSHA256     string     `json:"workload_manifest_sha256,omitempty"`
	AcceptanceSHA256   string     `json:"acceptance_rules_sha256,omitempty"`
	StatisticsSHA256   string     `json:"statistics_sha256,omitempty"`
	CampaignClass      string     `json:"campaign_class"`
	PilotKind          string     `json:"pilot_kind,omitempty"`
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
	ID         string   `json:"id" yaml:"id"`
	Scales     []string `json:"scales" yaml:"scales"`
	Modes      []string `json:"modes" yaml:"modes"`
	SQL        string   `json:"sql,omitempty" yaml:"sql,omitempty"`
	PlanSHA256 string   `json:"plan_sha256,omitempty" yaml:"plan_sha256,omitempty"`
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
	if config.CampaignClass == "publication" || config.PilotKind == "nonprofile_smoke" {
		if err := config.ValidateProtocol(protocolRoot()); err != nil {
			return Config{}, value, err
		}
	}
	return config, value, nil
}

const finalProtocolVersion = "taskgate-final-v5-wsl2-v1"

func protocolRoot() string {
	if protocolRootOverride != "" {
		return protocolRootOverride
	}
	return filepath.Join("evaluation", "final-v5-wsl2", "protocol")
}

// protocolRootOverride is test-only package state. Publication binaries never
// accept an environment-selected protocol root.
var protocolRootOverride string

// ValidateProtocol binds a publication configuration to the checked-in
// preregistration bytes and to one exact, machine-readable workload profile.
// This validation is repeated by the finalizer; config declarations are never
// treated as the source of truth for publication completeness.
func (config Config) ValidateProtocol(root string) error {
	if config.ProtocolVersion != finalProtocolVersion || config.ProtocolProfile == "" {
		return errors.New("publication config must select the frozen protocol version and profile")
	}
	expectedProfile := config.ExperimentID
	switch config.ExperimentID {
	case "scale":
		expectedProfile = "scale"
		if config.KernelOnly {
			expectedProfile = "scale-extreme"
		}
	case "baseline", "artifact", "rls", "attack", "provsql", "compiler", "concurrency", "rq5":
		// These publication experiments intentionally use a same-named frozen
		// workload profile. Keeping the mapping here prevents a valid profile
		// for one adapter from being rebound to a different experiment.
	default:
		// Source-controlled extension experiments follow the same-name rule;
		// the final campaign still admits only requiredPublicationExperiments.
		if strings.TrimSpace(expectedProfile) == "" {
			return errors.New("publication config lacks an experiment/profile binding")
		}
	}
	if config.ExperimentID != "scale" && config.KernelOnly {
		return fmt.Errorf("kernel_only is not valid for experiment %q", config.ExperimentID)
	}
	if config.ProtocolProfile != expectedProfile {
		return fmt.Errorf("experiment %q requires frozen protocol profile %q", config.ExperimentID, expectedProfile)
	}
	files := map[string]string{
		"protocol-v1.yaml":         config.ProtocolSHA256,
		"workloads-v1.yaml":        config.WorkloadSHA256,
		"acceptance-rules-v1.yaml": config.AcceptanceSHA256,
		"statistics-v1.yaml":       config.StatisticsSHA256,
	}
	for name, expected := range files {
		path := filepath.Join(root, name)
		actual, err := FileSHA256(path)
		if err != nil {
			return fmt.Errorf("read frozen protocol %s: %w", name, err)
		}
		if !validSHA256(expected) || actual != expected {
			return fmt.Errorf("frozen protocol digest mismatch for %s", name)
		}
	}
	protocolBytes, err := os.ReadFile(filepath.Join(root, "protocol-v1.yaml"))
	if err != nil {
		return err
	}
	type replicateContract struct {
		Profiles                         []string `yaml:"profiles"`
		ProcessReplicates                int      `yaml:"process_replicates"`
		WarmupsPerCellPerProcess         int      `yaml:"warmups_per_cell_per_process"`
		MeasuredSamplesPerCellPerProcess int      `yaml:"measured_samples_per_cell_per_process"`
	}
	var protocol struct {
		ProtocolID string `yaml:"protocol_id"`
		Campaign   struct {
			ReplicateContracts map[string]replicateContract `yaml:"replicate_contracts"`
		} `yaml:"campaign"`
	}
	if err := yaml.Unmarshal(protocolBytes, &protocol); err != nil || protocol.ProtocolID != finalProtocolVersion {
		return errors.New("source-controlled protocol ID does not match config")
	}
	profileContracts := make(map[string]replicateContract)
	for name, contract := range protocol.Campaign.ReplicateContracts {
		if strings.TrimSpace(name) == "" || len(contract.Profiles) == 0 || contract.ProcessReplicates < 1 ||
			contract.WarmupsPerCellPerProcess < 0 || contract.MeasuredSamplesPerCellPerProcess < 1 {
			return errors.New("frozen protocol contains an invalid replicate contract")
		}
		for _, profileName := range contract.Profiles {
			if profileName == "" || profileName != strings.TrimSpace(profileName) {
				return errors.New("frozen protocol replicate contract contains an invalid profile")
			}
			if _, duplicate := profileContracts[profileName]; duplicate {
				return fmt.Errorf("frozen protocol profile %q belongs to multiple replicate contracts", profileName)
			}
			profileContracts[profileName] = contract
		}
	}
	selectedContract, present := profileContracts[config.ProtocolProfile]
	if !present {
		return fmt.Errorf("frozen protocol profile %q lacks a replicate contract", config.ProtocolProfile)
	}
	effectiveProcesses := config.ProcessReplicates
	if effectiveProcesses == 0 {
		effectiveProcesses = 1
	}
	if effectiveProcesses != selectedContract.ProcessReplicates || config.Warmups != selectedContract.WarmupsPerCellPerProcess ||
		config.Samples != selectedContract.MeasuredSamplesPerCellPerProcess {
		return fmt.Errorf("config replicate counts differ from frozen protocol profile %q", config.ProtocolProfile)
	}
	workloadBytes, err := os.ReadFile(filepath.Join(root, "workloads-v1.yaml"))
	if err != nil {
		return err
	}
	var manifest struct {
		SchemaVersion int `yaml:"schema_version"`
		Profiles      map[string]struct {
			Workloads []Workload `yaml:"workloads"`
		} `yaml:"profiles"`
	}
	if err := yaml.Unmarshal(workloadBytes, &manifest); err != nil {
		return fmt.Errorf("decode workload manifest: %w", err)
	}
	if len(profileContracts) != len(manifest.Profiles) {
		return errors.New("frozen workload profiles and replicate-contract profiles differ")
	}
	for profileName := range manifest.Profiles {
		if _, present := profileContracts[profileName]; !present {
			return fmt.Errorf("frozen workload profile %q lacks a replicate contract", profileName)
		}
	}
	profile, ok := manifest.Profiles[config.ProtocolProfile]
	if manifest.SchemaVersion != 2 || !ok || len(profile.Workloads) == 0 {
		return fmt.Errorf("unknown or invalid protocol profile %q", config.ProtocolProfile)
	}
	if !reflect.DeepEqual(config.Workloads, profile.Workloads) {
		return fmt.Errorf("config workload cells differ from frozen protocol profile %q", config.ProtocolProfile)
	}
	return nil
}

func StrictJSON(value []byte, target any) error {
	if len(value) == 0 {
		return errors.New("empty JSON")
	}
	tokens := json.NewDecoder(bytes.NewReader(value))
	tokens.UseNumber()
	if err := consumeStrictJSONValue(tokens); err != nil {
		return err
	}
	if _, err := tokens.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return fmt.Errorf("trailing JSON: %w", err)
	}

	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return fmt.Errorf("trailing JSON: %w", err)
	}
	return nil
}

// rejectExplicitNullObjectMembers distinguishes an omitted optional member
// from a member that is present with JSON null. Pointer-backed Go fields alone
// cannot make that distinction, while the evidence schemas deliberately do.
func rejectExplicitNullObjectMembers(value []byte, names ...string) error {
	var members map[string]json.RawMessage
	if err := StrictJSON(value, &members); err != nil {
		return err
	}
	for _, name := range names {
		member, present := members[name]
		if present && bytes.Equal(bytes.TrimSpace(member), []byte("null")) {
			return fmt.Errorf("JSON object member %q cannot be null", name)
		}
	}
	return nil
}

// consumeStrictJSONValue rejects duplicate object members recursively. Go's
// typed decoder intentionally accepts the last duplicate member, which is not
// an acceptable ambiguity in an evidence contract.
func consumeStrictJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object member name is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON object member %q", key)
			}
			seen[key] = struct{}{}
			if err := consumeStrictJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return errors.New("unterminated JSON object")
		}
	case '[':
		for decoder.More() {
			if err := consumeStrictJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return errors.New("unterminated JSON array")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
}

// PilotKinds are the reviewed non-publication run classes.
// profile_activation_smoke exercises Catalog-bound activation and isolation
// without executing a workload cell; artifact_targeted executes the frozen
// Artifact cells for validation only, and baseline_targeted does the same for
// Baseline S1 and S2. All three bind every arm to a deployment profile;
// synthetic_smoke never does.
var PilotKinds = [...]string{"synthetic_smoke", "real_system", "profile_activation_smoke", "artifact_targeted", "baseline_targeted", "nonprofile_smoke"}

// ValidPilotKind reports whether a pilot declares a reviewed run class.
func ValidPilotKind(kind string) bool {
	for _, candidate := range PilotKinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

func (config Config) Validate(expectedExperiment string) error {
	if config.SchemaVersion != 1 || !campaignIDPattern.MatchString(config.CampaignID) ||
		config.ExperimentID == "" || (expectedExperiment != "" && config.ExperimentID != expectedExperiment) ||
		(config.CampaignClass != "pilot" && config.CampaignClass != "publication") ||
		config.Deployments < 1 || config.ProcessReplicates < 0 || config.Warmups < 0 || config.Samples < 1 || config.RandomSeed == 0 || len(config.Workloads) == 0 {
		return errors.New("invalid experiment configuration")
	}
	if config.CampaignClass == "pilot" {
		if !ValidPilotKind(config.PilotKind) {
			return errors.New("pilot must declare a reviewed pilot kind")
		}
		if config.PilotKind == "nonprofile_smoke" {
			if config.Deployments != 3 || !config.FreshRootPerSample || !fullSHA.MatchString(config.SubmissionCommit) ||
				config.ProtocolVersion == "" || config.ProtocolProfile == "" || !validSHA256(config.ProtocolSHA256) ||
				!validSHA256(config.WorkloadSHA256) || !validSHA256(config.AcceptanceSHA256) || !validSHA256(config.StatisticsSHA256) ||
				(config.ProtocolProfile != "scale" && config.ProtocolProfile != "scale-extreme" && config.ProtocolProfile != "compiler") {
				return errors.New("non-profile smoke pilot requires three fresh executions and frozen scale/compiler protocol bindings")
			}
		} else if os.Getenv(p68CliffDiagnosisEnv) == p68CliffDiagnosisMarker && !config.isP68CliffDiagnosis() {
			return errors.New("P68 diagnosis requires the exact reviewed concurrency density and execution shape")
		} else if os.Getenv(p68CliffDiagnosisEnv) != p68CliffDiagnosisMarker &&
			(config.Deployments != 1 || config.Samples > 3) {
			return errors.New("pilot must declare a reviewed pilot kind, use one deployment, and use at most three samples per cell")
		}
	} else {
		if config.PilotKind != "" {
			return errors.New("publication config must not declare a pilot kind")
		}
		if config.Deployments != 3 || !config.FreshRootPerSample || !fullSHA.MatchString(config.SubmissionCommit) {
			return errors.New("publication config requires three deployments, fresh roots, and a full commit SHA")
		}
		if config.ProtocolVersion == "" || config.ProtocolProfile == "" || !validSHA256(config.ProtocolSHA256) ||
			!validSHA256(config.WorkloadSHA256) || !validSHA256(config.AcceptanceSHA256) || !validSHA256(config.StatisticsSHA256) {
			return errors.New("publication config lacks frozen protocol bindings")
		}
		switch config.ExperimentID {
		case "compiler":
			if config.Samples != 100 || config.ProcessReplicates != 5 || config.Warmups != 1 {
				return errors.New("compiler publication config requires 100 samples, five fresh processes, and exactly one untimed warmup")
			}
		case "rls", "attack":
			if config.Samples != 3 || config.Warmups != 0 || config.ProcessReplicates > 1 {
				return errors.New("adaptive publication config requires exactly three paired replicates, no warmups, and one process")
			}
		case "rq5":
			if config.Samples != 4 || config.Warmups != 0 || config.ProcessReplicates > 1 {
				return errors.New("RQ5 publication config requires four cycles, no warmups, and one process per deployment")
			}
		default:
			if config.Warmups != 5 || config.Samples != 30 || config.ProcessReplicates > 1 {
				return errors.New("publication config requires five warmups, 30 samples per cell, and one process")
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

func (config Config) isP68CliffDiagnosis() bool {
	return os.Getenv(p68CliffDiagnosisEnv) == p68CliffDiagnosisMarker &&
		config.CampaignClass == "pilot" && config.PilotKind == "real_system" &&
		config.ExperimentID == "concurrency" && config.Deployments == 1 &&
		config.ProcessReplicates == 1 && config.Warmups == 5 && config.Samples == 30 &&
		config.FreshRootPerSample
}

type Sample struct {
	SchemaVersion          int                `json:"schema_version"`
	CampaignID             string             `json:"campaign_id"`
	DeploymentID           string             `json:"deployment_id"`
	ExperimentID           string             `json:"experiment_id"`
	CellID                 string             `json:"cell_id"`
	SampleID               string             `json:"sample_id"`
	Iteration              int                `json:"iteration"`
	ProcessReplicate       int                `json:"process_replicate"`
	Warmup                 bool               `json:"warmup"`
	OrderPosition          int                `json:"order_position"`
	RandomSeed             int64              `json:"random_seed"`
	PairID                 string             `json:"pair_id"`
	PairedSystemOrder      string             `json:"paired_system_order"`
	RootGroupID            string             `json:"root_group_id"`
	System                 string             `json:"system"`
	Mode                   string             `json:"mode"`
	WorkloadID             string             `json:"workload_id"`
	Scale                  string             `json:"scale"`
	ClientAvailableMS      float64            `json:"client_available_ms"`
	ClientFullDrainMS      float64            `json:"client_full_drain_ms"`
	GenerationBoundaryMS   float64            `json:"generation_boundary_ms,omitempty"`
	FullTaskGateMS         float64            `json:"full_taskgate_ms,omitempty"`
	PipelineMS             map[string]float64 `json:"pipeline_ms"`
	DiagnosticMS           map[string]float64 `json:"diagnostic_ms"`
	Counters               map[string]int64   `json:"counters,omitempty"`
	RowCount               int64              `json:"row_count"`
	ColumnCount            int                `json:"column_count"`
	ResultSHA256           string             `json:"result_sha256"`
	PhysicalSQLSHA256      string             `json:"physical_sql_sha256,omitempty"`
	LogicalSQLSHA256       string             `json:"logical_sql_sha256,omitempty"`
	QueryPlanSHA256        string             `json:"query_plan_sha256,omitempty"`
	ReleaseSetSHA256       string             `json:"release_set_sha256,omitempty"`
	DependencySetSHA256    string             `json:"dependency_set_sha256,omitempty"`
	OutcomeSetSHA256       string             `json:"outcome_set_sha256,omitempty"`
	ArtifactSHA256         string             `json:"artifact_sha256"`
	ObjectSHA256           string             `json:"object_sha256"`
	ActualReleaseFacts     int64              `json:"actual_release_facts"`
	ChargedReleaseFacts    int64              `json:"charged_release_facts"`
	ActualDependencyFacts  int64              `json:"actual_dependency_facts"`
	ChargedDependencyFacts int64              `json:"charged_dependency_facts"`
	ActualOutcomeFacts     int64              `json:"actual_outcome_facts"`
	ChargedOutcomeFacts    int64              `json:"charged_outcome_facts"`
	PredicateAtomCount     int64              `json:"predicate_atom_count"`
	CompositeCount         int64              `json:"composite_count"`
	SemanticReplay         bool               `json:"semantic_replay"`
	IdempotentReplay       bool               `json:"idempotent_replay"`
	BusinessSQLDelta       int64              `json:"business_sql_delta"`
	// TaskGateAcceptanceV3 is the finalizer's own acceptance record for this
	// operation, retained on the paths that have been cut over to v3.
	//
	// It is the finalizer's OUTPUT, not the Adapter's claim: every member of it
	// was derived from frozen contract material and independently obtained
	// deployment evidence, and the Adapter never sees the inputs it came from.
	// A sample carries it so a later reader can see which plan, classification
	// and observed delta the operation was accepted under, rather than only that
	// it was.
	//
	TaskGateAcceptanceV3      *FinalizationV3                   `json:"taskgate_acceptance_v3,omitempty"`
	TaskGateRejectionV1       *TaskGateRejectionV1              `json:"taskgate_rejection_v1,omitempty"`
	RootEpochBefore           int64                             `json:"root_epoch_before"`
	RootEpochAfter            int64                             `json:"root_epoch_after"`
	RootTaskIDHash            string                            `json:"root_task_id_hash,omitempty"`
	RootSetSHA256Before       string                            `json:"root_set_sha256_before"`
	RootSetSHA256After        string                            `json:"root_set_sha256_after"`
	ParquetBytes              int64                             `json:"parquet_bytes"`
	EncryptedObjectBytes      int64                             `json:"encrypted_object_bytes"`
	ReceiptVersion            string                            `json:"receipt_version"`
	ReceiptSHA256             string                            `json:"receipt_sha256"`
	ArtifactIntentSHA256      string                            `json:"artifact_intent_sha256"`
	AvailabilityAuditSHA256   string                            `json:"availability_audit_sha256"`
	ReceiptVerified           bool                              `json:"receipt_verified,omitempty"`
	ArtifactAvailable         bool                              `json:"artifact_available,omitempty"`
	Rejected                  bool                              `json:"rejected,omitempty"`
	RejectedNoResult          bool                              `json:"rejected_no_result,omitempty"`
	RejectedNoArtifact        bool                              `json:"rejected_no_artifact,omitempty"`
	RejectedNoSuccessfulAudit bool                              `json:"rejected_no_successful_audit,omitempty"`
	CrossEpochReplay          bool                              `json:"cross_epoch_replay,omitempty"`
	BudgetViolation           bool                              `json:"budget_violation,omitempty"`
	GatewayMemoryPeakBytes    int64                             `json:"gateway_memory_peak_bytes"`
	GatewayCPUUsecDelta       int64                             `json:"gateway_cpu_usec_delta"`
	GatewayNetworkRXDelta     int64                             `json:"gateway_network_rx_delta"`
	GatewayNetworkTXDelta     int64                             `json:"gateway_network_tx_delta"`
	ControlWALBytesDelta      int64                             `json:"control_wal_bytes_delta"`
	BusinessWALBytesDelta     int64                             `json:"business_wal_bytes_delta"`
	Status                    string                            `json:"status"`
	ErrorCode                 string                            `json:"error_code"`
	PublicationEligible       bool                              `json:"publication_eligible"`
	KernelOnly                bool                              `json:"kernel_only,omitempty"`
	Reason                    string                            `json:"reason,omitempty"`
	Trace                     []TraceStep                       `json:"trace,omitempty"`
	BaselineVerification      *BaselineVerificationEvidence     `json:"baseline_verification,omitempty"`
	ScaleVerification         *ScaleVerificationEvidence        `json:"scale_verification,omitempty"`
	ArtifactVerification      *ArtifactVerificationEvidence     `json:"artifact_verification,omitempty"`
	ReplayVerification        *ReplayVerificationEvidence       `json:"replay_verification,omitempty"`
	CrossBindingVerification  *CrossBindingVerificationEvidence `json:"cross_binding_verification,omitempty"`
	IdempotentVerification    *IdempotentVerificationEvidence   `json:"idempotent_verification,omitempty"`
	RecoveryVerification      *RecoveryVerificationEvidence     `json:"recovery_verification,omitempty"`
	RLSVerification           *RLSVerificationEvidence          `json:"rls_verification,omitempty"`
	AttackVerification        *AttackVerificationEvidence       `json:"attack_verification,omitempty"`
	ProvSQLVerification       *ProvSQLVerificationEvidence      `json:"provsql_verification,omitempty"`
	CompilerVerification      *CompilerVerificationEvidence     `json:"compiler_verification,omitempty"`
	ConcurrencyVerification   *ConcurrencyVerification          `json:"concurrency_verification,omitempty"`
	RQ5Verification           *RQ5VerificationEvidence          `json:"rq5_verification,omitempty"`
	// ProfileBinding names the deployment profile this arm ran against. Every
	// arm of one cell must agree, including a Direct arm that never reaches the
	// Gateway but must still read the same Dataset, Catalog and Publication.
	ProfileBinding *ProfileBinding `json:"profile_binding,omitempty"`
}

// UnmarshalJSON preserves the schema distinction between absence and explicit
// null for the mutually exclusive finalizer adjudication records. The alias
// keeps StrictJSON's recursive duplicate-member and unknown-field checks
// without recursively invoking this method.
func (sample *Sample) UnmarshalJSON(value []byte) error {
	if sample == nil {
		return errors.New("cannot decode a sample into nil")
	}
	var members map[string]json.RawMessage
	if err := StrictJSON(value, &members); err != nil {
		return err
	}
	for _, name := range []string{"taskgate_acceptance_v3", "taskgate_rejection_v1"} {
		member, present := members[name]
		if present && bytes.Equal(bytes.TrimSpace(member), []byte("null")) {
			return fmt.Errorf("JSON object member %q cannot be null", name)
		}
	}
	var schemaVersion int
	if encoded, present := members["schema_version"]; present {
		if err := json.Unmarshal(encoded, &schemaVersion); err != nil {
			return fmt.Errorf("decode sample schema_version: %w", err)
		}
	}
	var experimentID string
	if encoded, present := members["experiment_id"]; present {
		if err := json.Unmarshal(encoded, &experimentID); err != nil {
			return fmt.Errorf("decode sample experiment_id: %w", err)
		}
	}
	outcomeVerificationPresent := false
	if encoded, present := members["taskgate_acceptance_v3"]; present {
		var acceptanceMembers map[string]json.RawMessage
		if err := StrictJSON(encoded, &acceptanceMembers); err != nil {
			return fmt.Errorf("decode taskgate_acceptance_v3 wire: %w", err)
		}
		_, outcomeVerificationPresent = acceptanceMembers["outcome_candidate_verification"]
	}
	var scaleVerificationVersion string
	if encoded, present := members["scale_verification"]; present &&
		!bytes.Equal(bytes.TrimSpace(encoded), []byte("null")) {
		var evidenceMembers map[string]json.RawMessage
		if err := StrictJSON(encoded, &evidenceMembers); err != nil {
			return fmt.Errorf("decode scale_verification wire: %w", err)
		}
		outcomeNames := []string{
			"expected_outcome_member_cardinality", "observed_outcome_member_cardinality",
			"expected_outcome_candidate_set_sha256", "observed_outcome_candidate_set_sha256",
		}
		linkNames := []string{"history_dependency_link", "candidate_dependency_link",
			"root_before_dependency_link", "root_after_dependency_link"}
		if schemaVersion == SampleSchemaVersion || schemaVersion == TaskGateRejectionSampleSchemaVersion {
			versionedNames := []string{
				"expected_existing_facts", "expected_union_facts",
				"existing_dependency_sha256", "union_dependency_sha256",
				outcomeNames[0], outcomeNames[1], outcomeNames[2], outcomeNames[3],
			}
			versionedNames = append(versionedNames, linkNames...)
			for _, name := range versionedNames {
				if _, present := evidenceMembers[name]; present {
					return fmt.Errorf("sample-v1/v2 cannot carry sample-v3 Scale member %q", name)
				}
			}
		}
		if encodedVersion, present := evidenceMembers["version"]; present {
			if err := json.Unmarshal(encodedVersion, &scaleVerificationVersion); err != nil {
				return fmt.Errorf("decode scale_verification version: %w", err)
			}
		}
		switch scaleVerificationVersion {
		case scaleEvidenceVersion, scaleDependencyEvidenceVersionV2:
			for _, name := range append(append([]string(nil), outcomeNames...), linkNames...) {
				if _, present := evidenceMembers[name]; present {
					return fmt.Errorf("Scale evidence version %q cannot carry later-version member %q", scaleVerificationVersion, name)
				}
			}
		case scaleDependencyEvidenceVersionV3:
			for _, name := range linkNames {
				if _, present := evidenceMembers[name]; present {
					return fmt.Errorf("Scale evidence-v3 cannot carry evidence-v4 member %q", name)
				}
			}
			for _, name := range outcomeNames {
				if _, present := evidenceMembers[name]; !present {
					return fmt.Errorf("Scale evidence version %q omits required member %q", scaleVerificationVersion, name)
				}
			}
		case scaleDependencyEvidenceVersionV4, scaleDependencyEvidenceVersionV5:
			for _, name := range append(append([]string(nil), outcomeNames...),
				"candidate_dependency_link", "root_before_dependency_link", "root_after_dependency_link") {
				if _, present := evidenceMembers[name]; !present {
					return fmt.Errorf("Scale evidence version %q omits required member %q", scaleVerificationVersion, name)
				}
			}
		}
	}
	if outcomeVerificationPresent &&
		(schemaVersion != FinalizedSampleSchemaVersion || experimentID != "scale" ||
			(scaleVerificationVersion != scaleDependencyEvidenceVersionV3 &&
				scaleVerificationVersion != scaleDependencyEvidenceVersionV4 &&
				scaleVerificationVersion != scaleDependencyEvidenceVersionV5)) {
		return errors.New("Outcome candidate verification is reserved for sample-v3 Scale evidence-v3/v4")
	}
	type sampleWire Sample
	var decoded sampleWire
	if err := StrictJSON(value, &decoded); err != nil {
		return err
	}
	*sample = Sample(decoded)
	return nil
}

type BaselineVerificationEvidence struct {
	Receipt                 queryreceipt.QueryReceiptV1    `json:"receipt"`
	KeyBundle               queryreceipt.PublicKeyBundleV1 `json:"key_bundle"`
	AuditProof              auditchain.InclusionProof      `json:"audit_proof"`
	TerminalProof           auditchain.InclusionProof      `json:"terminal_proof"`
	RegistrationProof       auditchain.InclusionProof      `json:"registration_proof"`
	AvailabilityProof       auditchain.InclusionProof      `json:"availability_proof"`
	ArtifactStatus          string                         `json:"artifact_status"`
	DownloadedParquetSHA256 string                         `json:"downloaded_parquet_sha256"`
	ParsedResultSHA256      string                         `json:"parsed_result_sha256"`
	VerifierManifest        *RedactedVerifierManifest      `json:"verifier_manifest,omitempty"`
}

// ScaleVerificationEvidence binds each scale result to the private, redacted
// deployment binding and to observations made independently of the response
// fields returned by Gateway. Boundary selects exactly one of DependencyE2E,
// OutcomeMerkle, or KernelStorage.
type ScaleVerificationEvidence struct {
	Version  string `json:"version"`
	Boundary string `json:"boundary"`
	// BindingFileSHA256 is the Adapter-retained exact private input file identity.
	// The deployment finalizer independently embeds that identity in its accepted
	// operation, and retained validation requires the two copies to agree exactly.
	// BindingSHA256 remains the canonical versioned final_v5_adapter section identity so
	// harmless top-level formatting and executable section material cannot be
	// conflated.
	BindingFileSHA256 string `json:"binding_file_sha256,omitempty"`
	BindingSHA256     string `json:"binding_sha256,omitempty"`
	DatasetSHA256     string `json:"dataset_sha256,omitempty"`
	// CatalogSHA256 is the master Catalog identity covered by the private
	// binding's LoadPublicationFile provenance check. For current profile
	// deployments, the signed live Catalog identity is ProfileBinding.CatalogSHA256.
	CatalogSHA256          string `json:"catalog_sha256,omitempty"`
	DatasetProbeSHA256     string `json:"dataset_probe_sha256,omitempty"`
	QuerySHA256            string `json:"query_sha256,omitempty"`
	ExpectedRows           int64  `json:"expected_rows,omitempty"`
	ExpectedColumns        int    `json:"expected_columns,omitempty"`
	ExpectedResultSHA256   string `json:"expected_result_sha256,omitempty"`
	ExpectedCandidateFacts int64  `json:"expected_candidate_facts,omitempty"`
	ObservedCandidateFacts int64  `json:"observed_candidate_facts,omitempty"`
	ExpectedExistingFacts  int64  `json:"expected_existing_facts,omitempty"`
	ExpectedOverlapFacts   int64  `json:"expected_overlap_facts,omitempty"`
	ObservedOverlapFacts   int64  `json:"observed_overlap_facts,omitempty"`
	ExpectedUnionFacts     int64  `json:"expected_union_facts,omitempty"`
	// The Outcome members are the four exact predicate-atom Fact identities and
	// their signed composite Fact identity. These digests use the independent
	// ordinary-set oracle domain, not the production radix-set domain carried by
	// Sample.OutcomeSetSHA256 and the signed receipt.
	ExpectedOutcomeMemberCardinality  int64                             `json:"expected_outcome_member_cardinality,omitempty"`
	ObservedOutcomeMemberCardinality  int64                             `json:"observed_outcome_member_cardinality,omitempty"`
	ExpectedOutcomeCandidateSetSHA256 string                            `json:"expected_outcome_candidate_set_sha256,omitempty"`
	ObservedOutcomeCandidateSetSHA256 string                            `json:"observed_outcome_candidate_set_sha256,omitempty"`
	HistoryDependencySHA256           string                            `json:"history_dependency_sha256,omitempty"`
	ExistingDependencySHA256          string                            `json:"existing_dependency_sha256,omitempty"`
	CandidateDependencySHA256         string                            `json:"candidate_dependency_sha256,omitempty"`
	UnionDependencySHA256             string                            `json:"union_dependency_sha256,omitempty"`
	HistoryDependencyLink             *ScaleDependencySetVerificationV1 `json:"history_dependency_link,omitempty"`
	CandidateDependencyLink           *ScaleDependencySetVerificationV1 `json:"candidate_dependency_link,omitempty"`
	RootBeforeDependencyLink          *ScaleDependencySetVerificationV1 `json:"root_before_dependency_link,omitempty"`
	RootAfterDependencyLink           *ScaleDependencySetVerificationV1 `json:"root_after_dependency_link,omitempty"`
	BusinessBefore                    BusinessSQLSnapshot               `json:"business_before,omitempty"`
	BusinessAfter                     BusinessSQLSnapshot               `json:"business_after,omitempty"`
	RootBefore                        RootLedgerSnapshot                `json:"root_before,omitempty"`
	RootAfter                         RootLedgerSnapshot                `json:"root_after,omitempty"`
	SourceObservationSHA256           string                            `json:"source_observation_sha256,omitempty"`
	ReplayObservationSHA256           string                            `json:"replay_observation_sha256,omitempty"`
	// ObserverWindow retains the v3 finalizer interval for dependency-e2e
	// TaskGate cells. It is absent on the Outcome-Merkle and kernel-storage
	// controls, which do not execute a fresh governed Task operation.
	ObserverWindow *ObserverWindowV2      `json:"observer_window,omitempty"`
	OutcomeMerkle  *OutcomeMerkleEvidence `json:"outcome_merkle,omitempty"`
	KernelStorage  *KernelStorageEvidence `json:"kernel_storage,omitempty"`
}

// OutcomeMerkleEvidence is emitted only by the production PostgreSQL-backed
// V5 radix evaluation hook. The ordinary-set oracle digests use a separate
// domain from the production Merkle root digests.
type OutcomeMerkleEvidence struct {
	ProductionPath              string  `json:"production_path"`
	ContentCachePolicy          string  `json:"content_cache_policy"`
	OverlapRounding             string  `json:"overlap_rounding"`
	FixtureSHA256               string  `json:"fixture_sha256"`
	BackendRunSHA256            string  `json:"backend_run_sha256"`
	RootCardinality             int64   `json:"root_cardinality"`
	CandidateCardinality        int64   `json:"candidate_cardinality"`
	OverlapCardinality          int64   `json:"overlap_cardinality"`
	NovelCardinality            int64   `json:"novel_cardinality"`
	UnionCardinality            int64   `json:"union_cardinality"`
	RootMemberOracleSHA256      string  `json:"root_member_oracle_sha256"`
	CandidateMemberOracleSHA256 string  `json:"candidate_member_oracle_sha256"`
	UnionMemberOracleSHA256     string  `json:"union_member_oracle_sha256"`
	ObservedUnionMemberSHA256   string  `json:"observed_union_member_sha256"`
	ProductionRootSHA256        string  `json:"production_root_sha256"`
	ProductionUnionSHA256       string  `json:"production_union_sha256"`
	ReplayUnionSHA256           string  `json:"replay_union_sha256"`
	BlocksLoaded                int64   `json:"blocks_loaded"`
	LeavesLoaded                int64   `json:"leaves_loaded"`
	HashesLoaded                int64   `json:"hashes_loaded"`
	BlocksReused                int64   `json:"blocks_reused"`
	LeavesChanged               int64   `json:"leaves_changed"`
	ChangedObjects              int64   `json:"changed_objects"`
	ReplayChangedObjects        int64   `json:"replay_changed_objects"`
	StorageObjectsBefore        int64   `json:"storage_objects_before"`
	StorageObjectsAfter         int64   `json:"storage_objects_after"`
	StorageBytesBefore          int64   `json:"storage_bytes_before"`
	StorageBytesAfter           int64   `json:"storage_bytes_after"`
	HeapAllocBytesAfter         int64   `json:"heap_alloc_bytes_after"`
	LoadMS                      float64 `json:"load_ms"`
	DifferenceUnionMS           float64 `json:"difference_union_ms"`
	PersistMS                   float64 `json:"persist_ms"`
}

// KernelStorageEvidence records the opt-in 10M/100M production ordinal
// BitmapSet algebra plus canonical portable-container round trip. It is not
// SQL or TaskGate end-to-end evidence.
type KernelStorageEvidence struct {
	ProductionPath        string  `json:"production_path"`
	FixtureSHA256         string  `json:"fixture_sha256"`
	RunIdentitySHA256     string  `json:"run_identity_sha256"`
	ExpectedCardinality   int64   `json:"expected_cardinality"`
	CandidateCardinality  int64   `json:"candidate_cardinality"`
	DifferenceCardinality int64   `json:"difference_cardinality"`
	UnionCardinality      int64   `json:"union_cardinality"`
	CandidateSHA256       string  `json:"candidate_sha256"`
	DifferenceSHA256      string  `json:"difference_sha256"`
	UnionSHA256           string  `json:"union_sha256"`
	RoundTripSHA256       string  `json:"round_trip_sha256"`
	SegmentCount          int64   `json:"segment_count"`
	ContainerCount        int64   `json:"container_count"`
	StorageBytes          int64   `json:"storage_bytes"`
	AllocatedBytes        int64   `json:"allocated_bytes"`
	Allocations           int64   `json:"allocations"`
	HeapAllocBytesAfter   int64   `json:"heap_alloc_bytes_after"`
	DifferenceMS          float64 `json:"difference_ms"`
	UnionMS               float64 `json:"union_ms"`
	CardinalityMS         float64 `json:"cardinality_ms"`
	StorageRoundTripMS    float64 `json:"storage_round_trip_ms"`
}

// ArtifactVerificationEvidence proves that a result-heavy label is the exact
// typed Parquet result that was requested, signed, committed, observed, and
// completely drained. Raw SQL, rows, object keys, and credentials are never
// retained.
type ArtifactVerificationEvidence struct {
	Version string `json:"version"`
	// BindingSHA256 identifies the public Contract-Bridge deployment record.
	// It is not the private final_v5_adapter section identity consumed by Scale
	// and ProvSQL, and publication finalization must not compare the two.
	BindingSHA256        string              `json:"binding_sha256"`
	DatasetSHA256        string              `json:"dataset_sha256"`
	CatalogSHA256        string              `json:"catalog_sha256"`
	DatasetProbeSHA256   string              `json:"dataset_probe_sha256"`
	QuerySHA256          string              `json:"query_sha256"`
	ExpectedRows         int64               `json:"expected_rows"`
	ExpectedColumns      int                 `json:"expected_columns"`
	ExpectedResultSHA256 string              `json:"expected_result_sha256"`
	ObservedRows         int64               `json:"observed_rows"`
	ObservedColumns      int                 `json:"observed_columns"`
	ObservedResultSHA256 string              `json:"observed_result_sha256"`
	BusinessBefore       BusinessSQLSnapshot `json:"business_before"`
	BusinessAfter        BusinessSQLSnapshot `json:"business_after"`
	RootBefore           RootLedgerSnapshot  `json:"root_before"`
	RootAfter            RootLedgerSnapshot  `json:"root_after"`
	// ObserverWindow is the interval the v3 accounting was settled over.
	//
	// It replaced a pair of schema-v1 ObserverSnapshots. Those carried the
	// role-wide total and the structural census as separate readings, so the two
	// could describe different instants with nothing able to tell; and they left
	// the runtime and image identities to the Adapter, which is the party the
	// finalizer is supposed to be independent of. A window is the pair, and
	// ObserverSnapshotV2.Validate makes its atomicity checkable.
	ObserverWindow ObserverWindowV2 `json:"observer_window"`
}

// BusinessSQLSnapshot is an independently observed pg_stat_statements sample.
// The reset epoch and deallocation counter make a counter reset (or eviction)
// distinguishable from a genuine zero-delta replay.
type BusinessSQLSnapshot struct {
	StatsResetUnixMicro int64 `json:"stats_reset_unix_micro"`
	Dealloc             int64 `json:"dealloc"`
	VisibleCalls        int64 `json:"visible_calls"`
	CompanionCalls      int64 `json:"companion_calls"`
}

// RootLedgerSnapshot is the complete, redacted root-head state needed by the
// Pilot replay checks. It contains no FactID or payload bytes.
type RootLedgerSnapshot struct {
	Epoch                    int64  `json:"epoch"`
	DictionarySetSHA256      string `json:"dictionary_set_sha256"`
	ReleaseSetSHA256         string `json:"release_set_sha256"`
	ReleaseCardinality       int64  `json:"release_cardinality"`
	DependencySetSHA256      string `json:"dependency_set_sha256"`
	DependencyCardinality    int64  `json:"dependency_cardinality"`
	OutcomeSetSHA256         string `json:"outcome_set_sha256"`
	OutcomeCardinality       int64  `json:"outcome_cardinality"`
	RootObservationSetSHA256 string `json:"root_observation_set_sha256"`
	RootObservationCount     int64  `json:"root_observation_count"`
}

// ReplayVerificationEvidence binds an ordinary semantic replay to independent
// Business SQL counters and complete root snapshots.
type ReplayVerificationEvidence struct {
	BusinessBefore          BusinessSQLSnapshot `json:"business_before"`
	BusinessAfter           BusinessSQLSnapshot `json:"business_after"`
	RootBefore              RootLedgerSnapshot  `json:"root_before"`
	RootAfter               RootLedgerSnapshot  `json:"root_after"`
	SourceObservationSHA256 string              `json:"source_observation_sha256"`
	ReplayObservationSHA256 string              `json:"replay_observation_sha256"`
}

// RedactedVerifierManifest retains the complete verifier verdict and its
// cryptographic bindings without retaining ciphertext or Parquet payloads.
type RedactedVerifierManifest struct {
	VerifierVersion           string `json:"verifier_version"`
	QueryIDHash               string `json:"query_id_hash"`
	ResultIDHash              string `json:"result_id_hash"`
	RootTaskIDHash            string `json:"root_task_id_hash"`
	ReceiptSHA256             string `json:"receipt_sha256"`
	ObservationSHA256         string `json:"observation_sha256"`
	ReleaseSetSHA256          string `json:"release_set_sha256"`
	DependencySetSHA256       string `json:"dependency_set_sha256"`
	OutcomeSetSHA256          string `json:"outcome_set_sha256"`
	ArtifactIntentSHA256      string `json:"artifact_intent_sha256"`
	ObjectKeySHA256           string `json:"object_key_sha256"`
	CanonicalCiphertextSHA256 string `json:"canonical_ciphertext_sha256"`
	CanonicalCiphertextSize   int64  `json:"canonical_ciphertext_size"`
	ReleasedParquetSHA256     string `json:"released_parquet_sha256"`
	ReleasedParquetSize       int64  `json:"released_parquet_size"`
	SchemaSHA256              string `json:"schema_sha256"`
	TerminalAuditSequence     int64  `json:"terminal_audit_sequence"`
	RegistrationAuditSequence int64  `json:"registration_audit_sequence"`
	AvailabilityAuditSequence int64  `json:"availability_audit_sequence"`
	VerificationResult        string `json:"verification_result"`
}

// CrossBindingVerificationEvidence records a second approved root executing
// the same logical query. Every identity is salted/redacted in the JSONL.
type CrossBindingVerificationEvidence struct {
	FirstTaskIDHash                string                    `json:"first_task_id_hash"`
	SecondTaskIDHash               string                    `json:"second_task_id_hash"`
	FirstRootTaskIDHash            string                    `json:"first_root_task_id_hash"`
	SecondRootTaskIDHash           string                    `json:"second_root_task_id_hash"`
	FirstQueryIDHash               string                    `json:"first_query_id_hash"`
	SecondQueryIDHash              string                    `json:"second_query_id_hash"`
	FirstGrantSHA256               string                    `json:"first_grant_sha256"`
	SecondGrantSHA256              string                    `json:"second_grant_sha256"`
	FirstCacheKeySHA256            string                    `json:"first_cache_key_sha256"`
	SecondCacheKeySHA256           string                    `json:"second_cache_key_sha256"`
	FirstSQLFingerprintSHA256      string                    `json:"first_sql_fingerprint_sha256"`
	SecondSQLFingerprintSHA256     string                    `json:"second_sql_fingerprint_sha256"`
	FirstCatalogSHA256             string                    `json:"first_catalog_sha256"`
	SecondCatalogSHA256            string                    `json:"second_catalog_sha256"`
	FirstSchemaSHA256              string                    `json:"first_schema_sha256"`
	SecondSchemaSHA256             string                    `json:"second_schema_sha256"`
	FirstDatasourceIDHash          string                    `json:"first_datasource_id_hash"`
	SecondDatasourceIDHash         string                    `json:"second_datasource_id_hash"`
	FirstObservationSHA256         string                    `json:"first_observation_sha256"`
	SecondObservationSHA256        string                    `json:"second_observation_sha256"`
	FirstObservationBindingSHA256  string                    `json:"first_observation_binding_sha256"`
	SecondObservationBindingSHA256 string                    `json:"second_observation_binding_sha256"`
	FirstSourceQueryIDHash         string                    `json:"first_source_query_id_hash"`
	SecondSourceQueryIDHash        string                    `json:"second_source_query_id_hash"`
	SecondRootFirstQueryIDHash     string                    `json:"second_root_first_query_id_hash"`
	BusinessBefore                 BusinessSQLSnapshot       `json:"business_before"`
	BusinessAfter                  BusinessSQLSnapshot       `json:"business_after"`
	SemanticReplayAudits           int64                     `json:"semantic_replay_audits"`
	SettlementAudits               int64                     `json:"settlement_audits"`
	SemanticReplay                 bool                      `json:"semantic_replay"`
	IdempotentReplay               bool                      `json:"idempotent_replay"`
	VerifierManifest               *RedactedVerifierManifest `json:"verifier_manifest"`
}

// TerminalIdentitySnapshot is the redacted identity of one terminal result.
type TerminalIdentitySnapshot struct {
	Found                     bool   `json:"found"`
	QueryIDHash               string `json:"query_id_hash"`
	ResultIDHash              string `json:"result_id_hash"`
	ReceiptSHA256             string `json:"receipt_sha256"`
	IntentSHA256              string `json:"intent_sha256"`
	ObjectKeySHA256           string `json:"object_key_sha256"`
	CommittedObjectSHA256     string `json:"committed_object_sha256"`
	CanonicalCiphertextSHA256 string `json:"canonical_ciphertext_sha256"`
	CanonicalCiphertextSize   int64  `json:"canonical_ciphertext_size"`
	ArtifactStatus            string `json:"artifact_status"`
	ObservationSHA256         string `json:"observation_sha256"`
}

// IdempotentControlSnapshot captures all Business, Control, root, audit, and
// canonical-object state that must remain unchanged on request-ID replay.
type IdempotentControlSnapshot struct {
	Business           BusinessSQLSnapshot      `json:"business"`
	Root               RootLedgerSnapshot       `json:"root"`
	QueryRecords       int64                    `json:"query_records"`
	ExposureCharges    int64                    `json:"exposure_charges"`
	Observations       int64                    `json:"observations"`
	Receipts           int64                    `json:"receipts"`
	Artifacts          int64                    `json:"artifacts"`
	AvailableArtifacts int64                    `json:"available_artifacts"`
	TerminalAudits     int64                    `json:"terminal_audits"`
	RegistrationAudits int64                    `json:"registration_audits"`
	AvailabilityAudits int64                    `json:"availability_audits"`
	CanonicalObjects   int64                    `json:"canonical_objects"`
	Target             TerminalIdentitySnapshot `json:"target"`
}

type IdempotentVerificationEvidence struct {
	Before   IdempotentControlSnapshot `json:"before"`
	After    IdempotentControlSnapshot `json:"after"`
	Returned TerminalIdentitySnapshot  `json:"returned"`
}

type CanonicalObjectSnapshot struct {
	Exists                    bool   `json:"exists"`
	ObjectKeySHA256           string `json:"object_key_sha256"`
	CanonicalCiphertextSHA256 string `json:"canonical_ciphertext_sha256"`
	CanonicalCiphertextSize   int64  `json:"canonical_ciphertext_size"`
	IntentSHA256              string `json:"intent_sha256"`
}

// RecoveryExposureSnapshot is the redacted projection of signed exposure
// evidence retained at the failure and post-recovery boundaries.
type RecoveryExposureSnapshot struct {
	RootTaskIDHash            string `json:"root_task_id_hash"`
	ProfileVersion            string `json:"profile_version"`
	ActualReleaseFacts        int64  `json:"actual_release_facts"`
	ActualInfluenceFacts      int64  `json:"actual_influence_facts"`
	ActualOutcomeFacts        int64  `json:"actual_outcome_facts"`
	ChargedReleaseFacts       int64  `json:"charged_release_facts"`
	ChargedInfluenceFacts     int64  `json:"charged_influence_facts"`
	ChargedOutcomeFacts       int64  `json:"charged_outcome_facts"`
	ObservationSHA256         string `json:"observation_sha256"`
	DictionarySetSHA256       string `json:"dictionary_set_sha256"`
	ReleaseSetSHA256          string `json:"release_set_sha256"`
	InfluenceSetSHA256        string `json:"influence_set_sha256"`
	OutcomeSetSHA256          string `json:"outcome_set_sha256"`
	RootEpoch                 int64  `json:"root_epoch"`
	PredicateProfileVersion   string `json:"predicate_profile_version"`
	PredicateContextSHA256    string `json:"predicate_context_sha256"`
	PredicateSetSHA256        string `json:"predicate_set_sha256"`
	ActualPredicateAtomCount  int64  `json:"actual_predicate_atom_count"`
	ChargedPredicateAtomCount int64  `json:"charged_predicate_atom_count"`
	CompositeOutcomeSHA256    string `json:"composite_outcome_sha256"`
	ActualCompositeCount      int64  `json:"actual_composite_count"`
	ChargedCompositeCount     int64  `json:"charged_composite_count"`
}

// RecoveryVerificationEvidence contains raw counters captured on both sides
// of a forced canonical-exists-but-PENDING recovery. The finalizer recomputes
// the no-requery/no-resettlement assertions instead of trusting booleans.
type RecoveryVerificationEvidence struct {
	FailureObserved                    bool                     `json:"failure_observed"`
	CanonicalObjectObserved            bool                     `json:"canonical_object_observed"`
	ArtifactStatusBefore               string                   `json:"artifact_status_before"`
	ArtifactStatusAfter                string                   `json:"artifact_status_after"`
	BusinessCallsBefore                int64                    `json:"business_calls_before"`
	BusinessCallsAtFailure             int64                    `json:"business_calls_at_failure"`
	BusinessCallsAfter                 int64                    `json:"business_calls_after"`
	QueryRecordsBefore                 int64                    `json:"query_records_before"`
	QueryRecordsAtFailure              int64                    `json:"query_records_at_failure"`
	QueryRecordsAfter                  int64                    `json:"query_records_after"`
	SettlementsAtFailure               int64                    `json:"settlements_at_failure"`
	SettlementsAfter                   int64                    `json:"settlements_after"`
	UsedQueriesBefore                  int64                    `json:"used_queries_before"`
	UsedQueriesAtFailure               int64                    `json:"used_queries_at_failure"`
	UsedQueriesAfter                   int64                    `json:"used_queries_after"`
	ReceiptSHA256AtFailure             string                   `json:"receipt_sha256_at_failure"`
	ReceiptSHA256After                 string                   `json:"receipt_sha256_after"`
	IntentSHA256AtFailure              string                   `json:"intent_sha256_at_failure"`
	IntentSHA256After                  string                   `json:"intent_sha256_after"`
	BusinessBeforeSnapshot             BusinessSQLSnapshot      `json:"business_before_snapshot"`
	BusinessAtFailureSnapshot          BusinessSQLSnapshot      `json:"business_at_failure_snapshot"`
	BusinessAfterSnapshot              BusinessSQLSnapshot      `json:"business_after_snapshot"`
	RootAtFailure                      RootLedgerSnapshot       `json:"root_at_failure"`
	RootAfter                          RootLedgerSnapshot       `json:"root_after"`
	ExposureAtFailure                  RecoveryExposureSnapshot `json:"exposure_at_failure"`
	ExposureAfter                      RecoveryExposureSnapshot `json:"exposure_after"`
	ObjectAtFailure                    CanonicalObjectSnapshot  `json:"object_at_failure"`
	ObjectAfter                        CanonicalObjectSnapshot  `json:"object_after"`
	SettlementAuditSequencesAtFailure  []int64                  `json:"settlement_audit_sequences_at_failure"`
	SettlementAuditSequencesAfter      []int64                  `json:"settlement_audit_sequences_after"`
	TerminalAuditsAtFailure            int64                    `json:"terminal_audits_at_failure"`
	TerminalAuditsAfter                int64                    `json:"terminal_audits_after"`
	RegistrationAuditsAtFailure        int64                    `json:"registration_audits_at_failure"`
	RegistrationAuditsAfter            int64                    `json:"registration_audits_after"`
	AvailabilityAuditsAtFailure        int64                    `json:"availability_audits_at_failure"`
	AvailabilityAuditsAfter            int64                    `json:"availability_audits_after"`
	TerminalAuditSequenceAtFailure     int64                    `json:"terminal_audit_sequence_at_failure"`
	TerminalAuditSequenceAfter         int64                    `json:"terminal_audit_sequence_after"`
	RegistrationAuditSequenceAtFailure int64                    `json:"registration_audit_sequence_at_failure"`
	RegistrationAuditSequenceAfter     int64                    `json:"registration_audit_sequence_after"`
	AvailabilityAuditSequenceAtFailure int64                    `json:"availability_audit_sequence_at_failure"`
	AvailabilityAuditSequenceAfter     int64                    `json:"availability_audit_sequence_after"`
}

type RLSVerificationEvidence struct {
	Version                       string                      `json:"version"`
	CorpusID                      string                      `json:"corpus_id"`
	CorpusSHA256                  string                      `json:"corpus_sha256"`
	TraceSHA256                   string                      `json:"trace_sha256"`
	DatasetID                     string                      `json:"dataset_id"`
	DatasetSHA256                 string                      `json:"dataset_sha256"`
	PolicySeed                    int64                       `json:"policy_seed"`
	Product                       string                      `json:"product,omitempty"`
	BudgetProfile                 string                      `json:"budget_profile,omitempty"`
	RootTaskIDHash                string                      `json:"root_task_id_hash,omitempty"`
	PolicySchema                  string                      `json:"policy_schema"`
	PolicyTable                   string                      `json:"policy_table"`
	PolicyName                    string                      `json:"policy_name"`
	RelRowSecurity                bool                        `json:"relrowsecurity"`
	RelForceRowSecurity           bool                        `json:"relforcerowsecurity"`
	SessionUser                   string                      `json:"session_user"`
	CurrentRole                   string                      `json:"current_role"`
	BaselineRole                  string                      `json:"baseline_role"`
	TableOwnerRole                string                      `json:"table_owner_role"`
	BaselineRoleIsOwner           bool                        `json:"baseline_role_is_owner"`
	BaselineRoleCanLogin          bool                        `json:"baseline_role_canlogin"`
	BaselineRoleSuperuser         bool                        `json:"baseline_role_superuser"`
	BaselineRoleInherit           bool                        `json:"baseline_role_inherit"`
	BaselineRoleCreateDB          bool                        `json:"baseline_role_createdb"`
	BaselineRoleCreateRole        bool                        `json:"baseline_role_createrole"`
	BaselineRoleReplication       bool                        `json:"baseline_role_replication"`
	BaselineRoleBypassRLS         bool                        `json:"baseline_role_bypassrls"`
	PoliciesJSON                  json.RawMessage             `json:"policies_json"`
	PoliciesSHA256                string                      `json:"policies_sha256"`
	MembershipsJSON               json.RawMessage             `json:"memberships_json"`
	MembershipsSHA256             string                      `json:"memberships_sha256"`
	GrantsJSON                    json.RawMessage             `json:"grants_json"`
	GrantsSHA256                  string                      `json:"grants_sha256"`
	OracleComputedBefore          bool                        `json:"oracle_computed_before_bounded"`
	OracleTrace                   []finalv5oracle.Observation `json:"oracle_trace"`
	OracleResult                  finalv5oracle.TraceUnion    `json:"oracle_result"`
	OraclePrefixes                []finalv5oracle.TraceUnion  `json:"oracle_prefixes"`
	Steps                         []RLSStepEvidence           `json:"steps"`
	SuccessfulQueries             int                         `json:"successful_queries"`
	FirstRejectionIndex           int                         `json:"first_rejection_index,omitempty"`
	UnrelatedAuthorizationDenials int                         `json:"unrelated_authorization_denials"`
	ResultsAfterBudget            int                         `json:"results_after_budget"`
	StopReason                    string                      `json:"stop_reason,omitempty"`
	FinalRoot                     *RootLedgerSnapshot         `json:"final_root,omitempty"`
	NegativeControl               *RLSNegativeEvidence        `json:"negative_control,omitempty"`
}

// RLSStepEvidence binds one frozen query to the independent oracle prefix and,
// for TaskGate, to the signed V8 artifact and exact production root state.
type RLSStepEvidence struct {
	Index                  int                           `json:"index"`
	StepID                 string                        `json:"step_id"`
	Family                 string                        `json:"family"`
	Variant                string                        `json:"variant"`
	LogicalSQLSHA256       string                        `json:"logical_sql_sha256"`
	DirectSQLSHA256        string                        `json:"direct_sql_sha256"`
	ExpectedResultSHA256   string                        `json:"expected_result_sha256"`
	ObservedResultSHA256   string                        `json:"observed_result_sha256,omitempty"`
	VerifiedResultSHA256   string                        `json:"verified_result_sha256,omitempty"`
	RowCount               int64                         `json:"row_count"`
	ColumnCount            int                           `json:"column_count"`
	ScalarInt64            *int64                        `json:"scalar_int64,omitempty"`
	DecisionPreviousStep   int                           `json:"decision_previous_step,omitempty"`
	DecisionPreviousValue  int64                         `json:"decision_previous_value,omitempty"`
	DecisionRule           string                        `json:"decision_rule,omitempty"`
	DecisionThreshold      int64                         `json:"decision_threshold,omitempty"`
	Accepted               bool                          `json:"accepted"`
	Rejected               bool                          `json:"rejected"`
	ObservedErrorCode      string                        `json:"observed_error_code,omitempty"`
	ObservedErrorReason    string                        `json:"observed_error_reason,omitempty"`
	RequestIDHash          string                        `json:"request_id_hash,omitempty"`
	QueryIDHash            string                        `json:"query_id_hash,omitempty"`
	ResultIDHash           string                        `json:"result_id_hash,omitempty"`
	PlanSHA256             string                        `json:"plan_sha256,omitempty"`
	ObservationSHA256      string                        `json:"observation_sha256,omitempty"`
	ReleaseSetSHA256       string                        `json:"release_set_sha256,omitempty"`
	DependencySetSHA256    string                        `json:"dependency_set_sha256,omitempty"`
	OutcomeSetSHA256       string                        `json:"outcome_set_sha256,omitempty"`
	ActualReleaseFacts     int64                         `json:"actual_release_facts"`
	ChargedReleaseFacts    int64                         `json:"charged_release_facts"`
	ActualDependencyFacts  int64                         `json:"actual_dependency_facts"`
	ChargedDependencyFacts int64                         `json:"charged_dependency_facts"`
	ActualOutcomeFacts     int64                         `json:"actual_outcome_facts"`
	ChargedOutcomeFacts    int64                         `json:"charged_outcome_facts"`
	PredicateAtomCount     int64                         `json:"predicate_atom_count"`
	CompositeCount         int64                         `json:"composite_count"`
	SemanticReplay         bool                          `json:"semantic_replay"`
	IdempotentReplay       bool                          `json:"idempotent_replay"`
	RootTaskIDHash         string                        `json:"root_task_id_hash,omitempty"`
	Before                 *RLSControlSnapshot           `json:"before,omitempty"`
	After                  *RLSControlSnapshot           `json:"after,omitempty"`
	OraclePrefix           finalv5oracle.TraceUnion      `json:"oracle_prefix"`
	ArtifactSHA256         string                        `json:"artifact_sha256,omitempty"`
	ObjectSHA256           string                        `json:"object_sha256,omitempty"`
	ParquetBytes           int64                         `json:"parquet_bytes"`
	EncryptedObjectBytes   int64                         `json:"encrypted_object_bytes"`
	ReceiptVersion         string                        `json:"receipt_version,omitempty"`
	ReceiptSHA256          string                        `json:"receipt_sha256,omitempty"`
	ArtifactIntentSHA256   string                        `json:"artifact_intent_sha256,omitempty"`
	AvailabilitySHA256     string                        `json:"availability_audit_sha256,omitempty"`
	Verification           *BaselineVerificationEvidence `json:"verification,omitempty"`
	RejectedQuery          *AttackRejectedQueryEvidence  `json:"rejected_query,omitempty"`
}

type RLSNegativeEvidence struct {
	TargetReceiptNo                  string                       `json:"target_receipt_no"`
	TargetDepartment                 string                       `json:"target_department"`
	PolicyDepartment                 string                       `json:"policy_department"`
	TargetPresentOutsidePolicy       bool                         `json:"target_present_outside_policy"`
	PolicyFiltered                   bool                         `json:"policy_filtered"`
	ExpectedRowCount                 int64                        `json:"expected_row_count"`
	ObservedRowCount                 int64                        `json:"observed_row_count"`
	ExpectedResultSHA256             string                       `json:"expected_result_sha256"`
	ObservedResultSHA256             string                       `json:"observed_result_sha256"`
	AuthorizationSQLSHA256           string                       `json:"authorization_sql_sha256"`
	ExpectedAuthorizationErrorCode   string                       `json:"expected_authorization_error_code"`
	ObservedAuthorizationErrorCode   string                       `json:"observed_authorization_error_code"`
	ObservedAuthorizationErrorReason string                       `json:"observed_authorization_error_reason"`
	AuthorizationRejectedNoRows      bool                         `json:"authorization_rejected_no_rows"`
	Before                           *RLSControlSnapshot          `json:"before,omitempty"`
	After                            *RLSControlSnapshot          `json:"after,omitempty"`
	RejectedQuery                    *AttackRejectedQueryEvidence `json:"rejected_query,omitempty"`
}

type RLSControlSnapshot struct {
	TaskIDHash         string              `json:"task_id_hash"`
	RootTaskIDHash     string              `json:"root_task_id_hash"`
	Product            string              `json:"product"`
	BudgetProfile      string              `json:"budget_profile"`
	MaxQueries         int64               `json:"max_queries"`
	MaxRows            int64               `json:"max_rows"`
	UsedQueries        int64               `json:"used_queries"`
	UsedRows           int64               `json:"used_rows"`
	ReservedQueries    int64               `json:"reserved_queries"`
	ReservedRows       int64               `json:"reserved_rows"`
	MaxReleaseFacts    int64               `json:"max_release_facts"`
	MaxDependencyFacts int64               `json:"max_dependency_facts"`
	MaxOutcomeFacts    int64               `json:"max_outcome_facts"`
	Business           BusinessSQLSnapshot `json:"business"`
	Root               RootLedgerSnapshot  `json:"root"`
	QueryRecords       int64               `json:"query_records"`
	Settlements        int64               `json:"settlements"`
	Observations       int64               `json:"observations"`
	Receipts           int64               `json:"receipts"`
	Artifacts          int64               `json:"artifacts"`
	AvailableArtifacts int64               `json:"available_artifacts"`
	SuccessfulAudits   int64               `json:"successful_audits"`
	FailureAudits      int64               `json:"failure_audits"`
	CanonicalObjects   int64               `json:"canonical_objects"`
}

type AttackVerificationEvidence struct {
	Version                  string               `json:"version"`
	CorpusID                 string               `json:"corpus_id"`
	CorpusSHA256             string               `json:"corpus_sha256"`
	DatasetID                string               `json:"dataset_id"`
	Product                  string               `json:"product"`
	BudgetProfile            string               `json:"budget_profile,omitempty"`
	RootTaskIDHash           string               `json:"root_task_id_hash,omitempty"`
	Steps                    []AttackStepEvidence `json:"steps"`
	PrimaryResultSHA256      string               `json:"primary_result_sha256"`
	CompleteRowSetSHA256     string               `json:"complete_row_set_sha256,omitempty"`
	DecomposedRowSetSHA256   string               `json:"decomposed_row_set_sha256,omitempty"`
	NormalFormSHA256         []string             `json:"normal_form_sha256,omitempty"`
	ExpectedThresholds       []int64              `json:"expected_thresholds,omitempty"`
	ObservedThresholdResults []int64              `json:"observed_threshold_results,omitempty"`
	OutcomeCeiling           int64                `json:"outcome_ceiling,omitempty"`
	ObservedOutcome          int64                `json:"observed_outcome,omitempty"`
	ThresholdRejectionIndex  int                  `json:"threshold_rejection_index,omitempty"`
	FinalRoot                *RootLedgerSnapshot  `json:"final_root,omitempty"`
	AnchorRequestIDHash      string               `json:"anchor_request_id_hash,omitempty"`
	AnchorQueryIDHash        string               `json:"anchor_query_id_hash,omitempty"`
	AnchorResultIDHash       string               `json:"anchor_result_id_hash,omitempty"`
}

// AttackStepEvidence binds a single corpus step either to an independently
// verified released artifact or to a fail-closed structured rejection. Raw
// TaskGate, query, result, and object identities are never retained.
type AttackStepEvidence struct {
	Index                  int                           `json:"index"`
	VariantID              string                        `json:"variant_id"`
	Classification         string                        `json:"classification"`
	Role                   string                        `json:"role"`
	LogicalSQLSHA256       string                        `json:"logical_sql_sha256"`
	DirectSQLSHA256        string                        `json:"direct_sql_sha256"`
	Accepted               bool                          `json:"accepted"`
	Rejected               bool                          `json:"rejected"`
	ObservedErrorCode      string                        `json:"observed_error_code,omitempty"`
	ObservedErrorReason    string                        `json:"observed_error_reason,omitempty"`
	TraceIDHash            string                        `json:"trace_id_hash,omitempty"`
	RequestIDHash          string                        `json:"request_id_hash,omitempty"`
	QueryIDHash            string                        `json:"query_id_hash,omitempty"`
	ResultIDHash           string                        `json:"result_id_hash,omitempty"`
	RowCount               int64                         `json:"row_count"`
	ColumnCount            int                           `json:"column_count"`
	ResultSHA256           string                        `json:"result_sha256,omitempty"`
	ScalarInt64            *int64                        `json:"scalar_int64,omitempty"`
	RowSHA256              []string                      `json:"row_sha256,omitempty"`
	RowSetSHA256           string                        `json:"row_set_sha256,omitempty"`
	PlanSHA256             string                        `json:"plan_sha256,omitempty"`
	ResultMetadataJSON     json.RawMessage               `json:"result_metadata_json,omitempty"`
	ObservationSHA256      string                        `json:"observation_sha256,omitempty"`
	ReleaseSetSHA256       string                        `json:"release_set_sha256,omitempty"`
	DependencySetSHA256    string                        `json:"dependency_set_sha256,omitempty"`
	OutcomeSetSHA256       string                        `json:"outcome_set_sha256,omitempty"`
	ActualReleaseFacts     int64                         `json:"actual_release_facts"`
	ChargedReleaseFacts    int64                         `json:"charged_release_facts"`
	ActualDependencyFacts  int64                         `json:"actual_dependency_facts"`
	ChargedDependencyFacts int64                         `json:"charged_dependency_facts"`
	ActualOutcomeFacts     int64                         `json:"actual_outcome_facts"`
	ChargedOutcomeFacts    int64                         `json:"charged_outcome_facts"`
	PredicateAtomCount     int64                         `json:"predicate_atom_count"`
	CompositeCount         int64                         `json:"composite_count"`
	SemanticReplay         bool                          `json:"semantic_replay"`
	IdempotentReplay       bool                          `json:"idempotent_replay"`
	RootTaskIDHash         string                        `json:"root_task_id_hash,omitempty"`
	RootEpochAfter         int64                         `json:"root_epoch_after"`
	ArtifactSHA256         string                        `json:"artifact_sha256,omitempty"`
	ObjectSHA256           string                        `json:"object_sha256,omitempty"`
	ParquetBytes           int64                         `json:"parquet_bytes"`
	EncryptedObjectBytes   int64                         `json:"encrypted_object_bytes"`
	ReceiptVersion         string                        `json:"receipt_version,omitempty"`
	ReceiptSHA256          string                        `json:"receipt_sha256,omitempty"`
	ArtifactIntentSHA256   string                        `json:"artifact_intent_sha256,omitempty"`
	AvailabilitySHA256     string                        `json:"availability_audit_sha256,omitempty"`
	Before                 *AttackControlSnapshot        `json:"before,omitempty"`
	After                  *AttackControlSnapshot        `json:"after,omitempty"`
	Verification           *BaselineVerificationEvidence `json:"verification,omitempty"`
	RejectedQuery          *AttackRejectedQueryEvidence  `json:"rejected_query,omitempty"`
}

// AttackControlSnapshot is a repeatable-read Control projection plus the
// independently observed Business SQL and canonical-object counters.
type AttackControlSnapshot struct {
	TaskIDHash         string              `json:"task_id_hash"`
	RootTaskIDHash     string              `json:"root_task_id_hash"`
	Product            string              `json:"product"`
	BudgetProfile      string              `json:"budget_profile"`
	MaxQueries         int64               `json:"max_queries"`
	UsedQueries        int64               `json:"used_queries"`
	ReservedQueries    int64               `json:"reserved_queries"`
	MaxOutcomeFacts    int64               `json:"max_outcome_facts"`
	Business           BusinessSQLSnapshot `json:"business"`
	Root               RootLedgerSnapshot  `json:"root"`
	QueryRecords       int64               `json:"query_records"`
	Settlements        int64               `json:"settlements"`
	Observations       int64               `json:"observations"`
	Receipts           int64               `json:"receipts"`
	Artifacts          int64               `json:"artifacts"`
	AvailableArtifacts int64               `json:"available_artifacts"`
	SuccessfulAudits   int64               `json:"successful_audits"`
	FailureAudits      int64               `json:"failure_audits"`
	CanonicalObjects   int64               `json:"canonical_objects"`
}

// AttackRejectedQueryEvidence is a query-specific negative projection. A
// lowering rejection legitimately has Found=false; an exposure rejection must
// retain its failed query/receipt/reservation without a released artifact.
type AttackRejectedQueryEvidence struct {
	Found              bool   `json:"found"`
	QueryIDHash        string `json:"query_id_hash,omitempty"`
	Status             string `json:"status,omitempty"`
	ErrorCode          string `json:"error_code,omitempty"`
	ResultSHA256       string `json:"result_sha256,omitempty"`
	ReservationStatus  string `json:"reservation_status,omitempty"`
	EncryptedResults   int64  `json:"encrypted_results"`
	EncryptedChunks    int64  `json:"encrypted_chunks"`
	Materializations   int64  `json:"materializations"`
	QueryObservations  int64  `json:"query_observations"`
	RootObservations   int64  `json:"root_observations"`
	Artifacts          int64  `json:"artifacts"`
	AvailableArtifacts int64  `json:"available_artifacts"`
	AvailabilityAudits int64  `json:"availability_audits"`
	SuccessfulAudits   int64  `json:"successful_audits"`
	FailureAudits      int64  `json:"failure_audits"`
	Receipts           int64  `json:"receipts"`
}

type ProvSQLVerificationEvidence struct {
	Version                       string                              `json:"version"`
	Boundary                      string                              `json:"boundary"`
	BindingFileSHA256             string                              `json:"binding_file_sha256"`
	BindingSHA256                 string                              `json:"binding_sha256"`
	FixtureVersion                string                              `json:"fixture_version"`
	FixtureSQLSHA256              string                              `json:"fixture_sql_sha256"`
	EnableSQLSHA256               string                              `json:"enable_sql_sha256"`
	DatasetSHA256                 string                              `json:"dataset_sha256"`
	DatasetProbeSQLSHA256         string                              `json:"dataset_probe_sql_sha256"`
	BusinessDatasetProbeSQLSHA256 string                              `json:"business_dataset_probe_sql_sha256"`
	DatasetRows                   int64                               `json:"dataset_rows"`
	ScaleLimit                    int64                               `json:"scale_limit"`
	Nonce                         int64                               `json:"nonce"`
	Warmup                        bool                                `json:"warmup"`
	NonceBindingSHA256            string                              `json:"nonce_binding_sha256"`
	PhysicalSQLSHA256             string                              `json:"physical_sql_sha256"`
	LogicalSQLSHA256              string                              `json:"logical_sql_sha256"`
	CacheConditionSHA256          string                              `json:"cache_condition_sha256"`
	ExecutionOrderSHA256          string                              `json:"execution_order_sha256"`
	ExpectedRows                  int64                               `json:"expected_rows"`
	ExpectedColumns               int                                 `json:"expected_columns"`
	ExpectedResultSHA256          string                              `json:"expected_result_sha256"`
	ObservedResultSHA256          string                              `json:"observed_result_sha256"`
	ExpectedDependencyFacts       int64                               `json:"expected_dependency_facts"`
	ExpectedDependencySHA256      string                              `json:"expected_dependency_sha256"`
	DependencyLink                *ProvSQLDependencySetVerificationV1 `json:"dependency_link,omitempty"`
	TypedDrainFields              int64                               `json:"typed_drain_fields"`
	TypedDrainSHA256              string                              `json:"typed_drain_sha256"`
	FieldOIDs                     []uint32                            `json:"field_oids"`
	PostgreSQLVersion             string                              `json:"postgresql_version"`
	PostgreSQLVersionNum          string                              `json:"postgresql_version_num"`
	StatementTimeoutMS            int64                               `json:"statement_timeout_ms"`
	MaxParallelWorkers            int64                               `json:"max_parallel_workers_per_gather"`
	ClientMinMessages             string                              `json:"client_min_messages"`
	LogMinMessages                string                              `json:"log_min_messages"`
	ProvSQLVersion                string                              `json:"provsql_version,omitempty"`
	ProvSQLCommit                 string                              `json:"provsql_commit,omitempty"`
	SharedPreload                 bool                                `json:"shared_preload,omitempty"`
	AggTokenTextAsUUID            bool                                `json:"agg_token_text_as_uuid,omitempty"`
	AggTokenOID                   uint32                              `json:"agg_token_oid,omitempty"`
	UUIDOID                       uint32                              `json:"uuid_oid,omitempty"`
	CarrierGateType               string                              `json:"carrier_gate_type,omitempty"`
	RowGateType                   string                              `json:"row_gate_type,omitempty"`
	RootTypesVerified             bool                                `json:"root_types_verified,omitempty"`
	AggregateTokens               int64                               `json:"aggregate_tokens,omitempty"`
	RowTokens                     int64                               `json:"row_tokens,omitempty"`
	GatesBefore                   int64                               `json:"gates_before,omitempty"`
	GatesAfter                    int64                               `json:"gates_after,omitempty"`
	ArtifactBytesBefore           int64                               `json:"artifact_bytes_before,omitempty"`
	ArtifactBytesAfter            int64                               `json:"artifact_bytes_after,omitempty"`
	RepresentationSHA256          string                              `json:"representation_sha256,omitempty"`
	// Only the TaskGate arm carries these independent runtime boundaries. The
	// direct PostgreSQL and ProvSQL arms must leave every pointer nil.
	BusinessBefore *BusinessSQLSnapshot `json:"business_before,omitempty"`
	BusinessAfter  *BusinessSQLSnapshot `json:"business_after,omitempty"`
	RootBefore     *RootLedgerSnapshot  `json:"root_before,omitempty"`
	RootAfter      *RootLedgerSnapshot  `json:"root_after,omitempty"`
	ObserverWindow *ObserverWindowV2    `json:"observer_window,omitempty"`
	FailureStage   string               `json:"failure_stage,omitempty"`
}

type CompilerVerificationEvidence struct {
	FixtureVersion                string                     `json:"fixture_version"`
	RegistrySHA256                string                     `json:"registry_sha256"`
	ProductsSHA256                string                     `json:"products_sha256"`
	FixtureSQLSHA256              string                     `json:"fixture_sql_sha256"`
	DatasetSHA256                 string                     `json:"dataset_sha256"`
	ExpectedDepth                 int                        `json:"expected_depth"`
	ObservedDepth                 int                        `json:"observed_depth"`
	ExpectedSources               int                        `json:"expected_sources"`
	ObservedSources               int                        `json:"observed_sources"`
	DirectResultSHA256            string                     `json:"direct_result_sha256"`
	NestedResultSHA256            string                     `json:"nested_result_sha256"`
	Artifacts                     []CompilerArtifactEvidence `json:"artifacts"`
	StructuredErrorCode           string                     `json:"structured_error_code"`
	StructuredErrorRelationSHA256 string                     `json:"structured_error_relation_sha256"`
	AllocationErrorCode           string                     `json:"allocation_error_code"`
}

type CompilerArtifactEvidence struct {
	Name                string `json:"name"`
	ArtifactSHA256      string `json:"artifact_sha256"`
	DefinitionSHA256    string `json:"definition_sha256"`
	DependencySHA256    string `json:"dependency_sha256"`
	InterfaceSHA256     string `json:"interface_sha256"`
	CanonicalPlanSHA256 string `json:"canonical_plan_sha256"`
	BindingSHA256       string `json:"binding_sha256"`
	OutputsSHA256       string `json:"outputs_sha256"`
	BaseProductsSHA256  string `json:"base_products_sha256"`
	ReachableRelations  int    `json:"reachable_relations"`
	DependencyEdges     int    `json:"dependency_edges"`
	ExpandedSources     int    `json:"expanded_sources"`
	DefinitionBytes     int    `json:"definition_bytes"`
	CanonicalPlanBytes  int    `json:"canonical_plan_bytes"`
}

type ConcurrencyVerification struct {
	Version                     string                         `json:"version"`
	FixtureSHA256               string                         `json:"fixture_sha256"`
	PlansSHA256                 string                         `json:"plans_sha256"`
	ProbeVersion                string                         `json:"probe_version"`
	GatewayInstanceSHA256       string                         `json:"gateway_instance_sha256"`
	RoundSHA256                 string                         `json:"round_sha256"`
	RootTaskIDHash              string                         `json:"root_task_id_hash"`
	ContenderRequestSetSHA256   string                         `json:"contender_request_set_sha256"`
	ExpectedWidth               int64                          `json:"expected_width"`
	HTTPActiveCapacity          int64                          `json:"http_active_capacity"`
	HTTPQueueCapacity           int64                          `json:"http_queue_capacity"`
	ControlPoolCapacity         int64                          `json:"control_pool_capacity"`
	ConnectorPoolCapacity       int64                          `json:"connector_pool_capacity"`
	ServiceArrivals             int64                          `json:"service_arrivals"`
	ServiceUniqueParticipants   int64                          `json:"service_unique_participants"`
	ServiceParticipantSetSHA256 string                         `json:"service_participant_set_sha256"`
	ServicePeakBarrierWaiting   int64                          `json:"service_peak_barrier_waiting"`
	ServicePeakActive           int64                          `json:"service_peak_active"`
	ServicePeakQueued           int64                          `json:"service_peak_queued"`
	ServiceCompleted            int64                          `json:"service_completed"`
	ServiceCanceled             int64                          `json:"service_canceled"`
	ServiceRejected             int64                          `json:"service_rejected"`
	PeakControlPoolInUse        int64                          `json:"peak_control_pool_in_use"`
	ControlPoolWaitCountDelta   int64                          `json:"control_pool_wait_count_delta"`
	ControlPoolWaitNanoseconds  int64                          `json:"control_pool_wait_nanoseconds"`
	RootLockWaitersObserved     int64                          `json:"root_lock_waiters_observed"`
	ProductionCASAttempts       int64                          `json:"production_cas_attempts"`
	ProductionCASConflicts      int64                          `json:"production_cas_conflicts"`
	ProductionCASRetries        int64                          `json:"production_cas_retries"`
	NaturalCASAttempts          int64                          `json:"natural_cas_attempts"`
	NaturalCASConflicts         int64                          `json:"natural_cas_conflicts"`
	NaturalCASRetries           int64                          `json:"natural_cas_retries"`
	InitialRoot                 RootLedgerSnapshot             `json:"initial_root"`
	BeforeBoundary              RootLedgerSnapshot             `json:"before_boundary"`
	AtBoundary                  RootLedgerSnapshot             `json:"at_boundary"`
	AfterRejectedOverflow       RootLedgerSnapshot             `json:"after_rejected_overflow"`
	ResourceBudgetProfile       string                         `json:"resource_budget_profile"`
	ResourceMaxQueries          int64                          `json:"resource_max_queries"`
	BudgetLimit                 int64                          `json:"budget_limit"`
	UsageBefore                 int64                          `json:"usage_before"`
	Accepted                    int64                          `json:"accepted"`
	Rejected                    int64                          `json:"rejected"`
	UsageAfter                  int64                          `json:"usage_after"`
	ChargedWinners              int64                          `json:"charged_winners"`
	ZeroNoveltySettlements      int64                          `json:"zero_novelty_settlements"`
	ExpectedResultSHA256        string                         `json:"expected_result_sha256"`
	FinalRootFactHashes         []string                       `json:"final_root_fact_hashes"`
	FinalRootSetSHA256          string                         `json:"final_root_set_sha256"`
	Contenders                  []ConcurrencyContenderEvidence `json:"contenders"`
	Overflow                    ConcurrencyOverflowEvidence    `json:"overflow"`
}

// ConcurrencyContenderEvidence is deliberately compact but complete: every
// contender is bound to one delegated child in the frozen root family, its
// unique request, signed V8 receipt,
// canonical result, artifact intent, availability event, and composite
// verifier transcript. It contains only salted identities and digests.
type ConcurrencyContenderEvidence struct {
	Index                   int                       `json:"index"`
	ParticipantSHA256       string                    `json:"participant_sha256"`
	TaskIDHash              string                    `json:"task_id_hash"`
	RootTaskIDHash          string                    `json:"root_task_id_hash"`
	RequestIDHash           string                    `json:"request_id_hash"`
	QueryIDHash             string                    `json:"query_id_hash"`
	ResultIDHash            string                    `json:"result_id_hash"`
	ResultSHA256            string                    `json:"result_sha256"`
	ObservationSHA256       string                    `json:"observation_sha256"`
	CompositeOutcomeSHA256  string                    `json:"composite_outcome_sha256"`
	PredicateSetSHA256      string                    `json:"predicate_set_sha256"`
	RootEpoch               int64                     `json:"root_epoch"`
	ActualOutcomeFacts      int64                     `json:"actual_outcome_facts"`
	ChargedOutcomeFacts     int64                     `json:"charged_outcome_facts"`
	CASAttempts             int64                     `json:"cas_attempts"`
	CASConflicts            int64                     `json:"cas_conflicts"`
	CASRetries              int64                     `json:"cas_retries"`
	ReceiptVersion          string                    `json:"receipt_version"`
	ReceiptSHA256           string                    `json:"receipt_sha256"`
	ArtifactIntentSHA256    string                    `json:"artifact_intent_sha256"`
	AvailabilityAuditSHA256 string                    `json:"availability_audit_sha256"`
	ReceiptVerified         bool                      `json:"receipt_verified"`
	ArtifactAvailable       bool                      `json:"artifact_available"`
	VerifierManifest        *RedactedVerifierManifest `json:"verifier_manifest"`
}

// ConcurrencyOverflowEvidence is the exact failure-only Control projection
// for the one B+1 request. A signed terminal failure receipt is retained, but
// no result, artifact, observation, successful audit, or root mutation is.
type ConcurrencyOverflowEvidence struct {
	Attempts           int64  `json:"attempts"`
	Rejected           int64  `json:"rejected"`
	ErrorCode          string `json:"error_code"`
	Found              bool   `json:"found"`
	QueryIDHash        string `json:"query_id_hash"`
	Status             string `json:"status"`
	ReservationStatus  string `json:"reservation_status"`
	ResultSHA256       string `json:"result_sha256"`
	EncryptedResults   int64  `json:"encrypted_results"`
	EncryptedChunks    int64  `json:"encrypted_chunks"`
	Materializations   int64  `json:"materializations"`
	QueryObservations  int64  `json:"query_observations"`
	RootObservations   int64  `json:"root_observations"`
	Artifacts          int64  `json:"artifacts"`
	AvailableArtifacts int64  `json:"available_artifacts"`
	AvailabilityAudits int64  `json:"availability_audits"`
	SuccessfulAudits   int64  `json:"successful_audits"`
	ReleaseAudits      int64  `json:"release_audits"`
	FailureAudits      int64  `json:"failure_audits"`
	Receipts           int64  `json:"receipts"`
}

// RQ5VerificationEvidence binds one of the four measured daily cycles to an
// independently built 345,000-row publication and to a real sequential
// Gateway lifecycle. Both paired modes retain this complete cycle evidence;
// build_verify_activate selects NewInitial as its measured query, while
// retained_route selects NewRestored after the old-Catalog check and restore.
type RQ5VerificationEvidence struct {
	Version               string                   `json:"version"`
	FixtureSHA256         string                   `json:"fixture_sha256"`
	BuildManifestSHA256   string                   `json:"build_manifest_sha256"`
	PhaseImageID          string                   `json:"phase_image_id"`
	OnlineImageID         string                   `json:"online_image_id"`
	OAImageID             string                   `json:"oa_image_id"`
	PhaseBinarySHA256     string                   `json:"phase_binary_sha256"`
	OnlineBinarySHA256    string                   `json:"online_binary_sha256"`
	OABinarySHA256        string                   `json:"oa_binary_sha256"`
	PhaseBinaryMTimeUnix  int64                    `json:"phase_binary_mtime_unix"`
	OnlineBinaryMTimeUnix int64                    `json:"online_binary_mtime_unix"`
	OABinaryMTimeUnix     int64                    `json:"oa_binary_mtime_unix"`
	DatasetManifestSHA256 string                   `json:"dataset_manifest_sha256"`
	GeneratorSHA256       string                   `json:"generator_sha256"`
	ConfigSHA256          string                   `json:"config_sha256"`
	RowsPerPublication    int64                    `json:"rows_per_publication"`
	CycleIndex            int                      `json:"cycle_index"`
	FromDay               string                   `json:"from_day"`
	ToDay                 string                   `json:"to_day"`
	PublicationSetSHA256  string                   `json:"publication_set_sha256"`
	Publications          []RQ5PublicationEvidence `json:"publications"`
	Build                 RQ5BuildEvidence         `json:"build"`
	Topology              RQ5TopologyEvidence      `json:"topology"`
	LifecycleSHA256       string                   `json:"lifecycle_sha256"`
	Lifecycle             []RQ5LifecycleStep       `json:"lifecycle"`
	Route                 RQ5RouteEvidence         `json:"route"`
}

type RQ5PublicationEvidence struct {
	Index                     int    `json:"index"`
	Day                       string `json:"day"`
	PublicationName           string `json:"publication_name"`
	RowCount                  int64  `json:"row_count"`
	ApprovedInputSHA256       string `json:"approved_input_sha256"`
	CatalogSHA256             string `json:"catalog_sha256"`
	BundleManifestSHA256      string `json:"bundle_manifest_sha256"`
	PublicationManifestSHA256 string `json:"publication_manifest_sha256"`
	DictionarySHA256          string `json:"dictionary_sha256"`
	SidecarSHA256             string `json:"sidecar_sha256"`
	SchemaSHA256              string `json:"schema_sha256"`
	HOTArtifactSHA256         string `json:"hot_artifact_sha256"`
	ColdArtifactSHA256        string `json:"cold_artifact_sha256"`
	SidecarArtifactSHA256     string `json:"sidecar_artifact_sha256"`
	DirectResultSHA256        string `json:"direct_result_sha256"`
	ArtifactBytes             int64  `json:"artifact_bytes"`
	HOTArtifactBytes          int64  `json:"hot_artifact_bytes"`
}

type RQ5BuildEvidence struct {
	Day                       string           `json:"day"`
	RowCount                  int64            `json:"row_count"`
	CycleWallMS               float64          `json:"cycle_wall_ms"`
	ArtifactBytes             int64            `json:"artifact_bytes"`
	HOTArtifactBytes          int64            `json:"hot_artifact_bytes"`
	PublicationManifestSHA256 string           `json:"publication_manifest_sha256"`
	DictionarySHA256          string           `json:"dictionary_sha256"`
	VerificationReceiptSHA256 string           `json:"verification_receipt_sha256"`
	Build                     RQ5PhaseEvidence `json:"build"`
	StrictVerify              RQ5PhaseEvidence `json:"strict_verify"`
	Activation                RQ5PhaseEvidence `json:"activation"`
}

type RQ5PhaseEvidence struct {
	Phase               string  `json:"phase"`
	Status              string  `json:"status"`
	WallMS              float64 `json:"wall_ms"`
	PeakRSSBytes        int64   `json:"peak_rss_bytes"`
	ExecutableSHA256    string  `json:"executable_sha256"`
	ArgvSHA256          string  `json:"argv_sha256"`
	StdoutSHA256        string  `json:"stdout_sha256"`
	CommandReportSHA256 string  `json:"command_report_sha256"`
}

type RQ5TopologyEvidence struct {
	Model                     string `json:"model"`
	Disclosure                string `json:"disclosure"`
	SingleServiceSlot         bool   `json:"single_service_slot"`
	RequestRouterPresent      bool   `json:"request_router_present"`
	MaxConcurrentServices     int64  `json:"max_concurrent_services"`
	ServiceStarts             int64  `json:"service_starts"`
	ServiceStops              int64  `json:"service_stops"`
	FinalActiveServices       int64  `json:"final_active_services"`
	HOTArtifactLimitBytes     int64  `json:"hot_artifact_limit_bytes"`
	MaxActiveHOTArtifactBytes int64  `json:"max_active_hot_artifact_bytes"`
}

// RQ5LifecycleStep is an observation made by the single slot itself. A start
// is permitted only from active=0 and a stop only from active=1. The boot hash
// changes on every restart, including restore of the same Catalog.
type RQ5LifecycleStep struct {
	Sequence              int    `json:"sequence"`
	Action                string `json:"action"`
	Reason                string `json:"reason"`
	Day                   string `json:"day"`
	CatalogSHA256         string `json:"catalog_sha256"`
	PublicationSHA256     string `json:"publication_sha256"`
	ServiceInstanceSHA256 string `json:"service_instance_sha256"`
	ActiveBefore          int64  `json:"active_before"`
	ActiveAfter           int64  `json:"active_after"`
}

type RQ5RouteEvidence struct {
	SwitchToNewWallMS         float64          `json:"switch_to_new_wall_ms"`
	RetainedCheckWallMS       float64          `json:"retained_check_wall_ms"`
	RestoreNewWallMS          float64          `json:"restore_new_wall_ms"`
	FullRouteWallMS           float64          `json:"full_route_wall_ms"`
	OldPublicationSHA256      string           `json:"old_publication_sha256"`
	NewPublicationSHA256      string           `json:"new_publication_sha256"`
	OldCatalogSHA256          string           `json:"old_catalog_sha256"`
	NewCatalogSHA256          string           `json:"new_catalog_sha256"`
	OldTaskIDHash             string           `json:"old_task_id_hash"`
	NewTaskIDHash             string           `json:"new_task_id_hash"`
	NewRootTaskIDHash         string           `json:"new_root_task_id_hash"`
	ChildTaskIDHash           string           `json:"child_task_id_hash"`
	ChildParentTaskIDHash     string           `json:"child_parent_task_id_hash"`
	ChildRootTaskIDHash       string           `json:"child_root_task_id_hash"`
	OldLedgerBeforeSHA256     string           `json:"old_ledger_before_sha256"`
	OldLedgerAfterSHA256      string           `json:"old_ledger_after_sha256"`
	NewLedgerBeforeRestore    string           `json:"new_ledger_before_restore_sha256"`
	NewLedgerAfterRestore     string           `json:"new_ledger_after_restore_sha256"`
	OldCacheKeySHA256         string           `json:"old_cache_key_sha256"`
	NewCacheKeySHA256         string           `json:"new_cache_key_sha256"`
	CrossReplaySourceSHA256   string           `json:"cross_replay_source_sha256"`
	CrossReplayTargetSHA256   string           `json:"cross_replay_target_sha256"`
	CrossPublicationReplayHit bool             `json:"cross_publication_replay_hit"`
	ChildPublicationSHA256    string           `json:"child_publication_sha256"`
	RootPublicationSHA256     string           `json:"root_publication_sha256"`
	OldInitial                RQ5QueryEvidence `json:"old_initial"`
	NewInitial                RQ5QueryEvidence `json:"new_initial"`
	OldRetained               RQ5QueryEvidence `json:"old_retained"`
	NewRestored               RQ5QueryEvidence `json:"new_restored"`
}

// RQ5QueryEvidence retains no raw task, request, query, result, or object key.
// The verifier manifest is produced only after verifying the signed V8
// receipt, audit inclusions, canonical ciphertext, and released Parquet.
type RQ5QueryEvidence struct {
	Day                     string                    `json:"day"`
	CatalogSHA256           string                    `json:"catalog_sha256"`
	PublicationSHA256       string                    `json:"publication_sha256"`
	TaskIDHash              string                    `json:"task_id_hash"`
	RootTaskIDHash          string                    `json:"root_task_id_hash"`
	RequestIDHash           string                    `json:"request_id_hash"`
	QueryIDHash             string                    `json:"query_id_hash"`
	ResultIDHash            string                    `json:"result_id_hash"`
	ResultSHA256            string                    `json:"result_sha256"`
	RowCount                int64                     `json:"row_count"`
	ColumnCount             int                       `json:"column_count"`
	ClientAvailableMS       float64                   `json:"client_available_ms"`
	ClientFullDrainMS       float64                   `json:"client_full_drain_ms"`
	PipelineMS              map[string]float64        `json:"pipeline_ms"`
	DiagnosticMS            map[string]float64        `json:"diagnostic_ms"`
	PhysicalSQLSHA256       string                    `json:"physical_sql_sha256"`
	LogicalSQLSHA256        string                    `json:"logical_sql_sha256"`
	QueryPlanSHA256         string                    `json:"query_plan_sha256"`
	ActualReleaseFacts      int64                     `json:"actual_release_facts"`
	ChargedReleaseFacts     int64                     `json:"charged_release_facts"`
	ActualDependencyFacts   int64                     `json:"actual_dependency_facts"`
	ChargedDependencyFacts  int64                     `json:"charged_dependency_facts"`
	ActualOutcomeFacts      int64                     `json:"actual_outcome_facts"`
	ChargedOutcomeFacts     int64                     `json:"charged_outcome_facts"`
	PredicateAtomCount      int64                     `json:"predicate_atom_count"`
	CompositeCount          int64                     `json:"composite_count"`
	SemanticReplay          bool                      `json:"semantic_replay"`
	BusinessSQLDelta        int64                     `json:"business_sql_delta"`
	RootEpochBefore         int64                     `json:"root_epoch_before"`
	RootEpochAfter          int64                     `json:"root_epoch_after"`
	RootSetSHA256Before     string                    `json:"root_set_sha256_before"`
	RootSetSHA256After      string                    `json:"root_set_sha256_after"`
	ParquetBytes            int64                     `json:"parquet_bytes"`
	EncryptedObjectBytes    int64                     `json:"encrypted_object_bytes"`
	ReceiptVersion          string                    `json:"receipt_version"`
	ReceiptSHA256           string                    `json:"receipt_sha256"`
	ArtifactIntentSHA256    string                    `json:"artifact_intent_sha256"`
	AvailabilityAuditSHA256 string                    `json:"availability_audit_sha256"`
	ReceiptVerified         bool                      `json:"receipt_verified"`
	ArtifactAvailable       bool                      `json:"artifact_available"`
	VerifierManifest        *RedactedVerifierManifest `json:"verifier_manifest"`
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
	if (sample.SchemaVersion != SampleSchemaVersion && sample.SchemaVersion != TaskGateRejectionSampleSchemaVersion &&
		sample.SchemaVersion != FinalizedSampleSchemaVersion) ||
		sample.CampaignID == "" || sample.DeploymentID == "" || sample.ExperimentID == "" ||
		sample.CellID == "" || sample.SampleID == "" || sample.Iteration < 1 || sample.ProcessReplicate < 1 || sample.OrderPosition < 1 || sample.RandomSeed == 0 ||
		sample.PairID == "" || strings.TrimSpace(sample.PairedSystemOrder) == "" || strings.TrimSpace(sample.RootGroupID) == "" ||
		sample.System == "" || sample.Mode == "" || sample.WorkloadID == "" || sample.Scale == "" ||
		(sample.Status != "pass" && sample.Status != "fail" && sample.Status != "invalid") {
		return errors.New("sample is missing required identity/status fields")
	}
	if sample.TaskGateAcceptanceV3 != nil && sample.TaskGateRejectionV1 != nil {
		return errors.New("sample cannot carry both taskgate_acceptance_v3 and taskgate_rejection_v1")
	}
	switch sample.SchemaVersion {
	case SampleSchemaVersion:
		if sample.TaskGateRejectionV1 != nil {
			return errors.New("sample-v1 cannot carry taskgate_rejection_v1")
		}
		if sample.TaskGateAcceptanceV3 != nil && sample.TaskGateAcceptanceV3.OutcomeCandidateVerification != nil {
			return errors.New("sample-v1 cannot carry Outcome candidate verification")
		}
		if err := sample.validateLegacyEvidenceWire(); err != nil {
			return err
		}
	case TaskGateRejectionSampleSchemaVersion:
		if sample.Status != "fail" || sample.TaskGateRejectionV1 == nil || sample.TaskGateAcceptanceV3 != nil {
			return errors.New("sample-v2 is reserved for a finalizer-rejected FAIL with no acceptance")
		}
		if err := sample.TaskGateRejectionV1.Validate(); err != nil {
			return fmt.Errorf("taskgate_rejection_v1: %w", err)
		}
		if err := sample.validateLegacyEvidenceWire(); err != nil {
			return err
		}
	case FinalizedSampleSchemaVersion:
		if (sample.Status != "pass" && sample.Status != "fail") || sample.TaskGateRejectionV1 != nil ||
			sample.TaskGateAcceptanceV3 == nil {
			return errors.New("sample-v3 is reserved for a post-acceptance PASS or retained evidence FAIL")
		}
		if sample.ExperimentID == "scale" && sample.ScaleVerification == nil {
			return errors.New("sample-v3 Scale sample requires retained Scale evidence")
		}
		if sample.ExperimentID == "artifact" && sample.ArtifactVerification == nil {
			return errors.New("sample-v3 Artifact sample requires retained Artifact evidence")
		}
		if sample.TaskGateAcceptanceV3.OutcomeCandidateVerification != nil &&
			(sample.ExperimentID != "scale" || sample.ScaleVerification == nil ||
				(sample.ScaleVerification.Version != scaleDependencyEvidenceVersionV3 &&
					sample.ScaleVerification.Version != scaleDependencyEvidenceVersionV4 &&
					sample.ScaleVerification.Version != scaleDependencyEvidenceVersionV5)) {
			return errors.New("Outcome candidate verification is reserved for sample-v3 Scale evidence-v3/v4")
		}
		if evidence := sample.ScaleVerification; evidence != nil {
			if (evidence.Version != scaleDependencyEvidenceVersionV2 &&
				evidence.Version != scaleDependencyEvidenceVersionV3 &&
				evidence.Version != scaleDependencyEvidenceVersionV4 &&
				evidence.Version != scaleDependencyEvidenceVersionV5) ||
				evidence.HistoryDependencySHA256 != "" {
				return errors.New("sample-v3 Scale evidence must use Decision-18 evidence-v2/v3 without legacy history")
			}
			if evidence.Version == scaleDependencyEvidenceVersionV2 &&
				(evidence.hasOutcomeCandidateEvidenceV3() ||
					sample.TaskGateAcceptanceV3.OutcomeCandidateVerification != nil) {
				return errors.New("sample-v3 Scale evidence-v2 cannot carry Outcome candidate evidence-v3 members")
			}
			if (evidence.Version == scaleDependencyEvidenceVersionV3 ||
				evidence.Version == scaleDependencyEvidenceVersionV4 ||
				evidence.Version == scaleDependencyEvidenceVersionV5) &&
				(sample.TaskGateAcceptanceV3.OutcomeCandidateVerification == nil ||
					evidence.ExpectedOutcomeMemberCardinality == 0 ||
					evidence.ObservedOutcomeMemberCardinality == 0 ||
					evidence.ExpectedOutcomeCandidateSetSHA256 == "" ||
					evidence.ObservedOutcomeCandidateSetSHA256 == "") {
				return errors.New("sample-v3 Scale evidence-v3/v4 requires complete finalizer Outcome candidate verification")
			}
		}
		if sample.ArtifactVerification != nil && sample.ArtifactVerification.Version != artifactEvidenceVersionV2 {
			return errors.New("sample-v3 Artifact evidence must use evidence-v2")
		}
	}
	if sample.Status == "pass" && sample.TaskGateRejectionV1 != nil {
		return errors.New("passing sample cannot carry taskgate_rejection_v1")
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
	if sample.Status == "pass" && sample.PipelineMS["server_total"]+0.001 < sum {
		return errors.New("server_total is smaller than the non-overlapping pipeline phase sum")
	}
	if sample.ClientAvailableMS < 0 || sample.ClientFullDrainMS < 0 || sample.RowCount < 0 || sample.ColumnCount < 0 ||
		sample.ActualReleaseFacts < 0 || sample.ActualDependencyFacts < 0 || sample.ActualOutcomeFacts < 0 ||
		sample.ChargedReleaseFacts < 0 || sample.ChargedDependencyFacts < 0 || sample.ChargedOutcomeFacts < 0 {
		return errors.New("sample contains invalid measurement values or FactSet cardinalities")
	}
	// An overcharge is a semantic failure worth retaining with its exact
	// observed cardinalities. Only a completed passing operation may assert the
	// charged-subset relationship here; adapter/finalizer gates diagnose failed
	// operations without the runner replacing their evidence.
	if sample.Status == "pass" && (sample.ChargedReleaseFacts > sample.ActualReleaseFacts ||
		sample.ChargedDependencyFacts > sample.ActualDependencyFacts || sample.ChargedOutcomeFacts > sample.ActualOutcomeFacts) {
		return errors.New("sample charged FactSet exceeds its actual FactSet")
	}
	// Replay markers and the zero-Business-SQL invariant describe a completed
	// operation. A failed or invalid operation may stop before setting either
	// marker, or may retain the very non-zero delta that explains its failure.
	// Requiring success semantics here would also make the runner's replacement
	// invalid sample unwritable, erasing that requested operation from JSONL.
	if sample.Status == "pass" {
		if sample.SemanticReplay && sample.BusinessSQLDelta != 0 {
			return errors.New("semantic replay executed Business PostgreSQL")
		}
		if sample.Mode == "semantic_replay" && !sample.SemanticReplay {
			return errors.New("semantic replay mode omitted its replay marker")
		}
		if sample.Mode == "idempotent_replay" && !sample.IdempotentReplay {
			return errors.New("idempotent replay mode omitted its replay marker")
		}
	}
	if sample.Status == "pass" && sample.ResultSHA256 != "" && !validSHA256(sample.ResultSHA256) {
		return errors.New("invalid result SHA-256")
	}
	return nil
}

func (sample Sample) validateLegacyEvidenceWire() error {
	if evidence := sample.ScaleVerification; evidence != nil {
		if evidence.ExpectedExistingFacts != 0 || evidence.ExpectedUnionFacts != 0 ||
			evidence.ExistingDependencySHA256 != "" || evidence.UnionDependencySHA256 != "" ||
			evidence.hasOutcomeCandidateEvidenceV3() {
			return errors.New("sample-v1/v2 cannot carry sample-v3 Decision-18 Scale members")
		}
		if sample.Status == "pass" && evidence.Version != scaleEvidenceVersion {
			return errors.New("passing sample-v1 Scale evidence must use evidence-v1")
		}
	}
	if sample.Status == "pass" && sample.ArtifactVerification != nil &&
		sample.ArtifactVerification.Version != artifactEvidenceVersionV1 {
		return errors.New("passing sample-v1 Artifact evidence must use evidence-v1")
	}
	return nil
}

func (evidence ScaleVerificationEvidence) hasOutcomeCandidateEvidenceV3() bool {
	return evidence.ExpectedOutcomeMemberCardinality != 0 || evidence.ObservedOutcomeMemberCardinality != 0 ||
		evidence.ExpectedOutcomeCandidateSetSHA256 != "" || evidence.ObservedOutcomeCandidateSetSHA256 != ""
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
