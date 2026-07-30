package main

import (
	"encoding/json"
	"time"
)

const (
	configSchema = 1
	reportSchema = 1
)

type config struct {
	SchemaVersion         int                 `json:"schema_version"`
	Gateway               gatewayConfig       `json:"gateway"`
	BusinessDSNEnv        string              `json:"business_dsn_env"`
	ControlDSNEnv         string              `json:"control_dsn_env"`
	RequestTimeoutMS      int                 `json:"request_timeout_ms"`
	StatementTimeoutMS    int                 `json:"statement_timeout_ms"`
	OverlapTolerancePoint float64             `json:"overlap_tolerance_percentage_points"`
	RequireFreshRoot      *bool               `json:"require_fresh_root,omitempty"`
	Cases                 []workloadCase      `json:"cases"`
	Observer              *observerConfig     `json:"observer,omitempty"`
	IndexBuild            *commandMetric      `json:"index_build,omitempty"`
	Activation            *commandMetric      `json:"activation,omitempty"`
	Artifacts             artifactConfig      `json:"artifacts,omitempty"`
	EnvironmentManifest   *artifactReference  `json:"environment_manifest,omitempty"`
	SmallQueryBaseline    *smallQueryBaseline `json:"small_query_baseline,omitempty"`
}

type gatewayConfig struct {
	URL      string `json:"url"`
	TokenEnv string `json:"token_env"`
}

type workloadCase struct {
	ID                   string            `json:"id"`
	Shape                string            `json:"shape"`
	TargetOverlapPercent float64           `json:"target_overlap_percent"`
	OverlapDimension     string            `json:"overlap_dimension,omitempty"`
	TaskIDs              []string          `json:"task_ids"`
	SetupPlans           []json.RawMessage `json:"setup_plans,omitempty"`
	Plan                 json.RawMessage   `json:"plan"`
	DirectSQL            string            `json:"direct_sql"`
	DirectArgs           []any             `json:"direct_args,omitempty"`
	Expected             expectedResult    `json:"expected,omitempty"`
	SmallQuery           bool              `json:"small_query,omitempty"`
}

type expectedResult struct {
	RowCount       *int64 `json:"row_count,omitempty"`
	ReleaseFacts   *int64 `json:"release_facts,omitempty"`
	InfluenceFacts *int64 `json:"influence_facts,omitempty"`
	OutcomeFacts   *int64 `json:"outcome_facts,omitempty"`
}

type observerConfig struct {
	Argv                    []string `json:"argv"`
	TimeoutMS               int      `json:"timeout_ms,omitempty"`
	Required                bool     `json:"required,omitempty"`
	BusinessSQLCounter      string   `json:"business_sql_counter,omitempty"`
	NetworkRXCounter        string   `json:"network_rx_counter,omitempty"`
	NetworkTXCounter        string   `json:"network_tx_counter,omitempty"`
	RequiredMemoryScope     string   `json:"required_memory_scope,omitempty"`
	GatewayPeakMemoryMetric string   `json:"gateway_peak_memory_metric,omitempty"`
}

type commandMetric struct {
	Argv          []string `json:"argv"`
	Runs          int      `json:"runs,omitempty"`
	TimeoutMS     int      `json:"timeout_ms,omitempty"`
	SingleProcess bool     `json:"single_process,omitempty"`
	WarmVerified  bool     `json:"warm_verified,omitempty"`
	ArtifactPaths []string `json:"artifact_paths,omitempty"`
	HotPaths      []string `json:"hot_paths,omitempty"`
}

type artifactConfig struct {
	TotalPaths []string `json:"total_paths,omitempty"`
	HotPaths   []string `json:"hot_paths,omitempty"`
}

type smallQueryBaseline struct {
	ArtifactPath   string  `json:"artifact_path"`
	ArtifactSHA256 string  `json:"artifact_sha256"`
	P50MS          float64 `json:"p50_ms"`
	ThroughputQPS  float64 `json:"throughput_qps,omitempty"`
}

type artifactReference struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type report struct {
	SchemaVersion   int                 `json:"schema_version"`
	Status          string              `json:"status"`
	Acceptance      string              `json:"acceptance"`
	StartedAt       time.Time           `json:"started_at"`
	FinishedAt      time.Time           `json:"finished_at"`
	Configuration   reportConfig        `json:"configuration"`
	Provenance      provenance          `json:"provenance"`
	MetricSemantics map[string]string   `json:"metric_semantics"`
	IndexBuild      phaseMeasurement    `json:"index_build"`
	Activation      phaseMeasurement    `json:"activation"`
	Artifacts       artifactMeasurement `json:"artifacts"`
	Environment     evidenceArtifact    `json:"environment"`
	Storage         storageMeasurement  `json:"storage"`
	Samples         []sample            `json:"samples"`
	Summaries       []summary           `json:"summaries"`
	Coverage        coverage            `json:"coverage"`
	Gates           []gate              `json:"gates"`
	Errors          []string            `json:"errors,omitempty"`
	Warnings        []string            `json:"warnings,omitempty"`
}

type reportConfig struct {
	GatewayURL                   string    `json:"gateway_url"`
	RequestTimeoutMS             int       `json:"request_timeout_ms"`
	StatementTimeoutMS           int       `json:"statement_timeout_ms"`
	OverlapTolerancePoint        float64   `json:"overlap_tolerance_percentage_points"`
	RequireFreshRoot             bool      `json:"require_fresh_root"`
	CaseCount                    int       `json:"case_count"`
	TrialCount                   int       `json:"trial_count"`
	ConfiguredShapes             []string  `json:"configured_shapes"`
	ConfiguredOverlapPercentages []float64 `json:"configured_overlap_percentages"`
}

type provenance struct {
	ConfigSHA256             string `json:"config_sha256"`
	SourceSHA256             string `json:"source_sha256"`
	ObserverExecutableSHA256 string `json:"observer_executable_sha256,omitempty"`
	EnvironmentSHA256        string `json:"environment_sha256,omitempty"`
}

type evidenceArtifact struct {
	Status string `json:"status"`
	SHA256 string `json:"sha256,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type exposureResult struct {
	ProfileVersion        string `json:"profile_version"`
	ActualReleaseFacts    int64  `json:"actual_release_facts"`
	ActualInfluenceFacts  int64  `json:"actual_influence_facts"`
	ActualOutcomeFacts    int64  `json:"actual_outcome_facts"`
	ChargedReleaseFacts   int64  `json:"charged_release_facts"`
	ChargedInfluenceFacts int64  `json:"charged_influence_facts"`
	ChargedOutcomeFacts   int64  `json:"charged_outcome_facts"`
	ObservationSHA256     string `json:"observation_sha256"`
	DictionarySetDigest   string `json:"dictionary_set_digest,omitempty"`
	ReleaseSetSHA256      string `json:"release_set_sha256,omitempty"`
	InfluenceSetSHA256    string `json:"influence_set_sha256,omitempty"`
	OutcomeSetSHA256      string `json:"outcome_set_sha256,omitempty"`
	RootEpoch             int64  `json:"root_epoch,omitempty"`
}

type executeResponse struct {
	RowCount       int64              `json:"row_count"`
	Rows           [][]any            `json:"rows"`
	DatabaseMS     float64            `json:"database_ms"`
	ComponentMS    map[string]float64 `json:"component_ms"`
	SemanticReplay bool               `json:"semantic_replay"`
	Exposure       exposureResult     `json:"exposure"`
}

type sample struct {
	CaseID                 string             `json:"case_id"`
	Shape                  string             `json:"shape"`
	TargetOverlapPercent   float64            `json:"target_overlap_percent"`
	OverlapDimension       string             `json:"overlap_dimension"`
	Trial                  int                `json:"trial"`
	TaskSHA256             string             `json:"task_sha256"`
	SmallQuery             bool               `json:"small_query,omitempty"`
	Phase                  string             `json:"phase"`
	Status                 string             `json:"status"`
	Error                  string             `json:"error,omitempty"`
	ClientLatencyMS        float64            `json:"client_latency_ms,omitempty"`
	DatabaseMS             float64            `json:"database_ms,omitempty"`
	ComponentMS            map[string]float64 `json:"component_ms,omitempty"`
	RowCount               int64              `json:"row_count,omitempty"`
	ResultSHA256           string             `json:"result_sha256,omitempty"`
	SemanticReplay         bool               `json:"semantic_replay,omitempty"`
	Exposure               *exposureResult    `json:"exposure,omitempty"`
	ObservedOverlapPercent *float64           `json:"observed_overlap_percent,omitempty"`
	WAL                    walMeasurement     `json:"wal"`
	Observer               observerDelta      `json:"observer"`
}

type walMeasurement struct {
	Status        string `json:"status"`
	BusinessBytes *int64 `json:"business_bytes,omitempty"`
	ControlBytes  *int64 `json:"control_bytes,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

type observerSnapshot struct {
	SchemaVersion int              `json:"schema_version"`
	MemoryScope   string           `json:"memory_scope,omitempty"`
	Metrics       map[string]int64 `json:"metrics"`
}

type observerDelta struct {
	Status      string           `json:"status"`
	Before      map[string]int64 `json:"before,omitempty"`
	After       map[string]int64 `json:"after,omitempty"`
	Delta       map[string]int64 `json:"delta,omitempty"`
	MemoryScope string           `json:"memory_scope,omitempty"`
	Reason      string           `json:"reason,omitempty"`
}

type distribution struct {
	Count int     `json:"count"`
	Min   float64 `json:"min"`
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
	P99   float64 `json:"p99"`
	Max   float64 `json:"max"`
	Mean  float64 `json:"mean"`
}

type summary struct {
	Phase           string                  `json:"phase"`
	Shape           string                  `json:"shape,omitempty"`
	Samples         int                     `json:"samples"`
	ClientLatencyMS distribution            `json:"client_latency_ms"`
	DatabaseMS      *distribution           `json:"database_ms,omitempty"`
	ComponentMS     map[string]distribution `json:"component_ms,omitempty"`
}

type coverage struct {
	Overlaps map[string]coverageItem `json:"overlaps"`
	Shapes   map[string]coverageItem `json:"shapes"`
}

type coverageItem struct {
	Status  string    `json:"status"`
	Samples int       `json:"samples"`
	Values  []float64 `json:"observed_values,omitempty"`
	Reason  string    `json:"reason,omitempty"`
}

type gate struct {
	ID          string `json:"id"`
	Requirement string `json:"requirement"`
	Status      string `json:"status"`
	Evidence    any    `json:"evidence,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

type commandRun struct {
	Run              int     `json:"run"`
	Status           string  `json:"status"`
	WallMS           float64 `json:"wall_ms,omitempty"`
	RootPeakRSSBytes *int64  `json:"root_peak_rss_bytes,omitempty"`
	RSSScope         string  `json:"rss_scope,omitempty"`
	ExitCode         *int    `json:"exit_code,omitempty"`
	OutputSHA256     string  `json:"output_sha256,omitempty"`
	ExecutableSHA256 string  `json:"executable_sha256,omitempty"`
	Error            string  `json:"error,omitempty"`
	ArtifactBytes    *int64  `json:"artifact_bytes,omitempty"`
	HotArtifactBytes *int64  `json:"hot_artifact_bytes,omitempty"`
}

type phaseMeasurement struct {
	Status string       `json:"status"`
	Runs   []commandRun `json:"runs,omitempty"`
	Reason string       `json:"reason,omitempty"`
}

type artifactMeasurement struct {
	Status     string `json:"status"`
	TotalBytes *int64 `json:"total_bytes,omitempty"`
	HotBytes   *int64 `json:"hot_bytes,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type relationSize struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
	Class string `json:"class"`
}

type amortizedStorage struct {
	Roots                 int     `json:"roots"`
	FixedBytesPerRoot     float64 `json:"fixed_bytes_per_root"`
	RuntimeBytesPerRoot   float64 `json:"runtime_bytes_per_root"`
	EstimatedBytesPerRoot float64 `json:"estimated_bytes_per_root"`
}

type storageMeasurement struct {
	Status              string             `json:"status"`
	Relations           []relationSize     `json:"relations,omitempty"`
	FixedControlBytes   int64              `json:"fixed_control_bytes,omitempty"`
	RuntimeControlBytes int64              `json:"runtime_control_bytes,omitempty"`
	ArtifactBytes       int64              `json:"artifact_bytes,omitempty"`
	MeasuredRoots       int64              `json:"measured_roots,omitempty"`
	Amortized           []amortizedStorage `json:"amortized_1_10_100_roots,omitempty"`
	Semantics           string             `json:"semantics,omitempty"`
	Reason              string             `json:"reason,omitempty"`
}
