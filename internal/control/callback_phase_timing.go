package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"
)

const CallbackPhaseTimingV1Record = "taskgate-control-submitted-callback-phase-timing-v1"

const (
	callbackPhaseClaim          = "callback_claim"
	callbackPhaseTaskRowLock    = "task_row_lock"
	callbackPhaseAuditChainHead = "audit_chain_head"
	callbackPhaseCommit         = "commit"
)

var callbackPhaseOrder = [...]string{
	callbackPhaseClaim,
	callbackPhaseTaskRowLock,
	callbackPhaseAuditChainHead,
	callbackPhaseCommit,
}

type callbackPhaseTimingRecorder struct {
	mu     sync.Mutex
	writer io.Writer
}

type callbackPhaseTimingContextKey struct{}

type callbackPhaseTrace struct {
	recorder *callbackPhaseTimingRecorder
	started  time.Time
	taskID   string
	eventID  string
	phases   map[string]CallbackPhaseTimingPhaseV1
}

// CallbackPhaseTimingV1 is diagnostic-only telemetry for one submitted OA
// callback transaction. Offsets and durations are computed from time.Time's
// monotonic component; wall time is present only to align this record with the
// retained evaluation logs.
type CallbackPhaseTimingV1 struct {
	SchemaVersion int                          `json:"schema_version"`
	Record        string                       `json:"record"`
	TaskID        string                       `json:"task_id"`
	TaskIDSHA256  string                       `json:"task_id_sha256"`
	EventID       string                       `json:"event_id"`
	ObservedAt    string                       `json:"observed_at"`
	Phases        []CallbackPhaseTimingPhaseV1 `json:"phases"`
	FinalResult   string                       `json:"final_result"`
	ErrorClass    string                       `json:"error_class"`
}

type CallbackPhaseTimingPhaseV1 struct {
	Name             string  `json:"name"`
	Attempted        bool    `json:"attempted"`
	StartedOffsetMS  float64 `json:"started_offset_ms"`
	FinishedOffsetMS float64 `json:"finished_offset_ms"`
	DurationMS       float64 `json:"duration_ms"`
	Result           string  `json:"result"`
}

// CallbackPhaseTaskIDSHA256 is shared with the evaluation-only diagnostic
// adapter so a private task ID can be joined to its operation/order record
// without making production behavior depend on evaluation metadata.
func CallbackPhaseTaskIDSHA256(taskID string) string {
	digest := sha256.Sum256([]byte("TASKGATE-CALLBACK-PHASE-TIMING-V1\x00" + taskID))
	return hex.EncodeToString(digest[:])
}

func (s *Store) newCallbackPhaseTrace(taskID, eventID string) *callbackPhaseTrace {
	if s == nil || s.callbackPhaseTiming == nil {
		return nil
	}
	return &callbackPhaseTrace{
		recorder: s.callbackPhaseTiming, started: time.Now(), taskID: taskID, eventID: eventID,
		phases: make(map[string]CallbackPhaseTimingPhaseV1, len(callbackPhaseOrder)),
	}
}

func withCallbackPhaseTrace(ctx context.Context, trace *callbackPhaseTrace) context.Context {
	if trace == nil {
		return ctx
	}
	return context.WithValue(ctx, callbackPhaseTimingContextKey{}, trace)
}

func callbackPhaseTraceFromContext(ctx context.Context) *callbackPhaseTrace {
	trace, _ := ctx.Value(callbackPhaseTimingContextKey{}).(*callbackPhaseTrace)
	return trace
}

func (trace *callbackPhaseTrace) begin(name string) func(error) {
	if trace == nil {
		return func(error) {}
	}
	started := time.Now()
	return func(err error) {
		finished := time.Now()
		result := "ok"
		if err != nil {
			result = "error"
		}
		trace.phases[name] = CallbackPhaseTimingPhaseV1{
			Name: name, Attempted: true,
			StartedOffsetMS:  callbackDurationMS(started.Sub(trace.started)),
			FinishedOffsetMS: callbackDurationMS(finished.Sub(trace.started)),
			DurationMS:       callbackDurationMS(finished.Sub(started)), Result: result,
		}
	}
}

func (trace *callbackPhaseTrace) finish(err error, replay bool) {
	if trace == nil {
		return
	}
	phases := make([]CallbackPhaseTimingPhaseV1, 0, len(callbackPhaseOrder))
	for _, name := range callbackPhaseOrder {
		phase, present := trace.phases[name]
		if !present {
			phase = CallbackPhaseTimingPhaseV1{Name: name, Result: "not_attempted"}
		}
		phases = append(phases, phase)
	}
	finalResult := "committed"
	if replay {
		finalResult = "replay_committed"
	}
	errorClass := "none"
	if err != nil {
		finalResult = "error"
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			errorClass = "context_deadline_exceeded"
		case errors.Is(err, context.Canceled):
			errorClass = "context_cancelled"
		default:
			errorClass = "other"
		}
	}
	record := CallbackPhaseTimingV1{
		SchemaVersion: 1, Record: CallbackPhaseTimingV1Record,
		TaskID: trace.taskID, TaskIDSHA256: CallbackPhaseTaskIDSHA256(trace.taskID), EventID: trace.eventID,
		ObservedAt: trace.started.UTC().Format(time.RFC3339Nano), Phases: phases,
		FinalResult: finalResult, ErrorClass: errorClass,
	}
	trace.recorder.mu.Lock()
	defer trace.recorder.mu.Unlock()
	// Diagnostic output must never become part of callback success semantics.
	_ = json.NewEncoder(trace.recorder.writer).Encode(record)
}

func callbackDurationMS(duration time.Duration) float64 {
	return float64(duration.Nanoseconds()) / float64(time.Millisecond)
}
