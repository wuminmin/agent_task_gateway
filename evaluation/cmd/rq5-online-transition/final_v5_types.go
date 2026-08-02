package main

import (
	"encoding/json"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

const finalV5RQ5DriverVersion = "taskgate-final-v5-rq5-sequential-driver-v1"

type finalV5RQ5DriverRequest struct {
	SchemaVersion       int                         `json:"schema_version"`
	DriverVersion       string                      `json:"driver_version"`
	FixtureSHA256       string                      `json:"fixture_sha256"`
	BuildManifestSHA256 string                      `json:"build_manifest_sha256"`
	Operation           experiment.AdapterOperation `json:"operation"`
	CycleIndex          int                         `json:"cycle_index"`
	FromDay             string                      `json:"from_day"`
	ToDay               string                      `json:"to_day"`
	GeneratorSHA256     string                      `json:"generator_sha256"`
	ConfigSHA256        string                      `json:"config_sha256"`
	PhaseImageID        string                      `json:"phase_image_id"`
	OnlineImageID       string                      `json:"online_image_id"`
	OAImageID           string                      `json:"oa_image_id"`
	PhaseBinarySHA256   string                      `json:"phase_binary_sha256"`
	OnlineBinarySHA256  string                      `json:"online_binary_sha256"`
	OABinarySHA256      string                      `json:"oa_binary_sha256"`
	PhaseBinaryMTime    *int64                      `json:"phase_binary_mtime_unix"`
	OnlineBinaryMTime   *int64                      `json:"online_binary_mtime_unix"`
	OABinaryMTime       *int64                      `json:"oa_binary_mtime_unix"`
}

type finalV5RQ5DriverResponse struct {
	SchemaVersion int                                 `json:"schema_version"`
	DriverVersion string                              `json:"driver_version"`
	Status        string                              `json:"status"`
	ErrorCode     string                              `json:"error_code,omitempty"`
	Evidence      *experiment.RQ5VerificationEvidence `json:"evidence,omitempty"`
}

type finalV5PhaseReport struct {
	SchemaVersion    string          `json:"schema_version"`
	Status           string          `json:"status"`
	Phase            string          `json:"phase"`
	Day              string          `json:"day"`
	Sample           int             `json:"sample"`
	Executable       string          `json:"executable"`
	ExecutableSHA256 string          `json:"executable_sha256"`
	ArgvSHA256       string          `json:"argv_sha256"`
	WallMS           float64         `json:"wall_ms"`
	PeakRSSBytes     *uint64         `json:"peak_rss_bytes"`
	PeakRSSScope     string          `json:"peak_rss_scope"`
	ExitCode         int             `json:"exit_code"`
	StdoutBytes      int             `json:"stdout_bytes"`
	StdoutSHA256     string          `json:"stdout_sha256"`
	StderrBytes      int             `json:"stderr_bytes"`
	StderrSHA256     string          `json:"stderr_sha256"`
	CommandReport    json.RawMessage `json:"command_report"`
	Failure          string          `json:"failure,omitempty"`
	Measurement      string          `json:"measurement_boundary"`
}

type finalV5OfflineCommandReport struct {
	SchemaVersion             int                             `json:"schema_version"`
	Mode                      string                          `json:"mode"`
	Publications              []finalV5PublicationMeasurement `json:"publications"`
	TotalArtifactBytes        int64                           `json:"total_artifact_bytes"`
	HotArtifactBytes          int64                           `json:"hot_artifact_bytes"`
	VerificationReceiptSHA256 string                          `json:"verification_receipt_sha256,omitempty"`
}

type finalV5PublicationMeasurement struct {
	PublicationName   string `json:"publication_name"`
	RowCount          uint64 `json:"row_count"`
	ManifestDigest    string `json:"manifest_digest"`
	DictionaryDigest  string `json:"dictionary_digest"`
	SidecarDigest     string `json:"sidecar_digest"`
	ColdPayloadDigest string `json:"cold_payload_digest"`
	HotIndexDigest    string `json:"hot_index_digest"`
	ArtifactBytes     int64  `json:"artifact_bytes"`
	HotArtifactBytes  int64  `json:"hot_artifact_bytes"`
}

type finalV5QueryResponse struct {
	ResultID         string               `json:"result_id"`
	QueryID          string               `json:"query_id"`
	TaskID           string               `json:"task_id"`
	ArtifactStatus   string               `json:"artifact_status"`
	RowCount         int64                `json:"row_count"`
	ColumnCount      int                  `json:"column_count"`
	PipelineMS       map[string]float64   `json:"pipeline_ms"`
	DiagnosticMS     map[string]float64   `json:"diagnostic_ms"`
	PlanDigest       string               `json:"plan_digest"`
	SemanticReplay   bool                 `json:"semantic_replay"`
	IdempotentReplay bool                 `json:"idempotent_replay"`
	Receipt          json.RawMessage      `json:"receipt"`
	Exposure         finalV5QueryExposure `json:"exposure"`
}

type finalV5QueryExposure struct {
	QueryID                   string `json:"query_id"`
	RootTaskID                string `json:"root_task_id"`
	ProfileVersion            string `json:"profile_version"`
	ActualReleaseFacts        int64  `json:"actual_release_facts"`
	ActualInfluenceFacts      int64  `json:"actual_influence_facts"`
	ActualOutcomeFacts        int64  `json:"actual_outcome_facts"`
	ChargedReleaseFacts       int64  `json:"charged_release_facts"`
	ChargedInfluenceFacts     int64  `json:"charged_influence_facts"`
	ChargedOutcomeFacts       int64  `json:"charged_outcome_facts"`
	ActualPredicateAtomCount  int64  `json:"actual_predicate_atom_count"`
	ChargedPredicateAtomCount int64  `json:"charged_predicate_atom_count"`
	ActualCompositeCount      int64  `json:"actual_composite_count"`
	ChargedCompositeCount     int64  `json:"charged_composite_count"`
	ObservationSHA256         string `json:"observation_sha256"`
	DictionarySetDigest       string `json:"dictionary_set_digest"`
	ReleaseSetSHA256          string `json:"release_set_sha256"`
	InfluenceSetSHA256        string `json:"influence_set_sha256"`
	OutcomeSetSHA256          string `json:"outcome_set_sha256"`
	PredicateContextSHA256    string `json:"predicate_context_sha256"`
	PredicateSetSHA256        string `json:"predicate_set_sha256"`
	CompositeOutcomeSHA256    string `json:"composite_outcome_sha256"`
	RootEpoch                 int64  `json:"root_epoch"`
}
