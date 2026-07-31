package control

import (
	"encoding/json"
	"time"

	"taskbound.local/agent-data-gateway/internal/auditchain"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/ordinal"
)

type TaskState string

const (
	TaskAwaitingSubmission TaskState = "AWAITING_SUBMISSION"
	TaskAwaitingApproval   TaskState = "AWAITING_APPROVAL"
	TaskActive             TaskState = "ACTIVE"
	TaskArchived           TaskState = "ARCHIVED"
)

type TerminalReason string

const (
	TerminalCompleted       TerminalReason = "completed"
	TerminalBudgetExhausted TerminalReason = "budget_exhausted"
	TerminalRejected        TerminalReason = "rejected"
	TerminalExpired         TerminalReason = "expired"
	TerminalRevoked         TerminalReason = "revoked"
	TerminalFailed          TerminalReason = "failed"
)

type Principal struct {
	ID         string
	Subject    string
	Role       string
	TokenHash  string
	CreatedAt  time.Time
	DisabledAt *time.Time
}

type Task struct {
	ID             string
	PrincipalID    string
	Objective      string
	State          TaskState
	TerminalReason TerminalReason
	CatalogVersion string
	Sensitivity    string
	RequestContext json.RawMessage
	ApprovalRef    string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ExpiresAt      *time.Time
	RootTaskID     string
	ParentTaskID   string
}

type TaskFilter struct {
	PrincipalID string
	State       TaskState
	Limit       int
	AfterID     string
}

type TaskGrant struct {
	TaskID             string
	Subject            string
	Purpose            string
	ApprovedProducts   []string
	ApprovedColumns    map[string][]string
	MandatoryScope     json.RawMessage
	SensitivityCeiling string
	Budget             BudgetLimits
	Exposure           ExposureGrant
	ExpiresAt          time.Time
	CatalogVersion     string
	CatalogDigest      string
	DatasourceID       string
	SchemaDigest       string
	ApprovalReceipt    string
	CreatedAt          time.Time
}

type ExposureLimits struct {
	ReleaseFacts   int64 `json:"release_facts"`
	InfluenceFacts int64 `json:"influence_facts"`
	OutcomeFacts   int64 `json:"outcome_facts"`
}

type ExposureGrant struct {
	Limits         ExposureLimits `json:"limits"`
	ProfileVersion string         `json:"profile_version"`
}

func (g ExposureGrant) Enabled() bool {
	return g.Limits.ReleaseFacts > 0 || g.Limits.InfluenceFacts > 0 || g.Limits.OutcomeFacts > 0 || g.ProfileVersion != ""
}

type ExposureLedgerSnapshot struct {
	RootTaskID     string         `json:"root_task_id"`
	ProfileVersion string         `json:"profile_version"`
	Limits         ExposureLimits `json:"limits"`
	Used           ExposureLimits `json:"used"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

func (s ExposureLedgerSnapshot) Remaining() ExposureLimits {
	return ExposureLimits{
		ReleaseFacts:   max64(0, s.Limits.ReleaseFacts-s.Used.ReleaseFacts),
		InfluenceFacts: max64(0, s.Limits.InfluenceFacts-s.Used.InfluenceFacts),
		OutcomeFacts:   max64(0, s.Limits.OutcomeFacts-s.Used.OutcomeFacts),
	}
}

type ExposureReservationRequest struct {
	ProfileVersion          string
	EstimatedReleaseFacts   int64
	EstimatedInfluenceFacts int64
	EstimatedOutcomeFacts   int64
}

type ExposureReservation struct {
	QueryID                 string
	TaskID                  string
	RootTaskID              string
	ProfileVersion          string
	EstimatedReleaseFacts   int64
	EstimatedInfluenceFacts int64
	EstimatedOutcomeFacts   int64
}

type ExposureCharge struct {
	QueryID               string `json:"query_id"`
	RootTaskID            string `json:"root_task_id"`
	ProfileVersion        string `json:"profile_version"`
	ActualReleaseFacts    int64  `json:"actual_release_facts"`
	ActualInfluenceFacts  int64  `json:"actual_influence_facts"`
	ActualOutcomeFacts    int64  `json:"actual_outcome_facts"`
	ChargedReleaseFacts   int64  `json:"charged_release_facts"`
	ChargedInfluenceFacts int64  `json:"charged_influence_facts"`
	ChargedOutcomeFacts   int64  `json:"charged_outcome_facts"`
	ObservationSHA256     string `json:"observation_sha256"`
	// V4 fields bind a charge to the immutable dictionary and the one atomic
	// three-dimensional root-head transition. They are empty for V1--V3.
	DictionarySetDigest string `json:"dictionary_set_digest,omitempty"`
	ReleaseSetSHA256    string `json:"release_set_sha256,omitempty"`
	InfluenceSetSHA256  string `json:"influence_set_sha256,omitempty"`
	OutcomeSetSHA256    string `json:"outcome_set_sha256,omitempty"`
	RootEpoch           int64  `json:"root_epoch,omitempty"`
}

// OrdinalDynamicFact is a sparse V4 fact that cannot be assigned by the
// immutable base snapshot compiler (derived Release and Outcome facts). The
// canonical payload is compared byte-for-byte whenever the hash already
// exists, so a hash collision always fails closed.
type OrdinalDynamicFact struct {
	SHA256           string `json:"sha256"`
	Kind             string `json:"kind"`
	CanonicalPayload []byte `json:"canonical_payload"`
}

const (
	OrdinalDynamicDerivedRelease = "DERIVED_RELEASE"
	OrdinalDynamicOutcome        = "OUTCOME"
)

// OrdinalHybridSet combines exact snapshot ordinals with the intentionally
// small sparse dynamic dictionary. BitmapSet is immutable.
type OrdinalHybridSet struct {
	Static       ordinal.BitmapSet    `json:"-"`
	DynamicFacts []OrdinalDynamicFact `json:"dynamic_facts,omitempty"`
}

// OrdinalExposureObservation is the V4 settlement evidence. Digest may be
// omitted; the store computes and persists the canonical digest. If supplied,
// it must match exactly.
type OrdinalExposureObservation struct {
	ProfileVersion      string           `json:"profile_version"`
	DictionarySetDigest string           `json:"dictionary_set_digest"`
	Release             OrdinalHybridSet `json:"release"`
	Influence           OrdinalHybridSet `json:"influence"`
	Outcome             OrdinalHybridSet `json:"outcome"`
	ObservationSHA256   string           `json:"observation_sha256,omitempty"`
}

// OrdinalObservationReference is the distinct-request replay evidence. It is
// accepted only when the same root has already committed the observation; the
// store obtains all counts and set digests from PostgreSQL and never asks the
// caller to resend the bitmap.
type OrdinalObservationReference struct {
	ObservationSHA256   string `json:"observation_sha256"`
	DictionarySetDigest string `json:"dictionary_set_digest"`
}

// OrdinalMaterializationPublish requests atomic publication of a semantic
// replay entry with the successful source query. The remaining binding fields
// are copied from durable query/observation evidence, never trusted from the
// caller.
type OrdinalMaterializationPublish struct {
	CacheKeySHA256 string
	ExpiresAt      *time.Time
}

type OrdinalMaterializationLookup struct {
	CacheKeySHA256      string
	TaskID              string
	GrantDigest         string
	CatalogDigest       string
	DictionarySetDigest string
}

type OrdinalMaterialization struct {
	CacheKeySHA256 string
	TaskID         string
	RootTaskID     string
	SourceQueryID  string
	Observation    OrdinalObservationReference
	GrantDigest    string
	CatalogDigest  string
	ResultSHA256   string
	ResultKeyID    string
	RowCount       int64
	CreatedAt      time.Time
	ExpiresAt      *time.Time
}

type ApprovalEvent struct {
	EventID   string
	TaskID    string
	Actor     string
	Decision  string
	Payload   json.RawMessage
	CreatedAt time.Time
}

type BudgetLimits struct {
	Queries int64 `json:"queries"`
	Rows    int64 `json:"rows"`
	DBMS    int64 `json:"db_ms"`
}

type BudgetUsage struct {
	UsedQueries     int64 `json:"used_queries"`
	UsedRows        int64 `json:"used_rows"`
	UsedDBMS        int64 `json:"used_db_ms"`
	ReservedQueries int64 `json:"reserved_queries"`
	ReservedRows    int64 `json:"reserved_rows"`
	ReservedDBMS    int64 `json:"reserved_db_ms"`
}

type BudgetSnapshot struct {
	TaskID    string       `json:"task_id"`
	Limits    BudgetLimits `json:"limits"`
	Usage     BudgetUsage  `json:"usage"`
	UpdatedAt time.Time    `json:"updated_at"`
}

func (b BudgetSnapshot) Remaining() BudgetLimits {
	return BudgetLimits{
		Queries: max64(0, b.Limits.Queries-b.Usage.UsedQueries-b.Usage.ReservedQueries),
		Rows:    max64(0, b.Limits.Rows-b.Usage.UsedRows-b.Usage.ReservedRows),
		DBMS:    max64(0, b.Limits.DBMS-b.Usage.UsedDBMS-b.Usage.ReservedDBMS),
	}
}

type QueryStatus string

const (
	QueryReserved  QueryStatus = "RESERVED"
	QueryCompleted QueryStatus = "COMPLETED"
	QueryReleased  QueryStatus = "RELEASED"
	QueryFailed    QueryStatus = "FAILED"
	// QueryIndeterminate means the connector may have executed the statement,
	// but the gateway cannot prove its outcome.  The full reservation is
	// charged and the request_id is never executed again automatically.
	QueryIndeterminate QueryStatus = "INDETERMINATE"
	// QueryInterrupted is kept as a source-compatibility alias for callers of
	// the pre-V1 API. New durable records use QueryIndeterminate.
	QueryInterrupted QueryStatus = QueryIndeterminate
)

type QueryRecord struct {
	ID             string
	TaskID         string
	RequestID      string
	Actor          string
	RequestDigest  string
	SQLFingerprint string
	CatalogVersion string
	CatalogDigest  string
	DatasourceID   string
	SchemaDigest   string
	ManifestDigest string
	GrantDigest    string
	PolicyDecision string
	Status         QueryStatus
	ReservedRows   int64
	ReservedDBMS   int64
	ResultRows     int64
	ResultDBMS     int64
	// ResultObservedDBMS is the raw database time reported by the connector for
	// this query, before clamping to the reservation. It may exceed ChargedDBMS,
	// which is the value that was actually debited from the budget ledger.
	ResultObservedDBMS int64
	ChargedQueries     int64
	ChargedRows        int64
	ChargedDBMS        int64
	BudgetBefore       BudgetSnapshot
	BudgetAfter        *BudgetSnapshot
	ResultSHA256       string
	ErrorCode          string
	CreatedAt          time.Time
	CompletedAt        *time.Time
}

type BudgetReservation struct {
	QueryID     string
	TaskID      string
	RequestID   string
	AllowedRows int64
	AllowedDBMS int64
	Before      BudgetSnapshot
	After       BudgetSnapshot
	// Replay is true when (task_id, request_id) already exists. Record is the
	// first durable status and no new budget was reserved.
	Replay   bool
	Record   *QueryRecord
	Exposure *ExposureReservation
}

type ReserveRequest struct {
	QueryID        string
	TaskID         string
	RequestID      string
	Actor          string
	RequestDigest  string
	SQLFingerprint string
	CatalogVersion string
	CatalogDigest  string
	DatasourceID   string
	SchemaDigest   string
	ManifestDigest string
	GrantDigest    string
	PolicyDecision string
	RequestedRows  int64
	RequestedDBMS  int64
	Exposure       *ExposureReservationRequest
}

// BudgetSettlement commits actual resource use. Every completed settlement
// consumes one query; use ReleaseBudget for an execution that should not be
// charged. Rows and DBMS are bounded by the reservation. ObservedDBMS is the
// raw, untruncated database time reported by the connector; DBMS is the value
// actually charged (clamped to the reservation). Recording both keeps the
// ledger invariant (used+reserved <= limit, enforced via DBMS) while preserving
// the observed measurement so accounting quota, observed time, and the physical
// upper bound remain distinguishable.
type BudgetSettlement struct {
	QueryID      string
	Rows         int64
	DBMS         int64
	ObservedDBMS int64
	ErrorCode    string
	Exposure     *exposure.Observation
	// OrdinalExposure is mutually exclusive with Exposure and selects the V4
	// bitmap settlement path.
	OrdinalExposure        *OrdinalExposureObservation
	OrdinalObservationRef  *OrdinalObservationReference
	OrdinalMaterialization *OrdinalMaterializationPublish
}

type EncryptedResult struct {
	QueryID       string
	TaskID        string
	KeyID         string
	Nonce         []byte
	Ciphertext    []byte
	SHA256        string
	StorageFormat string
	PlaintextSize *int64
	ChunkCount    int64
	CreatedAt     time.Time
}

type ResultArtifactStatus string

const (
	ResultArtifactPending   ResultArtifactStatus = "PENDING"
	ResultArtifactAvailable ResultArtifactStatus = "AVAILABLE"
	ResultArtifactDeleting  ResultArtifactStatus = "DELETING"
	ResultArtifactDeleted   ResultArtifactStatus = "DELETED"
)

// ResultArtifact is the Control PostgreSQL record for an encrypted Parquet
// object. It contains no result rows or Parquet bytes.
type ResultArtifact struct {
	ResultID           string
	QueryID            string
	TaskID             string
	KeyID              string
	Format             string
	Encryption         string
	StagingKey         string
	ObjectKey          string
	ObjectETag         string
	ParquetSHA256      string
	ObjectSHA256       string
	ParquetSize        int64
	ObjectSize         int64
	RowCount           int64
	ColumnCount        int
	SchemaJSON         json.RawMessage
	ResultMetadataJSON json.RawMessage
	ACLJSON            json.RawMessage
	Status             ResultArtifactStatus
	CreatedAt          time.Time
	ExpiresAt          *time.Time
	ConsumedAt         *time.Time
	DeletedAt          *time.Time
}

type ResultEncryptionKeyStatus string

const (
	ResultEncryptionKeyActive ResultEncryptionKeyStatus = "ACTIVE"
	ResultEncryptionKeyErased ResultEncryptionKeyStatus = "ERASED"
)

type ResultEncryptionKey struct {
	KeyID     string
	Status    ResultEncryptionKeyStatus
	CreatedAt time.Time
	ErasedAt  *time.Time
	ErasedBy  string
}

type ResultRetentionHold struct {
	TaskID     string
	Reason     string
	CreatedBy  string
	CreatedAt  time.Time
	ReleasedBy string
	ReleasedAt *time.Time
}

type AuditEvent = auditchain.Event

type AuditFilter struct {
	TaskID    string
	QueryID   string
	Actor     string
	EventType string
	After     int64
	Limit     int
}

type AuditCheckpoint = auditchain.Checkpoint

type QueryRecordPage struct {
	Records    []QueryRecord
	NextCursor string
}

type QueryReceipt struct {
	Query    QueryRecord
	Audit    AuditEvent
	Receipt  *PersistedQueryReceipt
	Exposure *ExposureCharge
}

type PersistedQueryReceipt struct {
	QueryID               string
	Version               string
	GatewayKeyID          string
	Signature             string
	SignedAt              time.Time
	TerminalAuditSequence int64
	TerminalAuditHash     string
	ReceiptJSON           []byte
	ReceiptSHA256         string
	CreatedAt             time.Time
}

type SaveQueryReceiptRequest struct {
	QueryID               string
	Version               string
	GatewayKeyID          string
	Signature             string
	SignedAt              time.Time
	TerminalAuditSequence int64
	TerminalAuditHash     string
	ReceiptJSON           []byte
}

// TerminalReceiptBuilder signs the terminal query evidence that was just
// written in the surrounding transaction and returns the persisted receipt row.
type TerminalReceiptBuilder func(QueryReceipt) (SaveQueryReceiptRequest, error)

type CallbackStatus string

const (
	CallbackProcessing CallbackStatus = "PROCESSING"
	CallbackCompleted  CallbackStatus = "COMPLETED"
	CallbackRetryable  CallbackStatus = "RETRYABLE"
)

type CallbackClaim struct {
	EventID  string
	Status   CallbackStatus
	Claimed  bool
	Replay   bool
	Response []byte
}

type RecoveryReport struct {
	InterruptedQueries int
	ExpiredTasks       int
	RetryableCallbacks int
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
