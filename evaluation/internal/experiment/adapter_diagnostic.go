package experiment

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

const PreregisteredConcurrencyMissDiagnosticV1Record = "taskgate-preregistered-concurrency-miss-diagnostic-v1"

const TaskMigrationWaitDiagnosticV1Record = "taskgate-final-v5-task-migration-wait-diagnostic-v1"

// TaskMigrationWaitDiagnosticV1 is an explicitly versioned private diagnostic
// record. It is deliberately outside Sample v1/v2/v3: historical and current
// publication sample contracts remain byte-compatible while a diagnosis can
// still align every task-state wait with its owning sample.
type TaskMigrationWaitDiagnosticV1 struct {
	SchemaVersion int     `json:"schema_version"`
	Record        string  `json:"record"`
	CampaignID    string  `json:"campaign_id"`
	DeploymentID  string  `json:"deployment_id"`
	ExperimentID  string  `json:"experiment_id"`
	CellID        string  `json:"cell_id"`
	SampleID      string  `json:"sample_id"`
	OrderPosition int     `json:"order_position"`
	Warmup        bool    `json:"warmup"`
	TaskOrdinal   int     `json:"task_ordinal"`
	TaskRole      string  `json:"task_role"`
	TaskIDHash    string  `json:"task_id_hash"`
	ExpectedState string  `json:"expected_state"`
	LastState     string  `json:"last_state"`
	Status        string  `json:"status"`
	ElapsedMS     float64 `json:"elapsed_ms"`
	PollCount     int     `json:"poll_count"`
	LastError     string  `json:"last_error"`
	ObservedAt    string  `json:"observed_at"`
}

func NewTaskMigrationWaitDiagnosticV1(operation AdapterOperation, taskOrdinal int, taskRole, taskID,
	expectedState, lastState, status string, elapsed time.Duration, pollCount int, lastError string,
	observedAt time.Time) TaskMigrationWaitDiagnosticV1 {
	digest := sha256.Sum256([]byte("TASKGATE-FINAL-V5-TASK-MIGRATION-DIAGNOSTIC-V1\x00" +
		operation.CampaignID + "\x00" + operation.DeploymentID + "\x00" + operation.SampleID + "\x00" + taskID))
	return TaskMigrationWaitDiagnosticV1{
		SchemaVersion: 1, Record: TaskMigrationWaitDiagnosticV1Record,
		CampaignID: operation.CampaignID, DeploymentID: operation.DeploymentID,
		ExperimentID: operation.ExperimentID, CellID: operation.CellID, SampleID: operation.SampleID,
		OrderPosition: operation.OrderPosition, Warmup: operation.Warmup,
		TaskOrdinal: taskOrdinal, TaskRole: taskRole, TaskIDHash: hex.EncodeToString(digest[:]),
		ExpectedState: expectedState, LastState: lastState, Status: status,
		ElapsedMS: float64(elapsed.Microseconds()) / 1000, PollCount: pollCount,
		LastError: lastError, ObservedAt: observedAt.UTC().Format(time.RFC3339Nano),
	}
}

func (diagnostic TaskMigrationWaitDiagnosticV1) Validate() error {
	validExpected := diagnostic.ExpectedState == "AWAITING_APPROVAL" || diagnostic.ExpectedState == "ACTIVE"
	validStatus := diagnostic.Status == "reached" || diagnostic.Status == "timeout" || diagnostic.Status == "context_cancelled"
	validRole := diagnostic.TaskRole == "root" || diagnostic.TaskRole == "delegated_child"
	_, timestampErr := time.Parse(time.RFC3339Nano, diagnostic.ObservedAt)
	decodedHash, hashErr := hex.DecodeString(diagnostic.TaskIDHash)
	if diagnostic.SchemaVersion != 1 || diagnostic.Record != TaskMigrationWaitDiagnosticV1Record ||
		diagnostic.CampaignID == "" || diagnostic.DeploymentID == "" || diagnostic.ExperimentID == "" ||
		diagnostic.CellID == "" || diagnostic.SampleID == "" || diagnostic.OrderPosition < 1 ||
		diagnostic.TaskOrdinal < 1 || !validRole || hashErr != nil || len(decodedHash) != sha256.Size || !validExpected ||
		diagnostic.LastState == "" || !validStatus || diagnostic.ElapsedMS < 0 || diagnostic.PollCount < 1 ||
		strings.TrimSpace(diagnostic.LastError) == "" || timestampErr != nil {
		return errors.New("invalid task migration wait diagnostic")
	}
	if diagnostic.Status == "reached" && diagnostic.LastState != diagnostic.ExpectedState {
		return errors.New("reached task migration diagnostic does not carry the expected state")
	}
	return nil
}

// PreregisteredConcurrencyMissDiagnosticV1 is a private stderr control
// envelope. It keeps the exact Adapter cause in the credential-gated
// diagnostic channel while allowing the Runner to distinguish one validated
// preregistered miss from an ordinary process failure.
type PreregisteredConcurrencyMissDiagnosticV1 struct {
	SchemaVersion int    `json:"schema_version"`
	Record        string `json:"record"`
	ExperimentID  string `json:"experiment_id"`
	CellID        string `json:"cell_id"`
	SampleID      string `json:"sample_id"`
	Cause         string `json:"cause"`
}

func NewPreregisteredConcurrencyMissDiagnosticV1(sample Sample, cause error) PreregisteredConcurrencyMissDiagnosticV1 {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	return PreregisteredConcurrencyMissDiagnosticV1{
		SchemaVersion: 1,
		Record:        PreregisteredConcurrencyMissDiagnosticV1Record,
		ExperimentID:  sample.ExperimentID,
		CellID:        sample.CellID,
		SampleID:      sample.SampleID,
		Cause:         message,
	}
}

func (diagnostic PreregisteredConcurrencyMissDiagnosticV1) Validate() error {
	if diagnostic.SchemaVersion != 1 || diagnostic.Record != PreregisteredConcurrencyMissDiagnosticV1Record ||
		diagnostic.ExperimentID != "concurrency" || diagnostic.CellID == "" ||
		diagnostic.SampleID == "" || strings.TrimSpace(diagnostic.Cause) == "" {
		return errors.New("invalid preregistered concurrency miss diagnostic")
	}
	return nil
}
