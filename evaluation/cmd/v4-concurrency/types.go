package main

import (
	"encoding/json"
	"time"
)

const (
	concurrencyConfigSchema = 1
	concurrencyReportSchema = 2
)

type concurrencyConfig struct {
	SchemaVersion     int               `json:"schema_version"`
	Gateway           gatewayConfig     `json:"gateway"`
	ControlDSNEnv     string            `json:"control_dsn_env"`
	RequestTimeoutMS  int               `json:"request_timeout_ms"`
	LockWaitTimeoutMS int               `json:"lock_wait_timeout_ms"`
	Provision         *provisionConfig  `json:"provision,omitempty"`
	Cases             []concurrencyCase `json:"cases"`
}

type gatewayConfig struct {
	URL           string   `json:"url"`
	ContenderURLs []string `json:"contender_urls"`
	TokenEnv      string   `json:"token_env"`
}

type provisionConfig struct {
	OAURL            string              `json:"oa_url"`
	AlicePasswordEnv string              `json:"alice_password_env"`
	BobPasswordEnv   string              `json:"bob_password_env"`
	DataProducts     []string            `json:"data_products"`
	Columns          map[string][]string `json:"columns"`
	Scopes           map[string]any      `json:"scopes"`
}

// Each case uses an independently provisioned root family. PrefixPlan must
// commit the exact B-1 state in BoundaryDimension. All contender tasks execute
// the same ContenderPlan under the same root while its head row is locked. The
// one winning observation advances all three dimensions to AtBudget; every
// other contender must settle with zero novelty after the root lock is
// released. The campaign observes the lock queue but does not infer which
// epoch a request read or whether it retried. OverflowPlan must then attempt a
// genuinely new observation and fail at B+1.
type concurrencyCase struct {
	ID                string          `json:"id"`
	Concurrency       int             `json:"concurrency"`
	BoundaryDimension string          `json:"boundary_dimension"`
	RootTaskID        string          `json:"root_task_id"`
	PrefixTaskID      string          `json:"prefix_task_id"`
	ContenderTaskIDs  []string        `json:"contender_task_ids"`
	OverflowTaskID    string          `json:"overflow_task_id"`
	PrefixPlan        json.RawMessage `json:"prefix_plan"`
	ContenderPlan     json.RawMessage `json:"contender_plan"`
	OverflowPlan      json.RawMessage `json:"overflow_plan"`
	BeforeUsed        exposureCounts  `json:"before_used"`
	AtBudget          exposureCounts  `json:"at_budget"`
}

type exposureCounts struct {
	Release   int64 `json:"release"`
	Influence int64 `json:"influence"`
	Outcome   int64 `json:"outcome"`
}

func (value exposureCounts) add(other exposureCounts) exposureCounts {
	return exposureCounts{Release: value.Release + other.Release,
		Influence: value.Influence + other.Influence, Outcome: value.Outcome + other.Outcome}
}

func (value exposureCounts) subtract(other exposureCounts) exposureCounts {
	return exposureCounts{Release: value.Release - other.Release,
		Influence: value.Influence - other.Influence, Outcome: value.Outcome - other.Outcome}
}

func (value exposureCounts) zero() bool {
	return value == (exposureCounts{})
}

func (value exposureCounts) dimension(name string) int64 {
	switch name {
	case "release":
		return value.Release
	case "influence":
		return value.Influence
	case "outcome":
		return value.Outcome
	default:
		return -1
	}
}

type concurrencyReport struct {
	SchemaVersion int               `json:"schema_version"`
	Status        string            `json:"status"`
	Acceptance    string            `json:"acceptance"`
	StartedAt     time.Time         `json:"started_at"`
	FinishedAt    time.Time         `json:"finished_at"`
	Configuration reportConfig      `json:"configuration"`
	Provenance    reportProvenance  `json:"provenance"`
	MetricNotes   map[string]string `json:"metric_notes"`
	Cells         []concurrencyCell `json:"cells"`
	Gates         []reportGate      `json:"gates"`
	Errors        []string          `json:"errors,omitempty"`
}

type reportConfig struct {
	GatewayURL            string   `json:"gateway_url"`
	ContenderGatewayURLs  []string `json:"contender_gateway_urls"`
	ContenderGatewayCount int      `json:"contender_gateway_count"`
	PerGatewayControlPool int      `json:"per_gateway_control_pool"`
	RequestTimeoutMS      int      `json:"request_timeout_ms"`
	LockWaitTimeoutMS     int      `json:"lock_wait_timeout_ms"`
	CaseCount             int      `json:"case_count"`
	ConcurrencyLevels     []int    `json:"concurrency_levels"`
	BoundaryDimensions    []string `json:"boundary_dimensions"`
}

type reportProvenance struct {
	ConfigSHA256 string `json:"config_sha256"`
	SourceSHA256 string `json:"source_sha256"`
}

type concurrencyCell struct {
	CaseID                string             `json:"case_id"`
	Concurrency           int                `json:"concurrency"`
	BoundaryDimension     string             `json:"boundary_dimension"`
	RootTaskSHA256        string             `json:"root_task_sha256"`
	FamilyTaskSHA256      []string           `json:"family_task_sha256"`
	Status                string             `json:"status"`
	Error                 string             `json:"error,omitempty"`
	Initial               rootHeadEvidence   `json:"initial"`
	BeforeBoundary        rootHeadEvidence   `json:"before_boundary"`
	AtBoundary            rootHeadEvidence   `json:"at_boundary"`
	AfterRejectedOverflow rootHeadEvidence   `json:"after_rejected_overflow"`
	Prefix                prefixEvidence     `json:"prefix"`
	Contention            contentionEvidence `json:"contention"`
	Overflow              overflowEvidence   `json:"overflow"`
	Checks                cellChecks         `json:"checks"`
}

type rootHeadEvidence struct {
	Epoch              int64          `json:"epoch"`
	Limits             exposureCounts `json:"limits"`
	Used               exposureCounts `json:"used"`
	ReleaseSetSHA256   string         `json:"release_set_sha256,omitempty"`
	InfluenceSetSHA256 string         `json:"influence_set_sha256,omitempty"`
	OutcomeSetSHA256   string         `json:"outcome_set_sha256,omitempty"`
}

type prefixEvidence struct {
	Status         string         `json:"status"`
	LatencyMS      float64        `json:"latency_ms,omitempty"`
	ObservationSHA string         `json:"observation_sha256,omitempty"`
	Actual         exposureCounts `json:"actual"`
	Charged        exposureCounts `json:"charged"`
	RootEpoch      int64          `json:"root_epoch,omitempty"`
	ResultSHA256   string         `json:"result_sha256,omitempty"`
}

type contentionEvidence struct {
	Status                  string         `json:"status"`
	RootLockWaitersObserved int            `json:"root_lock_waiters_observed"`
	SuccessfulRequests      int            `json:"successful_requests"`
	FailedRequests          int            `json:"failed_requests"`
	ChargedWinners          int            `json:"charged_winners"`
	ZeroNoveltySettlements  int            `json:"zero_novelty_settlements"`
	TotalCharged            exposureCounts `json:"total_charged"`
	RootEpochs              []int64        `json:"root_epochs"`
	ObservationSHA256       []string       `json:"observation_sha256"`
	ResultSHA256            []string       `json:"result_sha256"`
	ClientLatencyMS         []float64      `json:"client_latency_ms"`
}

type overflowEvidence struct {
	Status                    string        `json:"status"`
	ExpectedErrorCode         string        `json:"expected_error_code"`
	ObservedErrorCode         string        `json:"observed_error_code,omitempty"`
	LatencyMS                 float64       `json:"latency_ms,omitempty"`
	QueryStatus               string        `json:"query_status,omitempty"`
	ExposureReservationStatus string        `json:"exposure_reservation_status,omitempty"`
	QueryResultSHA256         string        `json:"query_result_sha256,omitempty"`
	EncryptedResults          int64         `json:"encrypted_results"`
	EncryptedResultChunks     int64         `json:"encrypted_result_chunks"`
	Materializations          int64         `json:"materializations"`
	QueryObservations         int64         `json:"query_observations"`
	RootObservations          int64         `json:"root_observations"`
	TerminalSuccessAudits     int64         `json:"terminal_success_audits"`
	TerminalFailureAudits     int64         `json:"terminal_failure_audits"`
	Receipts                  int64         `json:"receipts"`
	ContentBefore             contentCounts `json:"content_before"`
	ContentAfter              contentCounts `json:"content_after"`
}

type contentCounts struct {
	Containers   int64 `json:"containers"`
	Sets         int64 `json:"sets"`
	DynamicFacts int64 `json:"dynamic_facts"`
	Observations int64 `json:"observations"`
}

type cellChecks struct {
	SharedRootFamily           bool `json:"shared_root_family"`
	FreshRoot                  bool `json:"fresh_root"`
	BMinusOneCommitted         bool `json:"b_minus_one_committed"`
	BCommitted                 bool `json:"b_committed"`
	ThreeDimensionalAtomic     bool `json:"three_dimensional_atomic"`
	RootLockQueueObserved      bool `json:"root_lock_queue_observed"`
	OverflowRejected           bool `json:"overflow_rejected"`
	FailureLeftNoPartialCommit bool `json:"failure_left_no_partial_commit"`
}

type reportGate struct {
	ID          string `json:"id"`
	Requirement string `json:"requirement"`
	Status      string `json:"status"`
	Evidence    any    `json:"evidence,omitempty"`
	Reason      string `json:"reason,omitempty"`
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
	DictionarySetDigest   string `json:"dictionary_set_digest"`
	ReleaseSetSHA256      string `json:"release_set_sha256"`
	InfluenceSetSHA256    string `json:"influence_set_sha256"`
	OutcomeSetSHA256      string `json:"outcome_set_sha256"`
	RootEpoch             int64  `json:"root_epoch"`
}

func (value exposureResult) actual() exposureCounts {
	return exposureCounts{Release: value.ActualReleaseFacts, Influence: value.ActualInfluenceFacts,
		Outcome: value.ActualOutcomeFacts}
}

func (value exposureResult) charged() exposureCounts {
	return exposureCounts{Release: value.ChargedReleaseFacts, Influence: value.ChargedInfluenceFacts,
		Outcome: value.ChargedOutcomeFacts}
}

type executeResponse struct {
	Rows           [][]any        `json:"rows"`
	RowCount       int64          `json:"row_count"`
	SemanticReplay bool           `json:"semantic_replay"`
	Exposure       exposureResult `json:"exposure"`
}
