package control

import (
	"encoding/json"
	"time"

	"taskbound.local/agent-data-gateway/internal/auditchain"
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
	ID              string
	PrincipalID     string
	Objective       string
	State           TaskState
	TerminalReason  TerminalReason
	CatalogVersion  string
	Sensitivity     string
	RequestedBudget json.RawMessage
	RequestContext  json.RawMessage
	ApprovalRef     string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	ExpiresAt       *time.Time
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
	ExpiresAt          time.Time
	CatalogVersion     string
	CatalogDigest      string
	DatasourceID       string
	SchemaDigest       string
	ApprovalReceipt    string
	CreatedAt          time.Time
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
	ChargedQueries int64
	ChargedRows    int64
	ChargedDBMS    int64
	BudgetBefore   BudgetSnapshot
	BudgetAfter    *BudgetSnapshot
	ResultSHA256   string
	ErrorCode      string
	CreatedAt      time.Time
	CompletedAt    *time.Time
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
	Replay bool
	Record *QueryRecord
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
}

// BudgetSettlement commits actual resource use. Every completed settlement
// consumes one query; use ReleaseBudget for an execution that should not be
// charged. Rows and DBMS are bounded by the reservation.
type BudgetSettlement struct {
	QueryID   string
	Rows      int64
	DBMS      int64
	ErrorCode string
}

type EncryptedResult struct {
	QueryID    string
	TaskID     string
	KeyID      string
	Nonce      []byte
	Ciphertext []byte
	SHA256     string
	CreatedAt  time.Time
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
	Query   QueryRecord
	Audit   AuditEvent
	Receipt *PersistedQueryReceipt
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
